package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/docker/docker/api/types"
	"github.com/ekilie/ekilied/internals/config"
)

// fakeDocker implements dockerService for tests. StreamLogs mimics a Docker
// follow: it reports the call, emits an optional line, then blocks until its
// context is cancelled.
type fakeDocker struct {
	started chan streamCall
	stopped chan string
	line    string
}

type streamCall struct {
	container string
	tail      int
}

func (f *fakeDocker) ListContainers(ctx context.Context) ([]types.Container, error) {
	return nil, nil
}

func (f *fakeDocker) StreamLogs(ctx context.Context, containerName string, tail int, logCh chan<- string) error {
	f.started <- streamCall{container: containerName, tail: tail}
	if f.line != "" {
		logCh <- f.line
	}
	<-ctx.Done()
	f.stopped <- containerName
	return ctx.Err()
}

func (f *fakeDocker) Close() error { return nil }

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		started: make(chan streamCall, 10),
		stopped: make(chan string, 10),
	}
}

func TestLogStreamsRegistry(t *testing.T) {
	s := newLogStreams(2)

	var cancelledA, cancelledX bool
	if ok, reason := s.add("a", func() { cancelledA = true }); !ok {
		t.Fatalf("add a: %s", reason)
	}
	if ok, reason := s.add("a", func() {}); ok || reason != "stream_id already active" {
		t.Fatalf("duplicate add = (%v, %q), want rejected", ok, reason)
	}
	if ok, _ := s.add("b", func() {}); !ok {
		t.Fatal("add b failed")
	}
	if ok, reason := s.add("c", func() {}); ok || !strings.Contains(reason, "too many") {
		t.Fatalf("over-capacity add = (%v, %q), want rejected", ok, reason)
	}
	if got := s.len(); got != 2 {
		t.Fatalf("len = %d, want 2", got)
	}

	if !s.stop("a") {
		t.Fatal("stop a = false, want true")
	}
	if !cancelledA {
		t.Fatal("stop a did not cancel the stream")
	}
	if s.stop("missing") {
		t.Fatal("stop missing = true, want false")
	}
	if got := s.len(); got != 1 {
		t.Fatalf("len after stop = %d, want 1", got)
	}

	if ok, _ := s.add("x", func() { cancelledX = true }); !ok {
		t.Fatal("add x failed")
	}
	s.stopAll()
	if s.len() != 0 {
		t.Fatalf("len after stopAll = %d, want 0", s.len())
	}
	if !cancelledX {
		t.Fatal("stopAll did not cancel the stream")
	}
}

func TestTruncateLogLine(t *testing.T) {
	short := "hello"
	if got := truncateLogLine(short); got != short {
		t.Fatalf("short line = %q, want unchanged", got)
	}

	long := strings.Repeat("x", maxLogLineBytes+100)
	got := truncateLogLine(long)
	if !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("long line not marked truncated: %q", got[len(got)-30:])
	}
	if len(got) != maxLogLineBytes+len(" ...(truncated)") {
		t.Fatalf("truncated length = %d, want %d", len(got), maxLogLineBytes+len(" ...(truncated)"))
	}
}

// Regression test for ekilie/ekilied#5: a log stream must not block the WS
// dispatch loop, log_stream_stop must end the Docker follow, and the registry
// must free the slot.
func TestLogStreamRunsAsyncAndStops(t *testing.T) {
	fake := newFakeDocker()
	jobCh := make(chan uint, 1)
	sendJob := make(chan struct{})
	sendStop := make(chan struct{})

	srv := newWSTestServer(t, func(conn *websocket.Conn, n int) {
		ctx := context.Background()
		// Tail over the cap must be clamped by the agent.
		conn.Write(ctx, websocket.MessageText,
			[]byte(`{"type":"log_stream","payload":{"container":"web","tail":99999,"stream_id":"s1"}}`))

		<-sendJob
		conn.Write(ctx, websocket.MessageText,
			[]byte(`{"type":"job","payload":{"job_id":7}}`))

		<-sendStop
		conn.Write(ctx, websocket.MessageText,
			[]byte(`{"type":"log_stream_stop","payload":{"stream_id":"s1"}}`))

		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	})

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &config.Config{WsURL: wsURL(srv), SessionToken: "test"}
	c := NewWSClient(cfg, rootCtx, func(ctx context.Context, jobID uint) { jobCh <- jobID })
	c.docker = fake

	connDone := make(chan struct{})
	go func() {
		_ = c.connectOnce(rootCtx)
		close(connDone)
	}()

	select {
	case call := <-fake.started:
		if call.container != "web" {
			t.Fatalf("container = %q, want web", call.container)
		}
		if call.tail != maxLogTail {
			t.Fatalf("tail = %d, want clamped %d", call.tail, maxLogTail)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("log stream never started")
	}
	if got := c.streams.len(); got != 1 {
		t.Fatalf("active streams = %d, want 1", got)
	}

	// The dispatch loop must still process job triggers while the stream runs.
	close(sendJob)
	select {
	case jobID := <-jobCh:
		if jobID != 7 {
			t.Fatalf("job id = %d, want 7", jobID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("job trigger was not dispatched while a log stream was open")
	}

	// log_stream_stop must end the Docker follow and free the registry slot.
	close(sendStop)
	select {
	case <-fake.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("log_stream_stop did not cancel the Docker follow")
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.streams.len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.streams.len(); got != 0 {
		t.Fatalf("active streams = %d, want 0 after stop", got)
	}

	cancel()
	select {
	case <-connDone:
	case <-time.After(3 * time.Second):
		t.Fatal("connectOnce did not return after cancel")
	}
}

// A dropped WS connection must cancel every open log stream.
func TestLogStreamCancelledOnConnectionDrop(t *testing.T) {
	fake := newFakeDocker()

	srv := newWSTestServer(t, func(conn *websocket.Conn, n int) {
		ctx := context.Background()
		conn.Write(ctx, websocket.MessageText,
			[]byte(`{"type":"log_stream","payload":{"container":"web","stream_id":"s1"}}`))
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	})

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &config.Config{WsURL: wsURL(srv), SessionToken: "test"}
	c := NewWSClient(cfg, rootCtx, nil)
	c.docker = fake

	connDone := make(chan struct{})
	go func() {
		_ = c.connectOnce(rootCtx)
		close(connDone)
	}()

	select {
	case <-fake.started:
	case <-time.After(3 * time.Second):
		t.Fatal("log stream never started")
	}
	if got := c.streams.len(); got != 1 {
		t.Fatalf("active streams = %d, want 1", got)
	}

	// Drop the connection from the agent side.
	if conn := c.getConn(); conn != nil {
		conn.Close(websocket.StatusNormalClosure, "drop")
	}

	select {
	case <-fake.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("connection drop did not cancel the Docker follow")
	}
	select {
	case <-connDone:
	case <-time.After(3 * time.Second):
		t.Fatal("connectOnce did not return after the connection dropped")
	}
	if got := c.streams.len(); got != 0 {
		t.Fatalf("active streams = %d, want 0 after connection drop", got)
	}
}
