package agent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/models"
	"gorm.io/gorm"
)

type Ekilied struct {
	cfg    *config.Config
	db     *gorm.DB
	ws     *WSClient
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(cfg *config.Config, db *gorm.DB) (*Ekilied, error) {
	ctx, cancel := context.WithCancel(context.Background())
	return &Ekilied{
		cfg:    cfg,
		db:     db,
		ws:     NewWSClient(cfg),
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Register performs the one-time registration handshake and returns
// the session token and agent ID. Does not persist anything.
func (e *Ekilied) Register() (sessionToken, agentID string, err error) {
	return e.ws.Register(e.ctx)
}

// RegisterAndSave performs registration and persists identity to SQLite.
func (e *Ekilied) RegisterAndSave() error {
	sessionToken, agentID, err := e.ws.Register(e.ctx)
	if err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}
	e.cfg.SessionToken = sessionToken
	e.cfg.AgentID = agentID

	e.db.Where("1 = 1").Delete(&models.Identity{})
	e.db.Create(&models.Identity{
		AgentID:      agentID,
		ServerID:     e.cfg.ServerID,
		SessionToken: sessionToken,
		APIURL:       e.cfg.APIURL,
		WsURL:        e.cfg.WsURL,
		PollInterval: e.cfg.PollInterval,
		Connected:    true,
		Version:      "1.0.0",
	})
	log.Printf("identity persisted: agent_id=%s", agentID)
	return nil
}

func (e *Ekilied) Start() error {
	log.Println("ekilied starting...")

	if e.cfg.NeedsRegistration() {
		if err := e.RegisterAndSave(); err != nil {
			return err
		}
	}

	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.heartbeatLoop() }()

	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.ws.Connect(e.ctx) }()

	log.Println("ekilied running")
	return nil
}

func (e *Ekilied) Stop() {
	log.Println("stopping ekilied...")
	e.cancel()
	e.wg.Wait()
	log.Println("ekilied stopped")
}

func (e *Ekilied) heartbeatLoop() {
	ticker := time.NewTicker(time.Duration(e.cfg.HeartbeatInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			if err := e.sendHeartbeat(e.ctx); err != nil {
				log.Printf("heartbeat failed: %v", err)
			}
		}
	}
}
