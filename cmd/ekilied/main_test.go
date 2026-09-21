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
