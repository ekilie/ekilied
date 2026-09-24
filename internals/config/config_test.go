package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestSaveSessionUpdatesInPlace(t *testing.T) {
	path := writeFixture(t, `# ekilie agent config
server_id: 42
agent_id: agt_old
session_token: old_token
api_url: https://engine.example.com
poll_interval: 5
`)

	if err := SaveSession(path, "agt_new", "ek_session_new"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"# ekilie agent config\n",
		"server_id: 42\n",
		"agent_id: agt_new\n",
		"session_token: ek_session_new\n",
		"api_url: https://engine.example.com\n",
		"poll_interval: 5\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "agt_old") || strings.Contains(got, "old_token") {
		t.Errorf("stale credentials remain in:\n%s", got)
	}

	// Round-trip through the real loader.
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentID != "agt_new" || cfg.SessionToken != "ek_session_new" {
		t.Errorf("round-trip mismatch: %+v", cfg)
	}
	if cfg.ServerID != 42 || cfg.APIURL != "https://engine.example.com" || cfg.PollInterval != 5 {
		t.Errorf("other keys not preserved: %+v", cfg)
	}
}

func TestSaveSessionAppendsMissingKeys(t *testing.T) {
	path := writeFixture(t, "api_url: https://engine.example.com\n")

	if err := SaveSession(path, "agt_1", "ek_session_1"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentID != "agt_1" || cfg.SessionToken != "ek_session_1" {
		t.Errorf("appended keys not loaded: %+v", cfg)
	}
}

func TestSaveSessionCreatesFile0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "agent.yml")

	if err := SaveSession(path, "agt_1", "ek_session_1"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0600 {
		t.Errorf("mode = %o, want 600", mode)
	}
}

func TestSaveSessionTightensExistingMode(t *testing.T) {
	path := writeFixture(t, "api_url: https://engine.example.com\n")

	if err := SaveSession(path, "agt_1", "ek_session_1"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0600 {
		t.Errorf("mode = %o, want 600", mode)
	}
}

func TestSaveSessionQuotesSpecialValues(t *testing.T) {
	path := writeFixture(t, "api_url: https://engine.example.com\n")

	// Tokens with YAML-significant characters must survive a round-trip.
	if err := SaveSession(path, "agt 1", "tok:en #x"); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentID != "agt 1" || cfg.SessionToken != "tok:en #x" {
		t.Errorf("round-trip mismatch: %+v", cfg)
	}
}

func TestSaveSessionValidation(t *testing.T) {
	if err := SaveSession("", "a", "b"); err == nil {
		t.Error("want error for empty path")
	}
	path := filepath.Join(t.TempDir(), "agent.yml")
	if err := SaveSession(path, "", "b"); err == nil {
		t.Error("want error for empty agent id")
	}
	if err := SaveSession(path, "a", ""); err == nil {
		t.Error("want error for empty session token")
	}
}

// Regression test for ekilie/ekilied#29: auto_update from the config file must
// be honored unless a flag or environment variable explicitly overrides it,
// and the effective source must be reported.
func TestAutoUpdatePrecedence(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		path := writeFixture(t, "api_url: https://engine.example.com\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.AutoUpdate {
			t.Fatal("default AutoUpdate = false, want true")
		}
		if cfg.AutoUpdateSource != "default" {
			t.Fatalf("source = %q, want default", cfg.AutoUpdateSource)
		}
	})

	t.Run("file false is honored", func(t *testing.T) {
		path := writeFixture(t, "api_url: https://engine.example.com\nauto_update: false\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.AutoUpdate {
			t.Fatal("file auto_update: false was ignored")
		}
		if cfg.AutoUpdateSource != "config file" {
			t.Fatalf("source = %q, want config file", cfg.AutoUpdateSource)
		}
	})

	t.Run("flag beats file in both directions", func(t *testing.T) {
		path := writeFixture(t, "api_url: https://engine.example.com\nauto_update: false\n")
		yes := true
		cfg, err := Load(path, WithFlags(FlagOverrides{AutoUpdate: &yes}))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.AutoUpdate || cfg.AutoUpdateSource != "flag" {
			t.Fatalf("flag true: AutoUpdate=%v source=%q, want true/flag", cfg.AutoUpdate, cfg.AutoUpdateSource)
		}

		path = writeFixture(t, "api_url: https://engine.example.com\nauto_update: true\n")
		no := false
		cfg, err = Load(path, WithFlags(FlagOverrides{AutoUpdate: &no}))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.AutoUpdate || cfg.AutoUpdateSource != "flag" {
			t.Fatalf("flag false: AutoUpdate=%v source=%q, want false/flag", cfg.AutoUpdate, cfg.AutoUpdateSource)
		}
	})

	t.Run("environment beats file", func(t *testing.T) {
		t.Setenv("EKILIED_AUTO_UPDATE", "true")
		path := writeFixture(t, "api_url: https://engine.example.com\nauto_update: false\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.AutoUpdate || cfg.AutoUpdateSource != "environment" {
			t.Fatalf("env true: AutoUpdate=%v source=%q, want true/environment", cfg.AutoUpdate, cfg.AutoUpdateSource)
		}
	})
}

