package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/dtos"
	"github.com/ekilie/ekilied/internals/jobengine"
	"github.com/ekilie/ekilied/internals/models"
	"gorm.io/gorm"
)

type Ekilied struct {
	cfg    *config.Config
	db     *gorm.DB
	ws     *WSClient
	engine *jobengine.JobEngine
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

	e.ws = NewWSClient(cfg, ctx, func(jobCtx context.Context, jobID uint) {
		e.engine.HandleJobTrigger(jobCtx, jobID)
	}, func(jobCtx context.Context, jobID uint, action string, params map[string]any) {
		e.engine.HandleJobTriggerFull(jobCtx, jobID, action, params)
	})
	e.engine = jobengine.NewJobEngine(e.ws)

	return e, nil
}

func (e *Ekilied) Config() *config.Config {
	return e.cfg
}

func (e *Ekilied) Register() (sessionToken, agentID string, err error) {
	return e.ws.Register(e.ctx, e.capabilityDTOs())
}

func (e *Ekilied) capabilityDTOs() []dtos.Capability {
	return []dtos.Capability{
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
}

// saveIdentity atomically replaces the stored identity with ident. The
// delete and insert run in one transaction, so a crash or write error can
// never leave the agent with no usable identity row.
func saveIdentity(db *gorm.DB, ident *models.Identity) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("1 = 1").Delete(&models.Identity{}).Error; err != nil {
			return fmt.Errorf("clear identity: %w", err)
		}
		if err := tx.Create(ident).Error; err != nil {
			return fmt.Errorf("create identity: %w", err)
		}
		return nil
	})
}

// RegisterAndSave performs the one-time registration handshake and persists
// the resulting identity. It is safe to call before Start.
func (e *Ekilied) RegisterAndSave() error {
	sessionToken, agentID, err := e.ws.Register(e.ctx, e.capabilityDTOs())
	if err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}
	e.cfg.SessionToken = sessionToken
	e.cfg.AgentID = agentID

	ident := &models.Identity{
		AgentID:      agentID,
		ServerID:     e.cfg.ServerID,
		SessionToken: sessionToken,
		APIURL:       e.cfg.APIURL,
		WsURL:        e.cfg.WsURL,
		PollInterval: e.cfg.PollInterval,
		Connected:    true,
		Version:      config.Version,
	}
	if err := saveIdentity(e.db, ident); err != nil {
		return fmt.Errorf("persist identity: %w", err)
	}
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

// Update collaborators exist as vars so tests can stub the network check,
// the binary swap, and the systemd/exec restart.
var (
	checkForUpdateFunc = jobengine.CheckForUpdate
	selfUpdateFunc     = jobengine.SelfUpdate
	restartAgentFunc   = jobengine.RestartAgent
)

// updateCheckLoop checks for a new release immediately on startup, then every
// update_check_interval. It returns after an update was applied and the
// restart was initiated (or when the context is cancelled).
func (e *Ekilied) updateCheckLoop() {
	interval := time.Duration(e.cfg.UpdateCheckInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	repo := "ekilie/ekilied"
	log.Printf("[update] checking for updates every %ds", e.cfg.UpdateCheckInterval)

	for {
		if e.checkAndUpdate(repo) {
			return
		}
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// checkAndUpdate runs one update check/apply cycle and reports whether the
// loop should stop because the agent is restarting into a new binary.
func (e *Ekilied) checkAndUpdate(repo string) bool {
	release, available, err := checkForUpdateFunc(repo, config.Version)
	if err != nil {
		log.Printf("[update] check failed: %v", err)
		return false
	}
	if !available {
		log.Printf("[update] already up to date (%s)", config.Version)
		return false
	}
	log.Printf("[update] new version available: %s", release.TagName)
	if err := selfUpdateFunc(repo, release); err != nil {
		if errors.Is(err, jobengine.ErrUpdateInProgress) {
			log.Printf("[update] another update is already in progress, skipping")
			return false
		}
		log.Printf("[update] failed: %v", err)
		return false
	}
	log.Printf("[update] updated, restarting...")
	if err := restartAgentFunc(e.ctx); err != nil {
		log.Printf("[update] restart failed: %v (old binary still running, will retry on next check)", err)
		return false
	}
	return true
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

// pollInterval is the configured job poll cadence, used whether or not the
// WebSocket is connected. WS triggers are real time; polling is the catch-up
// channel, so a single interval keeps the poll_interval knob predictable.
func (e *Ekilied) pollInterval() time.Duration {
	return time.Duration(e.cfg.PollInterval) * time.Second
}

func (e *Ekilied) httpPollLoop() {
	interval := e.pollInterval()
	for {
		timer := time.NewTimer(interval)
		select {
		case <-e.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		jobs, err := e.ws.PollJobs(e.ctx)
		if err != nil {
			continue
		}
		for _, job := range jobs {
			if e.engine.IsDispatched(job.ID) {
				log.Printf("polled job %d already dispatched via WS, skipping", job.ID)
				continue
			}
			log.Printf("polled job: id=%d action=%s", job.ID, job.Action)
			go e.engine.HandleJobTrigger(e.ctx, job.ID)
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
