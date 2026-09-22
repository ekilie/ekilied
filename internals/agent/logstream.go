package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Limits that protect the agent and the control-plane link from runaway
// Docker log streams. egressLow is a 128-slot channel shared by every stream,
// so one noisy container must not be able to flood it.
const (
	maxConcurrentLogStreams = 5
	maxLogTail              = 2000
	maxLogLineBytes         = 16 * 1024
	defaultLogTail          = 100
)

// logStreamRequest is the payload of a log_stream WS message.
type logStreamRequest struct {
	Container string `json:"container"`
	Tail      int    `json:"tail"`
	StreamID  string `json:"stream_id"`
}

// logStreams tracks the active Docker log streams of a WS connection.
type logStreams struct {
	mu     sync.Mutex
	cancel map[string]context.CancelFunc
	max    int
}

func newLogStreams(max int) *logStreams {
	return &logStreams{cancel: make(map[string]context.CancelFunc), max: max}
}

// add registers a stream. It returns a human-readable reason when rejected.
func (s *logStreams) add(streamID string, cancel context.CancelFunc) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.cancel[streamID]; exists {
		return false, "stream_id already active"
	}
	if len(s.cancel) >= s.max {
		return false, fmt.Sprintf("too many concurrent log streams (max %d)", s.max)
	}
	s.cancel[streamID] = cancel
	return true, ""
}

// remove stops tracking a stream without cancelling it. Used by the worker
// when the Docker follow ends on its own.
func (s *logStreams) remove(streamID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancel, streamID)
}

// stop cancels and removes a stream. It reports whether the stream existed.
func (s *logStreams) stop(streamID string) bool {
	s.mu.Lock()
	cancel, ok := s.cancel[streamID]
	if ok {
		delete(s.cancel, streamID)
	}
	s.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// stopAll cancels every tracked stream. Called when the WS connection ends so
// no Docker follow survives into the next connection.
func (s *logStreams) stopAll() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.cancel))
	for id, cancel := range s.cancel {
		cancels = append(cancels, cancel)
		delete(s.cancel, id)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *logStreams) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cancel)
}

// startLogStream handles a log_stream message: it validates the request,
// registers the stream, and runs the Docker follow in its own goroutine so
// the WS dispatch loop never blocks. The worker is tracked in pumps, so
// connectOnce waits for it before returning.
func (c *WSClient) startLogStream(connCtx context.Context, pumps *sync.WaitGroup, payload json.RawMessage) {
	if c.docker == nil {
		log.Printf("[ws] [ts=%s] docker not available for log_stream", ts())
		c.sendError("docker_not_available", "Docker is not installed on this server")
		return
	}

	var req logStreamRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		c.sendError("bad_request", "invalid log_stream payload")
		return
	}
	if req.Container == "" {
		c.sendError("bad_request", "log_stream requires a container")
		return
	}
	if req.StreamID == "" {
		c.sendError("bad_request", "log_stream requires a stream_id")
		return
	}
	if req.Tail <= 0 {
		req.Tail = defaultLogTail
	}
	if req.Tail > maxLogTail {
		req.Tail = maxLogTail
	}

	streamCtx, streamCancel := context.WithCancel(connCtx)
	if ok, reason := c.streams.add(req.StreamID, streamCancel); !ok {
		streamCancel()
		log.Printf("[ws] [ts=%s] log_stream rejected: %s", ts(), reason)
		c.sendError("log_stream_rejected", reason)
		return
	}

	pumps.Add(1)
	go func() {
		defer pumps.Done()
		defer c.streams.remove(req.StreamID)
		c.streamContainerLogs(streamCtx, req)
	}()

	log.Printf("[ws] [ts=%s] log stream started: container=%s tail=%d stream_id=%s active=%d",
		ts(), req.Container, req.Tail, req.StreamID, c.streams.len())
}

// stopLogStream handles a log_stream_stop message by cancelling the matching
// Docker follow.
func (c *WSClient) stopLogStream(payload json.RawMessage) {
	var req struct {
		StreamID string `json:"stream_id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.StreamID == "" {
		log.Printf("[ws] [ts=%s] log_stream_stop: invalid payload", ts())
		return
	}
	if c.streams.stop(req.StreamID) {
		log.Printf("[ws] [ts=%s] log_stream_stop: stream %s cancelled", ts(), req.StreamID)
	} else {
		log.Printf("[ws] [ts=%s] log_stream_stop: unknown stream %s", ts(), req.StreamID)
	}
}

// streamContainerLogs follows one container's logs and forwards them to the
// control plane over egressLow. It returns when the stream is cancelled, the
// connection drops, or the container stops.
func (c *WSClient) streamContainerLogs(streamCtx context.Context, req logStreamRequest) {
	logCh := make(chan string, 64)
	var forwarder sync.WaitGroup
	var linesSent atomic.Int64

	forwarder.Add(1)
	go func() {
		defer forwarder.Done()
		for line := range logCh {
			msg, err := json.Marshal(wsEnvelope{
				V: 1, Type: "log_line",
				Payload: wsLogLinePayload{
					StreamID:  req.StreamID,
					Container: req.Container,
					Stream:    "stdout",
					Line:      truncateLogLine(line),
					TS:        time.Now().UTC().Format(time.RFC3339),
				},
			})
			if err != nil {
				continue
			}
			select {
			case c.egressLow <- msg:
				linesSent.Add(1)
			default:
				log.Printf("[ws] [ts=%s] log_stream %s: egressLow full, dropping line", ts(), req.StreamID)
			}
		}
	}()

	err := c.docker.StreamLogs(streamCtx, req.Container, req.Tail, logCh)

	// StreamLogs has returned, so nothing else sends on logCh. Close it so
	// the forwarder drains the buffered lines and exits, then wait for it:
	// the worker must not outlive the connection teardown.
	close(logCh)
	forwarder.Wait()

	if err != nil {
		log.Printf("[ws] [ts=%s] log stream %s ended: %v (sent %d lines)", ts(), req.StreamID, err, linesSent.Load())
	} else {
		log.Printf("[ws] [ts=%s] log stream %s completed (sent %d lines)", ts(), req.StreamID, linesSent.Load())
	}
}

// truncateLogLine caps a single Docker log line so one huge line cannot bloat
// a WS frame.
func truncateLogLine(line string) string {
	if len(line) <= maxLogLineBytes {
		return line
	}
	return line[:maxLogLineBytes] + " ...(truncated)"
}
