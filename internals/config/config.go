package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ServerID          uint   `yaml:"server_id"`
	AgentID           string `yaml:"agent_id"`
	SessionToken      string `yaml:"session_token"`
	RegistrationToken string `yaml:"registration_token"`
	APIURL            string `yaml:"api_url"`
	WsURL             string `yaml:"ws_url"`
	PollInterval      int    `yaml:"poll_interval"`

	DataDir    string `yaml:"-"`
	LogDir     string `yaml:"-"`
	SocketPath string `yaml:"-"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := &Config{PollInterval: 5}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("api_url is required")
	}

	cfg.DataDir = "/opt/ekilie/agent"
	cfg.LogDir = "/var/log/ekilie"
	cfg.SocketPath = "/var/run/ekilie/agent.sock"
	return cfg, nil
}

func (c *Config) NeedsRegistration() bool { return c.RegistrationToken != "" && c.SessionToken == "" }
func (c *Config) HasSession() bool        { return c.SessionToken != "" }
EOF

# internals/agent/agent.go
cat > internals/agent/agent.go << 'AGEOF'
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
EOF

# internals/agent/websocket.go
cat > internals/agent/websocket.go << 'WSEOF'
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/ekilie/ekilied/internals/config"
)

type WSClient struct {
	cfg    *config.Config
	client *http.Client
}

func NewWSClient(cfg *config.Config) *WSClient {
	return &WSClient{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *WSClient) Register(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"server_id":     c.cfg.ServerID,
		"token":         c.cfg.RegistrationToken,
		"agent_version": "1.0.0",
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.APIURL+"/agents/register", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		return "", fmt.Errorf("registration failed: %s", resp.Status)
	}

	var result struct {
		SessionToken string `json:"session_token"`
		WsURL        string `json:"ws_url"`
		PollInterval int    `json:"poll_interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}

	if result.WsURL != "" {
		c.cfg.WsURL = result.WsURL
	}
	if result.PollInterval > 0 {
		c.cfg.PollInterval = result.PollInterval
	}

	log.Printf("registered as agent_id=%s", result.SessionToken[:12])
	return result.SessionToken, nil
}

func (c *WSClient) SendHeartbeat(ctx context.Context, agentID, sessionToken string, metrics interface{}) error {
	body, _ := json.Marshal(map[string]interface{}{
		"agent_id":  agentID,
		"ts":        time.Now().UTC(),
		"metrics":   metrics,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.cfg.APIURL+"/agents/heartbeat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("heartbeat: %s", resp.Status)
	}

	var result struct {
		PendingJobsCount int `json:"pending_jobs_count"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.PendingJobsCount > 0 {
		log.Printf("%d pending jobs", result.PendingJobsCount)
	}
	return nil
}

func (c *WSClient) PollJobs(ctx context.Context, agentID, sessionToken string) ([]interface{}, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.cfg.APIURL+"/agents/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Jobs []interface{} `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Jobs, nil
}

func (c *WSClient) Connect(ctx context.Context) {
	log.Println("WebSocket client ready (WS connection pending)")
	<-ctx.Done()
}
EOF

# internals/agent/heartbeat.go
cat > internals/agent/heartbeat.go << 'HBEOF'
package agent

import (
	"context"
	"log"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/ekilie/ekilied/internals/models"
)

var startTime = time.Now()

type HeartbeatMetrics struct {
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	DiskPercent   float64   `json:"disk_percent"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	AgentVersion  string    `json:"agent_version"`
}

func collectMetrics() HeartbeatMetrics {
	cpuP, _ := cpu.Percent(0, false)
	memV, _ := mem.VirtualMemory()
	diskV, _ := disk.Usage("/")

	cpuVal := 0.0
	if len(cpuP) > 0 {
		cpuVal = cpuP[0]
	}
	memVal := 0.0
	if memV != nil {
		memVal = memV.UsedPercent
	}
	diskVal := 0.0
	if diskV != nil {
		diskVal = diskV.UsedPercent
	}

	return HeartbeatMetrics{
		CPUPercent:    cpuVal,
		MemoryPercent: memVal,
		DiskPercent:   diskVal,
		UptimeSeconds: int64(time.Since(startTime).Seconds()),
		AgentVersion:  "1.0.0",
	}
}

func (e *Ekilied) sendHeartbeat(ctx context.Context) error {
	metrics := collectMetrics()
	log.Printf("heartbeat: cpu=%.1f%% mem=%.1f%% disk=%.1f%%",
		metrics.CPUPercent, metrics.MemoryPercent, metrics.DiskPercent)

	// Update local SQLite identity
	e.db.Model(&models.Identity{}).Where("1 = 1").Update("last_heartbeat", time.Now().Unix())

	return e.ws.SendHeartbeat(ctx, e.cfg.AgentID, e.cfg.SessionToken, metrics)
}
EOF

# internals/models/models.go
cat > internals/models/models.go << 'MODEOF'
package models

import (
	"time"

	"gorm.io/gorm"
)

type Identity struct {
	gorm.Model
	AgentID           string `gorm:"uniqueIndex;not null"`
	ServerID          uint   `gorm:"not null"`
	SessionToken      string `gorm:"not null"`
	RegistrationToken string
	APIURL            string `gorm:"not null"`
	WsURL             string `gorm:"not null"`
	PollInterval      int    `gorm:"default:5"`
	LastHeartbeat     int64
	Connected         bool   `gorm:"default:false"`
	Version           string `gorm:"not null"`
}

func (Identity) TableName() string { return "ekilied_identity" }

type Capability struct {
	gorm.Model
	AgentID     string `gorm:"index"`
	Name        string `gorm:"not null"`
	Version     string
	Available   bool `gorm:"not null"`
	LastChecked int64
}

func (Capability) TableName() string { return "ekilied_capabilities" }

type PendingJob struct {
	gorm.Model
	JobID        uint   `gorm:"uniqueIndex;not null"`
	Action       string `gorm:"not null"`
	Status       string `gorm:"not null"`
	Params       string `gorm:"type:text"`
	Retries      int    `gorm:"default:0"`
	MaxRetries   int    `gorm:"default:3"`
	LastError    string `gorm:"type:text"`
	StartedAt    int64
	CompletedAt  int64
	DeployLockID string `gorm:"index"`
}

func (PendingJob) TableName() string { return "ekilied_pending_jobs" }

type CompletedJob struct {
	gorm.Model
	JobID       uint   `gorm:"uniqueIndex;not null"`
	Action      string `gorm:"not null"`
	Status      string `gorm:"not null"`
	Params      string `gorm:"type:text"`
	Summary     string `gorm:"type:text"`
	Retries     int
	StartedAt   int64
	CompletedAt int64
}

func (CompletedJob) TableName() string { return "ekilied_completed_jobs" }

type SiteCache struct {
	gorm.Model
	SiteName      string `gorm:"uniqueIndex;not null"`
	SiteType      string `gorm:"not null"`
	Domains       string `gorm:"type:text"`
	WebDirectory  string `gorm:"default:/"`
	NginxConfig   string `gorm:"type:text"`
	DeployScript  string `gorm:"type:text"`
	ActiveRelease string
	LastDeployAt  int64
	EnvHash       string
}

func (SiteCache) TableName() string { return "ekilied_site_cache" }

type Setting struct {
	gorm.Model
	Key   string `gorm:"uniqueIndex;not null"`
	Value string `gorm:"type:text;not null"`
}

func (Setting) TableName() string { return "ekilied_settings" }

var _ = time.Time{}
EOF

echo "All files written successfully"
