package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/dtos"
)

type JobHandler func(ctx context.Context, jobID uint, action string, params json.RawMessage)
type WSClient struct {
	cfg       *config.Config
	client    *http.Client
	conn      *websocket.Conn
	connected atomic.Bool
	egress    chan []byte
	onJob     JobHandler
	docker    *DockerService
}

func (c *WSClient) Connected() bool {
	return c.connected.Load()
}

func NewWSClient(cfg *config.Config, onJob JobHandler) *WSClient {
	if onJob == nil {
		onJob = func(ctx context.Context, jobID uint, action string, params json.RawMessage) {}
	}
	return &WSClient{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		egress: make(chan []byte, 64),
		onJob:  onJob,
	}
}

func (c *WSClient) SetDockerService(docker *DockerService) {
	c.docker = docker
}

// ── Registration (always HTTP) ───────────────────────────────────────────

func (c *WSClient) Register(ctx context.Context) (sessionToken, agentID string, err error) {
	reqBody, _ := json.Marshal(dtos.RegisterRequest{
		ServerID:     c.cfg.ServerID,
		Token:        c.cfg.RegistrationToken,
		AgentVersion: "1.0.0",
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.APIURL+"/agents/register", bytes.NewReader(reqBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		return "", "", fmt.Errorf("registration failed (HTTP %d)", resp.StatusCode)
	}

	var result dtos.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("decode: %w", err)
	}

	if result.WsURL != "" {
		c.cfg.WsURL = result.WsURL
	}
	if result.PollInterval > 0 {
		c.cfg.PollInterval = result.PollInterval
	}

	log.Printf("registered: agent_id=%s", result.AgentID)
	return result.SessionToken, result.AgentID, nil
}

// ── WebSocket connect loop (primary) ─────────────────────────────────────

func (c *WSClient) Connect(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := c.connectOnce(ctx); err != nil {
			log.Printf("ws disconnected: %v — retrying in 5s", err)
			time.Sleep(5 * time.Second)
		}
	}
}

