package agent

import (
	"path/filepath"
	"testing"

	"github.com/ekilie/ekilied/internals/models"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openIdentityDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "identity.db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&models.Identity{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedIdentity(t *testing.T, db *gorm.DB, agentID string) {
	t.Helper()
	err := db.Create(&models.Identity{
		AgentID:      agentID,
		ServerID:     1,
		SessionToken: "old-token",
		APIURL:       "https://old.example.com",
		WsURL:        "wss://old.example.com/agents/ws",
		Version:      "0.0.1",
	}).Error
	if err != nil {
		t.Fatalf("seed identity: %v", err)
	}
}

func TestSaveIdentityReplacesRow(t *testing.T) {
	db := openIdentityDB(t)
	seedIdentity(t, db, "agt_old")

	err := saveIdentity(db, &models.Identity{
		AgentID:      "agt_new",
		ServerID:     2,
		SessionToken: "new-token",
		APIURL:       "https://new.example.com",
		WsURL:        "wss://new.example.com/agents/ws",
		Version:      "0.0.2",
	})
	if err != nil {
		t.Fatalf("saveIdentity: %v", err)
	}

	var rows []models.Identity
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("identity rows = %d, want 1", len(rows))
	}
	if rows[0].AgentID != "agt_new" || rows[0].SessionToken != "new-token" {
		t.Fatalf("identity = %+v, want the new row", rows[0])
	}
}

// Regression test for ekilie/ekilied#36: a failure between the delete and the
// insert must not leave the agent with no identity row. The insert is failed
// with a trigger, which is what a crash or write error looks like to GORM.
func TestSaveIdentityIsAtomic(t *testing.T) {
	db := openIdentityDB(t)
	seedIdentity(t, db, "agt_old")

	err := db.Exec(`CREATE TRIGGER fail_identity_insert BEFORE INSERT ON identities
		WHEN NEW.agent_id = 'agt_boom'
		BEGIN SELECT RAISE(ABORT, 'boom'); END;`).Error
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	err = saveIdentity(db, &models.Identity{
		AgentID:      "agt_boom",
		ServerID:     2,
		SessionToken: "new-token",
		APIURL:       "https://new.example.com",
		WsURL:        "wss://new.example.com/agents/ws",
		Version:      "0.0.2",
	})
	if err == nil {
		t.Fatal("saveIdentity succeeded, want the trigger to fail the insert")
	}

	var rows []models.Identity
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("identity rows = %d, want the old row preserved by the rollback", len(rows))
	}
	if rows[0].AgentID != "agt_old" || rows[0].SessionToken != "old-token" {
		t.Fatalf("identity = %+v, want the original row", rows[0])
	}
}
