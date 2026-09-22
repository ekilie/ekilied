package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/dtos"
	"github.com/ekilie/ekilied/internals/jobengine"
)

// Regression test for ekilie/ekilied#3: the HTTP 409 from the claim endpoint
// must be recognizable with errors.Is so the engine can skip execution.
func TestClaimJobMaps409ToErrJobAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	cfg := &config.Config{APIURL: srv.URL, SessionToken: "test-session"}
	c := NewWSClient(cfg, context.Background(), nil)

	_, err := c.ClaimJob(context.Background(), 7)
	if !errors.Is(err, jobengine.ErrJobAlreadyClaimed) {
		t.Fatalf("ClaimJob error = %v, want ErrJobAlreadyClaimed", err)
	}
}

// Regression test for ekilie/ekilied#16: the heartbeat envelope must carry
// the request struct directly so one json.Marshal produces the whole message,
// and the wire format must stay byte-identical to the old double-marshal.
func TestHeartbeatWSMessageWireFormat(t *testing.T) {
	req := dtos.HeartbeatRequest{
		AgentID:  "agt_42",
		ServerID: 42,
		TS:       "2026-09-22T10:00:00Z",
		Metrics: dtos.HeartbeatMetrics{
			CPUPercent:    12.5,
			MemoryPercent: 33.25,
			DiskPercent:   50,
			UptimeSeconds: 3600,
			AgentVersion:  "1.2.3",
		},
	}

	got, err := heartbeatWSMessage(req)
	if err != nil {
		t.Fatalf("heartbeatWSMessage: %v", err)
	}
	want := `{"v":1,"type":"heartbeat","payload":{"agent_id":"agt_42","server_id":42,"ts":"2026-09-22T10:00:00Z","metrics":{"cpu_percent":12.5,"memory_percent":33.25,"disk_percent":50,"uptime_seconds":3600,"agent_version":"1.2.3"}}}`
	if string(got) != want {
		t.Fatalf("wire format changed:\n got: %s\nwant: %s", got, want)
	}
}

func TestContainerListWSMessageWireFormat(t *testing.T) {
	infos := []containerInfo{{
		ID:     "abc123",
		Name:   "web",
		Image:  "nginx:latest",
		State:  "running",
		Status: "Up 2 minutes",
		Ports:  []string{"8080->80/tcp"},
		Uptime: "2m0s",
	}}

	got, err := containerListWSMessage(infos)
	if err != nil {
		t.Fatalf("containerListWSMessage: %v", err)
	}
	// json.Marshal escapes '>' as \u003e, so the golden string carries it too.
	want := `{"v":1,"type":"container_list","payload":{"containers":[{"id":"abc123","name":"web","image":"nginx:latest","state":"running","status":"Up 2 minutes","ports":["8080-\u003e80/tcp"],"uptime":"2m0s"}]}}`
	if string(got) != want {
		t.Fatalf("wire format changed:\n got: %s\nwant: %s", got, want)
	}

	// An empty listing must serialize as [] (never null) so the dashboard can
	// range over it.
	got, err = containerListWSMessage([]containerInfo{})
	if err != nil {
		t.Fatalf("containerListWSMessage(empty): %v", err)
	}
	want = `{"v":1,"type":"container_list","payload":{"containers":[]}}`
	if string(got) != want {
		t.Fatalf("empty wire format changed:\n got: %s\nwant: %s", got, want)
	}
}

func BenchmarkHeartbeatWSMessage(b *testing.B) {
	req := dtos.HeartbeatRequest{
		AgentID:  "agt_42",
		ServerID: 42,
		TS:       "2026-09-22T10:00:00Z",
		Metrics: dtos.HeartbeatMetrics{
			CPUPercent:    12.5,
			MemoryPercent: 33.25,
			DiskPercent:   50,
			UptimeSeconds: 3600,
			AgentVersion:  "1.2.3",
		},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := heartbeatWSMessage(req); err != nil {
			b.Fatal(err)
		}
	}
}