func (c *WSClient) connectOnce(ctx context.Context) error {
	url := c.cfg.WsURL + "?token=" + c.cfg.SessionToken

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"User-Agent": []string{"ekilied/1.0"},
		},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	c.conn = conn
	c.connected.Store(true)
	log.Println("ws connected")

	// Read pump — receives messages from control plane
	readCh := make(chan []byte, 64)
	go func() {
		defer close(readCh)
		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				log.Printf("ws read error: %v", err)
				return
			}
			readCh <- msg
		}
	}()

	// Egress pump — sends heartbeats and log messages
	go func() {
		for msg := range c.egress {
			if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
				log.Printf("ws write error: %v", err)
				return
			}
		}
	}()

	// Periodic ping to keep the connection alive
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go func() {
		pingTicker := time.NewTicker(30 * time.Second)
		defer pingTicker.Stop()
		for {
			select {
			case <-pingTicker.C:
				if err := conn.Ping(pingCtx); err != nil {
					log.Printf("ws ping error: %v", err)
					return
				}
			case <-pingCtx.Done():
				return
			}
		}
	}()

	// Process incoming messages
	for msg := range readCh {
		var envelope struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			log.Printf("ws unmarshal error: %v", err)
			continue
		}

		switch envelope.Type {
		case "job":
			var job struct {
				JobID  uint            `json:"job_id"`
				Action string          `json:"action"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(envelope.Payload, &job); err != nil {
				log.Printf("ws job unmarshal error: %v", err)
				continue
			}
			log.Printf("ws job received: id=%d action=%s", job.JobID, job.Action)
			go c.onJob(ctx, job.JobID, job.Action, job.Params)

		case "token_rotated":
			var payload struct {
				NewToken string `json:"new_token"`
			}
			json.Unmarshal(envelope.Payload, &payload)
			if payload.NewToken != "" {
				c.cfg.SessionToken = payload.NewToken
				log.Println("ws token rotated")
			}

		case "job_cancelled":
			log.Println("ws job cancelled (handling pending)")

		case "list_containers":
			if c.docker == nil {
				log.Println("docker not available for list_containers")
				continue
			}
			containers, err := c.docker.ListContainers(ctx)
			if err != nil {
				log.Printf("list containers error: %v", err)
				continue
			}
			infos := make([]containerInfo, 0, len(containers))
			for _, ct := range containers {
				infos = append(infos, containerToInfo(ct))
			}
			msg, _ := json.Marshal(map[string]any{
				"v": 1, "type": "container_list",
				"payload": map[string]any{
					"containers": infos,
				},
			})
			select {
			case c.egress <- msg:
			default:
			}

		case "log_stream":
			if c.docker == nil {
				log.Println("docker not available for log_stream")
				continue
			}
			var req struct {
				Container string `json:"container"`
				Tail      int    `json:"tail"`
				StreamID  string `json:"stream_id"`
			}
			json.Unmarshal(envelope.Payload, &req)
			if req.Tail == 0 {
				req.Tail = 100
			}

			log.Printf("starting log stream: container=%s stream_id=%s", req.Container, req.StreamID)

			logCh := make(chan string, 64)

			streamCtx, streamCancel := context.WithCancel(ctx)
			go func() {
				for line := range logCh {
					msg, _ := json.Marshal(map[string]any{
						"v": 1, "type": "log_line",
						"payload": map[string]any{
							"stream_id": req.StreamID,
							"container": req.Container,
							"stream":    "stdout",
							"line":      line,
							"ts":        time.Now().UTC().Format(time.RFC3339),
						},
					})
					select {
					case c.egress <- msg:
					default:
					}
				}
			}()

			err := c.docker.StreamLogs(streamCtx, req.Container, req.Tail, logCh)
			if err != nil {
				log.Printf("log stream ended: %v", err)
			}
			close(logCh)
			streamCancel()

		case "log_stream_stop":
			log.Println("log stream stop requested")

		default:
			log.Printf("ws unknown message type: %s", envelope.Type)
		}
	}

	c.connected.Store(false)
	return fmt.Errorf("connection closed")
}

// ── Heartbeat (prefer WS, fallback HTTP) ─────────────────────────────────

func (c *WSClient) SendHeartbeat(ctx context.Context, agentID, sessionToken string, metrics dtos.HeartbeatMetrics) error {
	payload, _ := json.Marshal(dtos.HeartbeatRequest{
		AgentID:  agentID,
		ServerID: c.cfg.ServerID,
		TS:       time.Now().UTC().Format(time.RFC3339),
		Metrics:  metrics,
	})

	if c.conn != nil {
		msg, _ := json.Marshal(map[string]any{
			"v": 1, "type": "heartbeat", "payload": json.RawMessage(payload),
		})
		select {
		case c.egress <- msg:
			return nil
		default:
			log.Println("ws egress full, falling back to http heartbeat")
		}
	}

	// HTTP fallback
	req, _ := http.NewRequestWithContext(ctx, "POST", c.cfg.APIURL+"/agents/heartbeat", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("heartbeat HTTP %d", resp.StatusCode)
	}

	var result dtos.HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.PendingJobsCount > 0 {
		log.Printf("%d pending job(s)", result.PendingJobsCount)
	}
	return nil
}

// ── Job HTTP helpers (used by job engine as WS fallback) ─────────────────

func (c *WSClient) PollJobs(ctx context.Context) ([]dtos.JobItem, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.cfg.APIURL+"/agents/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []dtos.JobItem `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *WSClient) AcceptJob(ctx context.Context, jobID uint) error {
	req, _ := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/agents/jobs/%d/accept", c.cfg.APIURL, jobID), nil)
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("accept HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *WSClient) StreamLogs(ctx context.Context, jobID uint, lines []dtos.LogLine) error {
	body, _ := json.Marshal(dtos.StreamLogsRequest{Lines: lines})

	req, _ := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/agents/jobs/%d/logs", c.cfg.APIURL, jobID),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("logs HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *WSClient) CompleteJob(ctx context.Context, jobID uint, status, errorMsg, step string, result any) error {
	body, _ := json.Marshal(dtos.CompleteJobRequest{
		Status: status, Error: errorMsg, Step: step, Result: result,
	})

	req, _ := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/agents/jobs/%d/complete", c.cfg.APIURL, jobID),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("complete HTTP %d", resp.StatusCode)
	}
	return nil
}