// Regression tests for ekilie/ekilied#30: agent.yml is parsed with yaml.v3, so
// inline comments, quoting, and colons behave the way YAML says they do.
func TestParseYAMLValues(t *testing.T) {
	path := writeFixture(t, `api_url: https://engine.example.com:8443/path # prod
poll_interval: 5 # seconds
session_token: "tok:en #x"
agent_id: abc"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIURL != "https://engine.example.com:8443/path" {
		t.Fatalf("APIURL = %q, want the URL without the inline comment", cfg.APIURL)
	}
	if cfg.PollInterval != 5 {
		t.Fatalf("PollInterval = %d, want 5 (inline comment must not corrupt it)", cfg.PollInterval)
	}
	if cfg.SessionToken != "tok:en #x" {
		t.Fatalf("SessionToken = %q, want the quoted value intact", cfg.SessionToken)
	}
	if cfg.AgentID != `abc"` {
		t.Fatalf("AgentID = %q, want the trailing quote preserved", cfg.AgentID)
	}
}

// Malformed values must fail startup with an error that names the offending
// line instead of silently defaulting.
func TestParseYAMLFailsFast(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantKey string
	}{
		{
			name:    "invalid int",
			content: "api_url: https://x\npoll_interval: not-a-number\n",
			wantKey: "poll_interval",
		},
		{
			name:    "unknown key",
			content: "api_url: https://x\npoll_intervall: 5\n",
			wantKey: "poll_intervall",
		},
		{
			name:    "invalid bool",
			content: "api_url: https://x\nauto_update: sometimes\n",
			wantKey: "auto_update",
		},
		{
			name:    "malformed yaml",
			content: "api_url: [unclosed\n",
			wantKey: "api_url",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFixture(t, tc.content)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Fatalf("error %q does not name %q", err, tc.wantKey)
			}
		})
	}
}

