package agent

import (
	"context"
	"testing"
	"time"

	"github.com/ekilie/ekilied/internals/config"
)

// Regression test for ekilie/ekilied#28: the configured poll interval is
// honored in both connected and disconnected states. The old loop hardcoded
// 5s whenever the WebSocket was up, so poll_interval was ignored.
func TestPollIntervalHonorsConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &config.Config{PollInterval: 30}
	ws := NewWSClient(cfg, ctx, nil)
	e := &Ekilied{cfg: cfg, ws: ws}

	ws.connected.Store(true)
	if got := e.pollInterval(); got != 30*time.Second {
		t.Fatalf("pollInterval with WS connected = %s, want 30s", got)
	}

	ws.connected.Store(false)
	if got := e.pollInterval(); got != 30*time.Second {
		t.Fatalf("pollInterval with WS down = %s, want 30s", got)
	}

	cfg.PollInterval = 7
	if got := e.pollInterval(); got != 7*time.Second {
		t.Fatalf("pollInterval after config change = %s, want 7s", got)
	}
}
