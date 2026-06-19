package agent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/models"
	"gorm.io/driver/sqlite"
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

func New(cfg *config.Config) (*Ekilied, error) {
	db, err := gorm.Open(sqlite.Open(cfg.DataDir+"/agent.db"), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.AutoMigrate(
		&models.Identity{}, &models.Capability{}, &models.PendingJob{},
		&models.CompletedJob{}, &models.SiteCache{}, &models.Setting{},
	); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Ekilied{cfg: cfg, db: db, ws: NewWSClient(cfg), ctx: ctx, cancel: cancel}, nil
}

func (e *Ekilied) Start() error {
	log.Println("ekilied starting...")
	if e.cfg.NeedsRegistration() {
		log.Println("performing registration handshake...")
		sessionToken, err := e.ws.Register(e.ctx)
		if err != nil {
			return fmt.Errorf("registration failed: %w", err)
		}
		e.cfg.SessionToken = sessionToken
		log.Println("registration successful")
	}
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.ws.Connect(e.ctx) }()
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.heartbeatLoop() }()
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
	ticker := time.NewTicker(30 * time.Second)
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
