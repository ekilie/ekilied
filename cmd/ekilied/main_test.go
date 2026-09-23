package main

import (
	"path/filepath"
	"testing"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/models"
	"github.com/ekilie/ekilied/pkg/database"
)

func openTestDB(t *testing.T) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := database.Connect(database.DefaultConfig(dbPath)); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.AutoMigrate(models.AllModels()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// Regression test for ekilie/ekilied#45: an agent whose registration
// succeeded but was never written to the config file must resume from the
// locally saved identity instead of re-registering with a burned token.
func TestRestoreSessionFromDB(t *testing.T) {
	openTestDB(t)

	database.GetDB().Create(&models.Identity{
		AgentID:      "agt_saved",
		ServerID:     42,
		SessionToken: "ek_session_saved",
		APIURL:       "https://engine.example.com",
		WsURL:        "wss://engine.example.com/agents/ws",
		Version:      "test",
	})

	cfg := &config.Config{APIURL: "https://engine.example.com"}
	if err := restoreSessionFromDB(cfg); err != nil {
		t.Fatalf("restoreSessionFromDB: %v", err)
	}
	if cfg.AgentID != "agt_saved" || cfg.SessionToken != "ek_session_saved" {
		t.Errorf("identity not adopted: %+v", cfg)
	}
	if cfg.ServerID != 42 {
		t.Errorf("server id not adopted: %+v", cfg)
	}
	if !cfg.HasSession() || cfg.NeedsRegistration() {
		t.Errorf("restored cfg should have session and need no registration: %+v", cfg)
	}
}

func TestRestoreSessionFromDBKeepsExplicitServerID(t *testing.T) {
	openTestDB(t)

	database.GetDB().Create(&models.Identity{
		AgentID:      "agt_saved",
		ServerID:     42,
		SessionToken: "ek_session_saved",
		APIURL:       "https://engine.example.com",
		WsURL:        "wss://engine.example.com/agents/ws",
		Version:      "test",
	})

	cfg := &config.Config{APIURL: "https://engine.example.com", ServerID: 7}
	if err := restoreSessionFromDB(cfg); err != nil {
		t.Fatalf("restoreSessionFromDB: %v", err)
	}
	if cfg.ServerID != 7 {
		t.Errorf("explicit server id overwritten: %+v", cfg)
	}
}

func TestRestoreSessionFromDBEmpty(t *testing.T) {
	openTestDB(t)

	cfg := &config.Config{APIURL: "https://engine.example.com"}
	if err := restoreSessionFromDB(cfg); err == nil {
		t.Error("want error for empty identity table")
	}
	if cfg.HasSession() {
		t.Errorf("cfg must not gain a session: %+v", cfg)
	}
}

func TestRestoreSessionFromDBWithoutToken(t *testing.T) {
	openTestDB(t)

	database.GetDB().Create(&models.Identity{
		AgentID:  "agt_nosession",
		ServerID: 42,
		APIURL:   "https://engine.example.com",
		Version:  "test",
	})

	cfg := &config.Config{APIURL: "https://engine.example.com"}
	if err := restoreSessionFromDB(cfg); err == nil {
		t.Error("want error for identity without session token")
	}
	if cfg.HasSession() {
		t.Errorf("cfg must not gain a session: %+v", cfg)
	}
}

// Regression test for ekilie/ekilied#29: the auto-update flag must only
// override the config file when it was explicitly passed.
func TestAutoUpdateOverride(t *testing.T) {
	var unset Flags
	if got := autoUpdateOverride(unset); got != nil {
		t.Fatalf("unset flag override = %v, want nil so the config file wins", got)
	}

	var on Flags
	if err := on.AutoUpdate.Set("true"); err != nil {
		t.Fatalf("Set(true): %v", err)
	}
	if got := autoUpdateOverride(on); got == nil || *got != true {
		t.Fatalf("explicit true override = %v, want true", got)
	}

	var off Flags
	if err := off.AutoUpdate.Set("false"); err != nil {
		t.Fatalf("Set(false): %v", err)
	}
	if got := autoUpdateOverride(off); got == nil || *got != false {
		t.Fatalf("explicit false override = %v, want false", got)
	}
}

func TestOptionalBoolParsing(t *testing.T) {
	var b optionalBool
	if !b.IsBoolFlag() {
		t.Fatal("IsBoolFlag = false, want true so --auto-update works without a value")
	}
	if got := b.String(); got != "" {
		t.Fatalf("unset String = %q, want empty", got)
	}
	if err := b.Set("false"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !b.set || b.value {
		t.Fatalf("after Set(false): set=%v value=%v, want true/false", b.set, b.value)
	}
	if got := b.String(); got != "false" {
		t.Fatalf("String = %q, want false", got)
	}
	if err := b.Set("not-a-bool"); err == nil {
		t.Fatal("invalid boolean accepted")
	}
}
