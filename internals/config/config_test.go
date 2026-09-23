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
