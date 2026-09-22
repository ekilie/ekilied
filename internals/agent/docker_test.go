package agent

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types"
)

// Regression test for ekilie/ekilied#6: Docker can return containers with an
// empty Names slice or a short ID, and indexing blindly killed the daemon.
func TestContainerToInfoEdgeCases(t *testing.T) {
	cases := []struct {
		name     string
		in       types.Container
		wantID   string
		wantName string
		wantPort string
		wantUp   bool
	}{
		{
			name:     "empty names slice",
			in:       types.Container{ID: "abcdef1234567890"},
			wantID:   "abcdef123456",
			wantName: "",
		},
		{
			name:     "short id",
			in:       types.Container{ID: "abc", Names: []string{"/web"}},
			wantID:   "abc",
			wantName: "web",
		},
		{
			name:     "name without slash",
			in:       types.Container{ID: "0123456789ab", Names: []string{"plain"}},
			wantID:   "0123456789ab",
			wantName: "plain",
		},
		{
			name:     "exactly twelve char id",
			in:       types.Container{ID: "123456789012", Names: []string{"/a"}},
			wantID:   "123456789012",
			wantName: "a",
		},
		{
			name:     "nil ports",
			in:       types.Container{ID: "abcdef123456", Names: []string{"/web"}, Ports: nil},
			wantID:   "abcdef123456",
			wantName: "web",
		},
		{
			name: "published port",
			in: types.Container{
				ID: "abcdef123456", Names: []string{"/web"},
				Ports: []types.Port{{IP: "0.0.0.0", PublicPort: 8080, PrivatePort: 80, Type: "tcp"}},
			},
			wantID:   "abcdef123456",
			wantName: "web",
			wantPort: "0.0.0.0:8080->80/tcp",
		},
		{
			name: "private port only",
			in: types.Container{
				ID: "abcdef123456", Names: []string{"/db"},
				Ports: []types.Port{{PrivatePort: 5432, Type: "tcp"}},
			},
			wantID:   "abcdef123456",
			wantName: "db",
			wantPort: "5432/tcp",
		},
		{
			name: "running container has uptime",
			in: types.Container{
				ID: "abcdef123456", Names: []string{"/web"},
				State: "running", Created: time.Now().Add(-2 * time.Minute).Unix(),
			},
			wantID:   "abcdef123456",
			wantName: "web",
			wantUp:   true,
		},
		{
			name:     "empty container",
			in:       types.Container{},
			wantID:   "",
			wantName: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := containerToInfo(tc.in)
			if got.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tc.wantID)
			}
			if got.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tc.wantName)
			}
			if tc.wantPort == "" {
				if len(got.Ports) != 0 {
					t.Errorf("Ports = %v, want none", got.Ports)
				}
			} else if len(got.Ports) != 1 || got.Ports[0] != tc.wantPort {
				t.Errorf("Ports = %v, want [%q]", got.Ports, tc.wantPort)
			}
			if tc.wantUp && got.Uptime == "" {
				t.Error("Uptime = empty, want a duration for a running container")
			}
		})
	}
}

// FuzzContainerToInfo makes sure no combination of ID and name can panic the
// container conversion.
func FuzzContainerToInfo(f *testing.F) {
	f.Add("abcdef1234567890", "/web")
	f.Add("", "")
	f.Add("abc", "plain")
	f.Add("short", "////")

	f.Fuzz(func(t *testing.T, id, name string) {
		c := types.Container{ID: id}
		if name != "" {
			c.Names = []string{name}
		}
		_ = containerToInfo(c)
	})
}
