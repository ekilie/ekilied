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

// JobHandler is the callback signature for when a job trigger arrives via WebSocket.
// The handler receives the context and the job ID; it should fetch and execute the
// job via the HTTP claim endpoint.
type JobHandler func(ctx context.Context, jobID uint)

// WSClient manages the WebSocket connection to the control plane.
// It handles connection lifecycle (connect, reconnect, disconnect),
// message dispatch, heartbeats, and provides HTTP helper methods
// for job claiming, log streaming, and job completion.
type WSClient struct {
	cfg       *config.Config
	client    *http.Client
	conn      *websocket.Conn
	connected atomic.Bool
	egress    chan []byte
	onJob     JobHandler
	docker    *DockerService
}

// Connected reports whether the WebSocket connection is currently established.
func (c *WSClient) Connected() bool {
	return c.connected.Load()
}

// NewWSClient creates a new WSClient. The onJob callback is invoked when
// a job trigger message is received. If nil, a no-op is used.
func NewWSClient(cfg *config.Config, onJob JobHandler) *WSClient {
	if onJob == nil {
		onJob = func(ctx context.Context, jobID uint) {}
	}
	return &WSClient{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		egress: make(chan []byte, 64),
		onJob:  onJob,
	}
}

// sendError queues an error message to be sent over the WebSocket egress channel.
func (c *WSClient) sendError(errType, message string) {
	msg, _ := json.Marshal(map[string]any{
		"v": 1, "type": "error",
		"payload": map[string]any{
			"error":   errType,
			"message": message,
		},
	})
	select {
	case c.egress <- msg:
	default:
	}
}

// SetDockerService attaches a DockerService for handling container-related
// WebSocket messages (list_containers, log_stream).
func (c *WSClient) SetDockerService(docker *DockerService) {
	c.docker = docker
}

// ── Registration (always HTTP) ───────────────────────────────────────────

// Register performs the one-time registration handshake with the control plane.
// It sends the registration token and receives a session token, WebSocket URL,
// and poll interval in return.
func (c *WSClient) Register(ctx context.Context) (sessionToken, agentID string, err error) {
	reqBody, _ := json.Marshal(dtos.RegisterRequest{
		ServerID:     c.cfg.ServerID,
		Token:        c.cfg.RegistrationToken,
		AgentVersion: config.Version,
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

	// API wraps response in {"success":true,"data":{...}}
	var apiResp struct {
		Success bool                  `json:"success"`
		Data    dtos.RegisterResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", "", fmt.Errorf("decode: %w", err)
	}
	result := apiResp.Data

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

// Connect runs the WebSocket connection loop. It attempts to connect and,
// if the connection drops, waits 5 seconds before retrying. It blocks
// until the context is cancelled.
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

// connectOnce dials the WebSocket URL, sets up read/egress/ping goroutines,
// and processes incoming messages until the connection is closed.
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
			// Lightweight job trigger — agent fetches full details via HTTP.
			var job struct {
				JobID uint `json:"job_id"`
			}
			if err := json.Unmarshal(envelope.Payload, &job); err != nil {
				log.Printf("ws job unmarshal error: %v", err)
				continue
			}
			log.Printf("ws job trigger received: id=%d", job.JobID)
			go c.onJob(ctx, job.JobID)

		case "token_rotated":
			// Session token rotation from control plane.
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
			// List Docker containers on this server and send back via egress.
			if c.docker == nil {
				log.Println("docker not available for list_containers")
				c.sendError("docker_not_available", "Docker is not installed on this server")
				continue
			}
			containers, err := c.docker.ListContainers(ctx)
			if err != nil {
				log.Printf("list containers error: %v", err)
				c.sendError("docker_error", err.Error())
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
			// Start streaming logs from a Docker container to the control plane.
			if c.docker == nil {
				log.Println("docker not available for log_stream")
				c.sendError("docker_not_available", "Docker is not installed on this server")
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

// SendHeartbeat attempts to send metrics over the WebSocket egress channel.
// If the channel is full, it falls back to an HTTP POST to /agents/heartbeat.
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

// ── Job HTTP helpers (used by job engine) ────────────────────────────────

// PollJobs fetches all pending jobs from the control plane via GET /agents/jobs.
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

// ClaimJob atomically claims and fetches a job via POST /agents/jobs/:id/claim.
// The backend marks the job as accepted and returns full details in one round trip.
func (c *WSClient) ClaimJob(ctx context.Context, jobID uint) (*dtos.JobItem, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/agents/jobs/%d/claim", c.cfg.APIURL, jobID), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claim job %d: %w", jobID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return nil, fmt.Errorf("claim job %d: already claimed", jobID)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claim job %d: HTTP %d", jobID, resp.StatusCode)
	}

	var apiResp struct {
		Success bool          `json:"success"`
		Data    *dtos.JobItem `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode job %d: %w", jobID, err)
	}

	if !apiResp.Success || apiResp.Data == nil {
		return nil, fmt.Errorf("claim job %d: no data returned", jobID)
	}

	return apiResp.Data, nil
}

// AcceptJob marks a job as accepted via POST /agents/jobs/:id/accept.
// Deprecated: Use ClaimJob instead, which atomically claims and fetches job details.
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

// StreamLogs sends a batch of log lines for a job via POST /agents/jobs/:id/logs.
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

// CompleteJob marks a job as completed (success or failed) via POST /agents/jobs/:id/complete.
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
