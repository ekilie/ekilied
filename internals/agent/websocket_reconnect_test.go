package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ekilie/ekilied/internals/config"
)

// newWSTestServer starts a WebSocket test server. The handler receives the
// accepted connection and its 1-based connection number.
func newWSTestServer(t *testing.T, handler func(conn *websocket.Conn, n int)) *httptest.Server {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		handler(conn, int(conns.Add(1)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// Regression test for ekilie/ekilied#4: every per-connection pump must exit
// before connectOnce returns, so reconnects do not accumulate goroutines.
func TestConnectOncePumpsExitOnDisconnect(t *testing.T) {
	srv := newWSTestServer(t, func(conn *websocket.Conn, n int) {
		// Let the handshake complete and the agent's pumps start, then drop.
		time.Sleep(50 * time.Millisecond)
		conn.Close(websocket.StatusNormalClosure, "bye")
	})

	cfg := &config.Config{WsURL: wsURL(srv), SessionToken: "test"}
	c := NewWSClient(cfg, context.Background(), nil)

	// Warm up once so lazy runtime goroutines are already started.
	if err := c.connectOnce(context.Background()); err == nil {
		t.Fatal("connectOnce = nil, want connection closed error")
	}

	base := runtime.NumGoroutine()

	const reconnects = 20
	for i := 0; i < reconnects; i++ {
		if err := c.connectOnce(context.Background()); err == nil {
			t.Fatal("connectOnce = nil, want connection closed error")
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > base+2 {
		t.Fatalf("goroutines grew from %d to %d across %d reconnects (pump leak)", base, got, reconnects)
	}
}

// Messages queued after a reconnect must reach the live connection, never a
// leaked pump from the dead one.
func TestEgressMessagesReachLiveConnectionAfterReconnect(t *testing.T) {
	var mu sync.Mutex
	received := map[int][]string{}

	srv := newWSTestServer(t, func(conn *websocket.Conn, n int) {
		if n == 1 {
			time.Sleep(50 * time.Millisecond)
			conn.Close(websocket.StatusNormalClosure, "bye")
			return
		}
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			mu.Lock()
			received[n] = append(received[n], string(data))
			mu.Unlock()
		}
	})

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &config.Config{WsURL: wsURL(srv), SessionToken: "test"}
	c := NewWSClient(cfg, rootCtx, nil)

	// First connection: dropped by the server, connectOnce returns.
	if err := c.connectOnce(rootCtx); err == nil {
		t.Fatal("first connectOnce = nil, want connection closed error")
	}

	// Second connection: stays up and records what it receives.
	connDone := make(chan struct{})
	go func() {
		_ = c.connectOnce(rootCtx)
		close(connDone)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for c.getConn() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if c.getConn() == nil {
		t.Fatal("second connection was never established")
	}

	const n = 10
	for i := 0; i < n; i++ {
		select {
		case c.egress <- []byte(fmt.Sprintf("msg-%d", i)):
		case <-time.After(2 * time.Second):
			t.Fatalf("egress channel blocked sending message %d", i)
		}
	}

	deadline = time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := len(received[2])
		mu.Unlock()
		if got >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	got := append([]string(nil), received[2]...)
	mu.Unlock()
	if len(got) != n {
		t.Fatalf("live connection received %d/%d messages (leaked pump stole some): %v", len(got), n, got)
	}
	for i, msg := range got {
		if want := fmt.Sprintf("msg-%d", i); msg != want {
			t.Fatalf("message %d = %q, want %q", i, msg, want)
		}
	}

	// Close the live connection so connectOnce and the server handler exit.
	if conn := c.getConn(); conn != nil {
		conn.Close(websocket.StatusNormalClosure, "test done")
	}
	select {
	case <-connDone:
	case <-time.After(3 * time.Second):
		t.Fatal("connectOnce did not return after closing the live connection")
	}
}
