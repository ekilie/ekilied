package agent

import (
	"context"
	"fmt"
	"log"
	"os/exec"
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
	engine *JobEngine
	docker *DockerService
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(cfg *config.Config, db *gorm.DB) (*Ekilied, error) {
	ctx, cancel := context.WithCancel(context.Background())

	e := &Ekilied{
		cfg:    cfg,
		db:     db,
		ctx:    ctx,
		cancel: cancel,
	}

	// WS client with job handler callback
	e.ws = NewWSClient(cfg, func(jobCtx context.Context, jobID uint) {
		e.engine.HandleJobTrigger(jobCtx, jobID)
	})
	e.engine = NewJobEngine(e.ws)

	return e, nil
}

func (e *Ekilied) Config() *config.Config {
	return e.cfg
}

func (e *Ekilied) Register() (sessionToken, agentID string, err error) {
	return e.ws.Register(e.ctx)
}

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

	e.detectCapabilities()

	dockerSvc, err := NewDockerService(e)
	if err == nil {
		e.docker = dockerSvc
		e.ws.SetDockerService(dockerSvc)
		log.Println("docker monitoring enabled")
	} else {
		log.Println("docker monitoring disabled:", err)
	}

	e.wg.Go(func() { ; e.heartbeatLoop() })

	e.wg.Go(func() { ; e.ws.Connect(e.ctx) })

	e.wg.Go(func() { ; e.httpPollLoop() })

	if e.cfg.AutoUpdate {
		e.wg.Go(func() { ; e.updateCheckLoop() })
	} else {
		log.Println("auto-update disabled")
	}

	log.Println("ekilied running")
	return nil
}

func (e *Ekilied) updateCheckLoop() {
	interval := time.Duration(e.cfg.UpdateCheckInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	repo := "ekilie/ekilied"
	log.Printf("[update] checking for updates every %ds", e.cfg.UpdateCheckInterval)

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			release, available, err := CheckForUpdate(repo, version)
			if err != nil {
				log.Printf("[update] check failed: %v", err)
				continue
			}
			if !available {
				continue
			}
			log.Printf("[update] new version available: %s", release.TagName)
			if err := SelfUpdate(repo, release); err != nil {
				log.Printf("[update] failed: %v", err)
			}
		}
	}
}

func (e *Ekilied) Stop() {
	log.Println("stopping ekilied...")
	e.cancel()
	if e.docker != nil {
		e.docker.Close()
	}
	e.wg.Wait()
	e.db.Model(&models.Identity{}).Where("1 = 1").Update("connected", false)
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

func (e *Ekilied) httpPollLoop() {
	pollTicker := time.NewTicker(time.Duration(e.cfg.PollInterval) * time.Second)
	defer pollTicker.Stop()

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-pollTicker.C:
			jobs, err := e.ws.PollJobs(e.ctx)
			if err != nil {
				continue
			}
			for _, job := range jobs {
				log.Printf("polled job: id=%d action=%s", job.ID, job.Action)
				go e.engine.HandleJobTrigger(e.ctx, job.ID)
			}
		}
	}
}

func (e *Ekilied) detectCapabilities() {
	log.Println("detecting capabilities...")
	caps := []models.Capability{
		{Name: "nginx", Available: commandExists("nginx", "-v")},
		{Name: "node", Available: commandExists("node", "--version")},
		{Name: "npm", Available: commandExists("npm", "--version")},
		{Name: "docker", Available: commandExists("docker", "--version")},
		{Name: "certbot", Available: commandExists("certbot", "--version")},
		{Name: "git", Available: commandExists("git", "--version")},
		{Name: "systemd", Available: commandExists("systemctl", "--version")},
		{Name: "php", Available: commandExists("php", "--version")},
		{Name: "composer", Available: commandExists("composer", "--version")},
	}

	for _, cap := range caps {
		e.db.Where("name = ?", cap.Name).Delete(&models.Capability{})
		e.db.Create(&cap)
		if cap.Available {
			log.Printf("  ✓ %s", cap.Name)
		} else {
			log.Printf("  ✗ %s", cap.Name)
		}
	}
}

func commandExists(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	return cmd.Run() == nil
}
