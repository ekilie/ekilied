package agent

import (
	"errors"
	"testing"

	"github.com/ekilie/ekilied/internals/config"
)

func stubHostUptime(t *testing.T, uptime uint64, err error) {
	t.Helper()
	old := hostUptimeFunc
	hostUptimeFunc = func() (uint64, error) { return uptime, err }
	t.Cleanup(func() { hostUptimeFunc = old })
}

// Regression test for ekilie/ekilied#33: UptimeSeconds must report host
// uptime, not the agent process uptime.
func TestCollectMetricsUsesHostUptime(t *testing.T) {
	stubHostUptime(t, 424242, nil)

	m := collectMetrics()

	if m.UptimeSeconds != 424242 {
		t.Fatalf("UptimeSeconds = %d, want host uptime 424242", m.UptimeSeconds)
	}
	if m.UptimeFallback {
		t.Fatal("UptimeFallback = true, want false when the host uptime is available")
	}
	if m.AgentUptimeSeconds < 0 || m.AgentUptimeSeconds > 10 {
		t.Fatalf("AgentUptimeSeconds = %d, want the freshly started process uptime", m.AgentUptimeSeconds)
	}
	if m.AgentVersion != config.Version {
		t.Fatalf("AgentVersion = %q, want %q", m.AgentVersion, config.Version)
	}
}

// When the host uptime cannot be read, the field falls back to the agent
// uptime and is flagged instead of silently reporting zero.
func TestCollectMetricsFallsBackToAgentUptime(t *testing.T) {
	stubHostUptime(t, 0, errors.New("uptime unavailable"))

	m := collectMetrics()

	if !m.UptimeFallback {
		t.Fatal("UptimeFallback = false, want true on host uptime error")
	}
	if m.UptimeSeconds != m.AgentUptimeSeconds {
		t.Fatalf("UptimeSeconds = %d, want agent uptime %d", m.UptimeSeconds, m.AgentUptimeSeconds)
	}
	if m.UptimeSeconds < 0 || m.UptimeSeconds > 10 {
		t.Fatalf("UptimeSeconds = %d, want a small value for a freshly started process", m.UptimeSeconds)
	}
}
