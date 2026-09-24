package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/dtos"
	"github.com/ekilie/ekilied/internals/jobengine"
)

// JobHandler is the callback signature for when a job trigger arrives via WebSocket.
// The handler receives the context and the job ID; it should fetch and execute the
// job via the HTTP claim endpoint.
type JobHandler func(ctx context.Context, jobID uint)

// JobFullHandler is the callback for when a full job payload arrives via WebSocket.
// The agent can start executing immediately without an HTTP claim round-trip.
type JobFullHandler func(ctx context.Context, jobID uint, action string, params map[string]any)

// Structured message types for WebSocket communication.
// Using structs instead of map[string]any reduces heap allocations on every send.

type wsEnvelope struct {
	V       int    `json:"v"`
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

type wsErrorPayload struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type wsContainerListPayload struct {
	Containers []containerInfo `json:"containers"`
}

type wsLogLinePayload struct {
	StreamID  string `json:"stream_id"`
	Container string `json:"container"`
	Stream    string `json:"stream"`
	Line      string `json:"line"`
	TS        string `json:"ts"`
}

// WSClient manages the WebSocket connection to the control plane.
// It handles connection lifecycle (connect, reconnect, disconnect),
// message dispatch, heartbeats, and provides HTTP helper methods
// for job claiming, log streaming, and job completion.
type WSClient struct {
	cfg       *config.Config
	rootCtx   context.Context
	client    *http.Client
	connMu    sync.Mutex
	conn      *websocket.Conn
	connected atomic.Bool
	egress    chan []byte
	egressLow chan []byte
	onJob     JobHandler
	onJobFull JobFullHandler
	docker    dockerService
	streams   *logStreams
}

func ts() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (c *WSClient) setConn(conn *websocket.Conn) {
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
}

func (c *WSClient) getConn() *websocket.Conn {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn
}

// Connected reports whether the WebSocket connection is currently established.
func (c *WSClient) Connected() bool {
	return c.connected.Load()
}

// NewWSClient creates a new WSClient. The onJob callback is invoked when
// a job trigger message is received. If nil, a no-op is used.
func NewWSClient(cfg *config.Config, rootCtx context.Context, onJob JobHandler, onJobFull ...JobFullHandler) *WSClient {
	if onJob == nil {
		onJob = func(ctx context.Context, jobID uint) {}
	}
	c := &WSClient{
		cfg:       cfg,
		rootCtx:   rootCtx,
		client:    &http.Client{Timeout: 30 * time.Second},
		egress:    make(chan []byte, 32),
		egressLow: make(chan []byte, 128),
		onJob:     onJob,
		streams:   newLogStreams(maxConcurrentLogStreams),
	}
	if len(onJobFull) > 0 {
		c.onJobFull = onJobFull[0]
	}
	return c
}

// sendError queues an error message to be sent over the WebSocket egress channel.
func (c *WSClient) sendError(errType, message string) {
	msg, _ := json.Marshal(wsEnvelope{
		V: 1, Type: "error",
		Payload: wsErrorPayload{Error: errType, Message: message},
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
// It sends the registration token, capabilities, and receives a session token,
// WebSocket URL, and poll interval in return.
func (c *WSClient) Register(ctx context.Context, capabilities []dtos.Capability) (sessionToken, agentID string, err error) {
	reqBody, _ := json.Marshal(dtos.RegisterRequest{
		ServerID:     c.cfg.ServerID,
		Token:        c.cfg.RegistrationToken,
		AgentVersion: config.Version,
		Capabilities: capabilities,
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

// Reconnect backoff bounds. The first retry is quick; later failures back off
// exponentially with jitter so a control-plane outage does not turn into a
// reconnect storm across the fleet (and does not trip the server's rate limit).
const (
	reconnectBaseDelay  = 1 * time.Second
	reconnectMaxDelay   = 2 * time.Minute
	reconnectResetAfter = 30 * time.Second
)

// Connect runs the WebSocket connection loop with exponential backoff and
// jitter until the context is cancelled. It blocks.
func (c *WSClient) Connect(ctx context.Context) {
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			log.Printf("[ws] [ts=%s] connect loop exiting (context done)", ts())
			return
		default:
		}

		log.Printf("[ws] [ts=%s] attempting connection to %s", ts(), c.cfg.WsURL)
		startedAt := time.Now()
		err := c.connectOnce(ctx)
		if time.Since(startedAt) >= reconnectResetAfter {
			// The connection was healthy for a while; treat the next failure
			// as fresh instead of inheriting an old backoff.
			attempt = 0
		}
		if err == nil {
			log.Printf("[ws] [ts=%s] connectOnce returned nil (shouldn't happen)", ts())
			continue
		}

		delay := reconnectDelay(attempt)
		attempt++
		log.Printf("[ws] [ts=%s] disconnected: %v, retrying in %s", ts(), err, delay)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// reconnectDelay returns the backoff for the given attempt with jitter: the
// first retry waits 0.5 to 1 second, later retries grow to a 1 to 2 minute
// ceiling.
func reconnectDelay(attempt int) time.Duration {
	delay := reconnectBaseDelay
	for i := 0; i < attempt && delay < reconnectMaxDelay; i++ {
		delay *= 2
	}
	if delay > reconnectMaxDelay {
		delay = reconnectMaxDelay
	}
	half := delay / 2
	return half + rand.N(half+1)
}

// connectOnce dials the WebSocket URL, sets up read/egress/ping goroutines,
// and processes incoming messages until the connection is closed.
//
// Every pump is scoped to a per-connection context and tracked by a
// WaitGroup, so when this function returns no goroutine from this connection
// is still alive. Without that, a leaked egress pump from a dead connection
// would compete with the new connection's pump for the shared egress
// channels and silently steal messages.
func (c *WSClient) connectOnce(ctx context.Context) error {
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	var pumps sync.WaitGroup

	// Auth travels in the Authorization header. The token is never put in the
	// URL query string, so it cannot leak into proxy access logs or journald.
	log.Printf("[ws] [ts=%s] dialing %s", ts(), c.cfg.WsURL)
	conn, _, err := websocket.Dial(connCtx, c.cfg.WsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"User-Agent":    []string{"ekilied/1.0"},
			"Authorization": []string{"Bearer " + c.cfg.SessionToken},
		},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	log.Printf("[ws] [ts=%s] connected", ts())

	c.setConn(conn)
	c.connected.Store(true)

	// Read pump: receives messages from control plane
	readCh := make(chan []byte, 64)
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		defer func() {
			log.Printf("[ws] [ts=%s] read pump exiting", ts())
			close(readCh)
		}()
		for {
			_, msg, err := conn.Read(connCtx)
			if err != nil {
				closeStatus := websocket.CloseStatus(err)
				if closeStatus == -1 {
					log.Printf("[ws] [ts=%s] read error: %v", ts(), err)
				} else {
					log.Printf("[ws] [ts=%s] connection closed (status=%d)", ts(), closeStatus)
				}
				return
			}
			log.Printf("[ws] [ts=%s] recv %d bytes", ts(), len(msg))
			select {
			case readCh <- msg:
			default:
				log.Printf("[ws] [ts=%s] read buffer full (cap=%d), dropping message", ts(), cap(readCh))
			}
		}
	}()

	// Egress pump: sends heartbeats and log messages
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		defer func() { log.Printf("[ws] [ts=%s] egress pump exiting", ts()) }()
		for {
			select {
			case <-connCtx.Done():
				return
			case msg := <-c.egress:
				log.Printf("[ws] [ts=%s] send (high) %d bytes", ts(), len(msg))
				if err := conn.Write(connCtx, websocket.MessageText, msg); err != nil {
					log.Printf("[ws] [ts=%s] write error (high): %v", ts(), err)
					return
				}
			case msg := <-c.egressLow:
				// Low priority: drain any pending high-priority messages first
				for {
					select {
					case high := <-c.egress:
						log.Printf("[ws] [ts=%s] send (high->low drain) %d bytes", ts(), len(high))
						if err := conn.Write(connCtx, websocket.MessageText, high); err != nil {
							log.Printf("[ws] [ts=%s] write error (drain): %v", ts(), err)
							return
						}
					default:
						goto writeLow
					}
				}
			writeLow:
				log.Printf("[ws] [ts=%s] send (low) %d bytes", ts(), len(msg))
				if err := conn.Write(connCtx, websocket.MessageText, msg); err != nil {
					log.Printf("[ws] [ts=%s] write error (low): %v", ts(), err)
					return
				}
			}
		}
	}()

	// Periodic ping to keep the connection alive
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		pingTicker := time.NewTicker(30 * time.Second)
		defer pingTicker.Stop()
		for {
			select {
			case <-pingTicker.C:
				log.Printf("[ws] [ts=%s] sending ping", ts())
				if err := conn.Ping(connCtx); err != nil {
					log.Printf("[ws] [ts=%s] ping error: %v", ts(), err)
					return
				}
				log.Printf("[ws] [ts=%s] ping ok (pong received)", ts())
			case <-connCtx.Done():
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
			log.Printf("[ws] [ts=%s] unmarshal error: %v", ts(), err)
			continue
		}

		t := ts()
		log.Printf("[ws] [ts=%s] recv type=%s payload=%d bytes", t, envelope.Type, len(envelope.Payload))

		switch envelope.Type {
		case "job":
			var job struct {
				JobID uint `json:"job_id"`
			}
			if err := json.Unmarshal(envelope.Payload, &job); err != nil {
				log.Printf("[ws] [ts=%s] job unmarshal error: %v", t, err)
				continue
			}
			log.Printf("[ws] [ts=%s] job trigger: id=%d", t, job.JobID)
			go c.onJob(c.rootCtx, job.JobID)

		case "job_full":
			var job struct {
				JobID  uint           `json:"job_id"`
				Action string         `json:"action"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(envelope.Payload, &job); err != nil {
				log.Printf("[ws] [ts=%s] job_full unmarshal error: %v", t, err)
				continue
			}
			log.Printf("[ws] [ts=%s] job_full: id=%d action=%s params=%+v", t, job.JobID, job.Action, job.Params)
			if c.onJobFull != nil {
				go c.onJobFull(c.rootCtx, job.JobID, job.Action, job.Params)
			} else {
				go c.onJob(c.rootCtx, job.JobID)
			}

		case "token_rotated":
			var payload struct {
				NewToken string `json:"new_token"`
			}
			json.Unmarshal(envelope.Payload, &payload)
			if payload.NewToken != "" {
				c.cfg.SessionToken = payload.NewToken
				// Never log any part of the token; rotated tokens are still
				// live credentials.
				log.Printf("[ws] [ts=%s] token rotated (%d chars)", t, len(payload.NewToken))
			} else {
				log.Printf("[ws] [ts=%s] token_rotated: empty token ignored", t)
			}

		case "job_cancelled":
			log.Printf("[ws] [ts=%s] job_cancelled (handling pending)", t)

		case "list_containers":
			log.Printf("[ws] [ts=%s] list_containers requested", t)
			if c.docker == nil {
				log.Printf("[ws] [ts=%s] docker not available for list_containers", t)
				c.sendError("docker_not_available", "Docker is not installed on this server")
				continue
			}
			containers, err := c.docker.ListContainers(connCtx)
			if err != nil {
				log.Printf("[ws] [ts=%s] list containers error: %v", t, err)
				c.sendError("docker_error", err.Error())
				continue
			}
			infos := make([]containerInfo, 0, len(containers))
			for _, ct := range containers {
				infos = append(infos, containerToInfo(ct))
			}
			resp, err := containerListWSMessage(infos)
			if err != nil {
				log.Printf("[ws] [ts=%s] list_containers: marshal error: %v", t, err)
				c.sendError("marshal_error", err.Error())
				continue
			}
			log.Printf("[ws] [ts=%s] list_containers: found %d, sending response (%d bytes)", t, len(infos), len(resp))
			select {
			case c.egressLow <- resp:
			default:
				log.Printf("[ws] [ts=%s] list_containers: egressLow full, dropping response", t)
			}

		case "log_stream":
			// Runs the Docker follow in its own goroutine; the dispatch loop
			// must never block on it.
			c.startLogStream(connCtx, &pumps, envelope.Payload)

		case "log_stream_stop":
			c.stopLogStream(envelope.Payload)

		default:
			log.Printf("[ws] [ts=%s] unknown message type: %s", t, envelope.Type)
		}
	}

	// Stop every per-connection goroutine and wait for them to exit before
	// returning, so this connection cannot steal messages from the next one.
	// stopAll cancels any Docker log follows that are still running.
	connCancel()
	c.streams.stopAll()
	pumps.Wait()

	c.connected.Store(false)
	c.setConn(nil)
	log.Printf("[ws] [ts=%s] read channel closed, connection ending", ts())
	return fmt.Errorf("connection closed")
}

// ── Heartbeat (prefer WS, fallback HTTP) ─────────────────────────────────

// heartbeatWSMessage builds the WebSocket envelope for a heartbeat. The
// request struct is marshaled once, as part of the envelope, instead of being
// pre-marshaled into a json.RawMessage and marshaled again.
func heartbeatWSMessage(req dtos.HeartbeatRequest) ([]byte, error) {
	return json.Marshal(wsEnvelope{V: 1, Type: "heartbeat", Payload: req})
}

// containerListWSMessage builds the WebSocket envelope for a container
// listing. The payload struct is marshaled once, as part of the envelope.
func containerListWSMessage(infos []containerInfo) ([]byte, error) {
	return json.Marshal(wsEnvelope{
		V: 1, Type: "container_list",
		Payload: wsContainerListPayload{Containers: infos},
	})
}

// SendHeartbeat attempts to send metrics over the WebSocket egress channel.
// If the channel is full, it falls back to an HTTP POST to /agents/heartbeat.
func (c *WSClient) SendHeartbeat(ctx context.Context, agentID, sessionToken string, metrics dtos.HeartbeatMetrics) error {
	t := ts()
	heartbeat := dtos.HeartbeatRequest{
		AgentID:  agentID,
		ServerID: c.cfg.ServerID,
		TS:       time.Now().UTC().Format(time.RFC3339),
		Metrics:  metrics,
	}

	if c.getConn() != nil {
		if msg, err := heartbeatWSMessage(heartbeat); err == nil {
			select {
			case c.egress <- msg:
				log.Printf("[ws] [ts=%s] heartbeat sent via WS cpu=%.1f%% mem=%.1f%%", t, metrics.CPUPercent, metrics.MemoryPercent)
				return nil
			default:
				log.Printf("[ws] [ts=%s] WS egress full, falling back to HTTP heartbeat", t)
			}
		} else {
			log.Printf("[ws] [ts=%s] heartbeat marshal error: %v, falling back to HTTP", t, err)
		}
	} else {
		log.Printf("[ws] [ts=%s] WS not connected, falling back to HTTP heartbeat", t)
	}

	// HTTP fallback. This is the only other marshal of the request, and only
	// one of the two paths ever runs.
	payload, err := json.Marshal(heartbeat)
	if err != nil {
		return fmt.Errorf("marshal heartbeat: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", c.cfg.APIURL+"/agents/heartbeat", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		log.Printf("[ws] [ts=%s] HTTP heartbeat error: %v", t, err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[ws] [ts=%s] HTTP heartbeat failed: status=%d", t, resp.StatusCode)
		return fmt.Errorf("heartbeat HTTP %d", resp.StatusCode)
	}

	var result dtos.HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.PendingJobsCount > 0 {
		log.Printf("[ws] [ts=%s] HTTP heartbeat ok, %d pending job(s)", t, result.PendingJobsCount)
	} else {
		log.Printf("[ws] [ts=%s] HTTP heartbeat ok", t)
	}
	return nil
}

// ── Job HTTP helpers (used by job engine) ────────────────────────────────

// PollJobs fetches all pending jobs from the control plane via GET /agents/jobs.
func (c *WSClient) PollJobs(ctx context.Context) ([]dtos.JobItem, error) {
	t := ts()
	log.Printf("[http] [ts=%s] polling jobs from %s/agents/jobs", t, c.cfg.APIURL)
	req, _ := http.NewRequestWithContext(ctx, "GET", c.cfg.APIURL+"/agents/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		log.Printf("[http] [ts=%s] poll jobs error: %v", t, err)
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []dtos.JobItem `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[http] [ts=%s] poll jobs decode error: %v", t, err)
		return nil, err
	}
	log.Printf("[http] [ts=%s] poll jobs: found %d pending", t, len(result.Data))
	if len(result.Data) > 0 {
		for _, job := range result.Data {
			log.Printf("[http] [ts=%s]   job id=%d action=%s type=%s", t, job.ID, job.Action, job.Type)
		}
	}
	return result.Data, nil
}

// ClaimJob atomically claims and fetches a job via POST /agents/jobs/:id/claim.
// The backend marks the job as accepted and returns full details in one round trip.
func (c *WSClient) ClaimJob(ctx context.Context, jobID uint) (*dtos.JobItem, error) {
	t := ts()
	log.Printf("[http] [ts=%s] claiming job %d", t, jobID)
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/agents/jobs/%d/claim", c.cfg.APIURL, jobID), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		log.Printf("[http] [ts=%s] claim job %d error: %v", t, jobID, err)
		return nil, fmt.Errorf("claim job %d: %w", jobID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		log.Printf("[http] [ts=%s] claim job %d: already claimed (409)", t, jobID)
		return nil, fmt.Errorf("claim job %d: %w", jobID, jobengine.ErrJobAlreadyClaimed)
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("[http] [ts=%s] claim job %d: HTTP %d", t, jobID, resp.StatusCode)
		return nil, fmt.Errorf("claim job %d: HTTP %d", jobID, resp.StatusCode)
	}

	var apiResp struct {
		Success bool          `json:"success"`
		Data    *dtos.JobItem `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		log.Printf("[http] [ts=%s] claim job %d decode error: %v", t, jobID, err)
		return nil, fmt.Errorf("decode job %d: %w", jobID, err)
	}

	if !apiResp.Success || apiResp.Data == nil {
		log.Printf("[http] [ts=%s] claim job %d: no data returned", t, jobID)
		return nil, fmt.Errorf("claim job %d: no data returned", jobID)
	}

	log.Printf("[http] [ts=%s] claimed job %d: action=%s", t, jobID, apiResp.Data.Action)
	return apiResp.Data, nil
}

// StreamLogs sends a batch of log lines for a job via POST /agents/jobs/:id/logs.
func (c *WSClient) StreamLogs(ctx context.Context, jobID uint, lines []dtos.LogLine) error {
	t := ts()
	log.Printf("[http] [ts=%s] streaming %d log lines for job %d", t, len(lines), jobID)
	body, _ := json.Marshal(dtos.StreamLogsRequest{Lines: lines})

	req, _ := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/agents/jobs/%d/logs", c.cfg.APIURL, jobID),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		log.Printf("[http] [ts=%s] stream logs for job %d error: %v", t, jobID, err)
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[http] [ts=%s] stream logs for job %d: HTTP %d", t, jobID, resp.StatusCode)
		return fmt.Errorf("logs HTTP %d", resp.StatusCode)
	}
	log.Printf("[http] [ts=%s] streamed %d logs for job %d", t, len(lines), jobID)
	return nil
}

// CompleteJob marks a job as completed (success or failed) via POST /agents/jobs/:id/complete.
func (c *WSClient) CompleteJob(ctx context.Context, jobID uint, status, errorMsg, step string, result any) error {
	t := ts()
	var resultPreview string
	if result != nil {
		b, _ := json.Marshal(result)
		if len(b) > 200 {
			resultPreview = string(b[:200]) + "..."
		} else {
			resultPreview = string(b)
		}
	}
	log.Printf("[http] [ts=%s] completing job %d: status=%s step=%s result=%s", t, jobID, status, step, resultPreview)
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
		log.Printf("[http] [ts=%s] complete job %d error: %v", t, jobID, err)
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[http] [ts=%s] complete job %d: HTTP %d", t, jobID, resp.StatusCode)
		return fmt.Errorf("complete HTTP %d", resp.StatusCode)
	}
	log.Printf("[http] [ts=%s] completed job %d", t, jobID)
	return nil
}