// Setup output must round-trip through Load even when credentials contain
// YAML-significant characters.
func TestMarshalSetupRoundTrip(t *testing.T) {
	setup := SetupConfig{
		ServerID:     42,
		AgentID:      `agt_42"`,
		SessionToken: `ek_session_a:b #c "d"`,
		APIURL:       "https://engine.example.com",
		WsURL:        "wss://engine.example.com/api/v1/agents/ws",
		PollInterval: 5,
	}

	data, err := MarshalSetup(setup)
	if err != nil {
		t.Fatalf("MarshalSetup: %v", err)
	}

	path := filepath.Join(t.TempDir(), "agent.yml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ServerID != setup.ServerID || cfg.AgentID != setup.AgentID ||
		cfg.SessionToken != setup.SessionToken || cfg.APIURL != setup.APIURL ||
		cfg.WsURL != setup.WsURL || cfg.PollInterval != setup.PollInterval {
		t.Fatalf("round trip mismatch:\n got: %+v\nwant: %+v", cfg, setup)
	}
}

// Regression tests for ekilie/ekilied#32: the derived WebSocket URL must use
// the backend's real endpoint, /api/v1/agents/ws.
func TestDeriveWsURL(t *testing.T) {
	cases := []struct {
		name    string
		api     string
		want    string
		wantErr bool
	}{
		{name: "bare host", api: "https://engine.ekilie.cloud", want: "wss://engine.ekilie.cloud/api/v1/agents/ws"},
		{name: "trailing slash", api: "https://engine.ekilie.cloud/", want: "wss://engine.ekilie.cloud/api/v1/agents/ws"},
		{name: "api prefix", api: "https://engine.ekilie.cloud/api/v1", want: "wss://engine.ekilie.cloud/api/v1/agents/ws"},
		{name: "api prefix trailing slash", api: "https://engine.ekilie.cloud/api/v1/", want: "wss://engine.ekilie.cloud/api/v1/agents/ws"},
		{name: "full endpoint", api: "https://engine.ekilie.cloud/api/v1/agents/ws", want: "wss://engine.ekilie.cloud/api/v1/agents/ws"},
		{name: "http dev", api: "http://localhost:8080", want: "ws://localhost:8080/api/v1/agents/ws"},
		{name: "custom base path", api: "https://host/backend", want: "wss://host/backend/api/v1/agents/ws"},
		{name: "scheme-less host", api: "engine.ekilie.cloud", want: "wss://engine.ekilie.cloud/api/v1/agents/ws"},
		{name: "query dropped", api: "https://engine.ekilie.cloud?debug=1", want: "wss://engine.ekilie.cloud/api/v1/agents/ws"},
		{name: "no host", api: "https://", wantErr: true},
		{name: "bad scheme", api: "ftp://engine.ekilie.cloud", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveWsURL(tc.api)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("deriveWsURL(%q) = %q, want error", tc.api, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("deriveWsURL(%q): %v", tc.api, err)
			}
			if got != tc.want {
				t.Fatalf("deriveWsURL(%q) = %q, want %q", tc.api, got, tc.want)
			}
		})
	}
}

func TestLoadDerivesWsURL(t *testing.T) {
	path := writeFixture(t, "api_url: https://engine.ekilie.cloud\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "wss://engine.ekilie.cloud/api/v1/agents/ws"; cfg.WsURL != want {
		t.Fatalf("WsURL = %q, want %q", cfg.WsURL, want)
	}
}

func TestLoadKeepsExplicitWsURL(t *testing.T) {
	path := writeFixture(t, "api_url: https://engine.ekilie.cloud\nws_url: wss://custom.example.com/socket\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "wss://custom.example.com/socket"; cfg.WsURL != want {
		t.Fatalf("WsURL = %q, want the explicit value %q", cfg.WsURL, want)
	}
}

// Regression tests for ekilie/ekilied#28: one default poll interval and a
// documented precedence (server > env > flags > file > defaults).
func TestPollIntervalDefault(t *testing.T) {
	if got := Defaults().PollInterval; got != 5 {
		t.Fatalf("Defaults().PollInterval = %d, want 5", got)
	}
}

func TestPollIntervalPrecedence(t *testing.T) {
	path := writeFixture(t, "api_url: https://engine.example.com\npoll_interval: 30\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval != 30 {
		t.Fatalf("file poll_interval = %d, want 30", cfg.PollInterval)
	}

	cfg, err = Load(path, WithFlags(FlagOverrides{PollInterval: 45}))
	if err != nil {
		t.Fatalf("Load with flag: %v", err)
	}
	if cfg.PollInterval != 45 {
		t.Fatalf("flag poll_interval = %d, want 45", cfg.PollInterval)
	}

	t.Setenv("EKILIED_POLL_INTERVAL", "60")
	cfg, err = Load(path, WithFlags(FlagOverrides{PollInterval: 45}))
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.PollInterval != 60 {
		t.Fatalf("env poll_interval = %d, want 60", cfg.PollInterval)
	}
}
