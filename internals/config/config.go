package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type FlagOverrides struct {
	APIURL            string
	WsURL             string
	Token             string
	ServerID          uint
	DBPath            string
	DataDir           string
	LogLevel          string
	PollInterval      int
	HeartbeatInterval int
}

type ConfigOption func(*Config)

func WithFlags(overrides FlagOverrides) ConfigOption {
	return func(cfg *Config) {
		if overrides.APIURL != "" {
			cfg.APIURL = overrides.APIURL
		}
		if overrides.WsURL != "" {
			cfg.WsURL = overrides.WsURL
		}
		if overrides.Token != "" {
			// If it looks like a session token, set it; otherwise treat as registration token
			if strings.HasPrefix(overrides.Token, "ek_session_") {
				cfg.SessionToken = overrides.Token
			} else {
				cfg.RegistrationToken = overrides.Token
			}
		}
		if overrides.ServerID > 0 {
			cfg.ServerID = overrides.ServerID
		}
		if overrides.DBPath != "" {
			cfg.DBPath = overrides.DBPath
		}
		if overrides.DataDir != "" {
			cfg.DataDir = overrides.DataDir
			if overrides.DBPath == "" {
				cfg.DBPath = cfg.DataDir + "/agent.db"
			}
		}
		if overrides.LogLevel != "" {
			cfg.LogLevel = overrides.LogLevel
		}
		if overrides.PollInterval > 0 {
			cfg.PollInterval = overrides.PollInterval
		}
		if overrides.HeartbeatInterval > 0 {
			cfg.HeartbeatInterval = overrides.HeartbeatInterval
		}
	}
}

type Config struct {
	// Agent identity
	ServerID          uint   `yaml:"server_id"`
	AgentID           string `yaml:"agent_id"`
	SessionToken      string `yaml:"session_token"`
	RegistrationToken string `yaml:"registration_token"`

	// Control plane connection
	APIURL           string `yaml:"api_url"`
	WsURL            string `yaml:"ws_url"`
	PollInterval     int    `yaml:"poll_interval"`
	HeartbeatInterval int   `yaml:"heartbeat_interval"`

	// Storage
	DBPath   string
	DataDir  string
	LogDir   string
	SocketPath string

	// Runtime
	LogLevel string
}

func Defaults() *Config {
	return &Config{
		PollInterval:      5,
		HeartbeatInterval: 30,
		DBPath:            "/opt/ekilie/agent/agent.db",
		DataDir:           "/opt/ekilie/agent",
		LogDir:            "/var/log/ekilie",
		SocketPath:        "/var/run/ekilie/agent.sock",
		LogLevel:          "info",
	}
}

func (c *Config) SetDefaults() {
	d := Defaults()
	if c.PollInterval == 0 {
		c.PollInterval = d.PollInterval
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = d.HeartbeatInterval
	}
	if c.DBPath == "" {
		c.DBPath = d.DBPath
	}
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if c.LogDir == "" {
		c.LogDir = d.LogDir
	}
	if c.SocketPath == "" {
		c.SocketPath = d.SocketPath
	}
	if c.LogLevel == "" {
		c.LogLevel = d.LogLevel
	}
}

// Load reads config from a YAML file, then applies env vars and options.
// The config file is optional — all settings can come from flags or env vars.
func Load(path string, opts ...ConfigOption) (*Config, error) {
	_ = godotenv.Load()

	cfg := Defaults()

	// 1. Load from YAML file if it exists
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := parseYAML(string(data), cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		}
	}

	// 2. Apply functional options (from flags)
	for _, opt := range opts {
		opt(cfg)
	}

	// 3. Environment variables override everything (12-factor)
	applyEnvOverrides(cfg)

	// 4. Fill defaults for any zero values
	cfg.SetDefaults()

	// 5. Infer WsURL from APIURL if not set
	if cfg.WsURL == "" && cfg.APIURL != "" {
		cfg.WsURL = strings.Replace(cfg.APIURL, "https://", "wss://", 1)
		if !strings.HasPrefix(cfg.WsURL, "wss://") && !strings.HasPrefix(cfg.WsURL, "ws://") {
			cfg.WsURL = "wss://" + cfg.APIURL
		}
		cfg.WsURL += "/agents/ws"
	}

	// 6. Validate
	if cfg.APIURL == "" && !cfg.HasSession() {
		return nil, fmt.Errorf("api_url is required — set --api-url, EKILIED_API_URL, or add api_url to agent.yml")
	}

	return cfg, nil
}

func (c *Config) NeedsRegistration() bool {
	return c.RegistrationToken != "" && c.SessionToken == ""
}

func (c *Config) HasSession() bool {
	return c.SessionToken != ""
}

// ── YAML parser (no dependency) ──────────────────────────────────────────

func parseYAML(data string, cfg *Config) error {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"' `)

		switch key {
		case "server_id":
			id, _ := strconv.ParseUint(val, 10, 64)
			cfg.ServerID = uint(id)
		case "agent_id":
			cfg.AgentID = val
		case "session_token":
			cfg.SessionToken = val
		case "registration_token":
			cfg.RegistrationToken = val
		case "api_url":
			cfg.APIURL = val
		case "ws_url":
			cfg.WsURL = val
		case "poll_interval":
			cfg.PollInterval, _ = strconv.Atoi(val)
		case "heartbeat_interval":
			cfg.HeartbeatInterval, _ = strconv.Atoi(val)
		}
	}
	return nil
}

// ── Environment variable overrides ───────────────────────────────────────

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("EKILIED_API_URL"); v != "" {
		cfg.APIURL = v
	}
	if v := os.Getenv("EKILIED_WS_URL"); v != "" {
		cfg.WsURL = v
	}
	if v := os.Getenv("EKILIED_SERVER_ID"); v != "" {
		id, _ := strconv.ParseUint(v, 10, 64)
		cfg.ServerID = uint(id)
	}
	if v := os.Getenv("EKILIED_SESSION_TOKEN"); v != "" {
		cfg.SessionToken = v
	}
	if v := os.Getenv("EKILIED_REGISTRATION_TOKEN"); v != "" {
		cfg.RegistrationToken = v
	}
	if v := os.Getenv("EKILIED_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("EKILIED_DATA_DIR"); v != "" {
		cfg.DataDir = v
		if os.Getenv("EKILIED_DB_PATH") == "" {
			cfg.DBPath = v + "/agent.db"
		}
	}
	if v := os.Getenv("EKILIED_LOG_DIR"); v != "" {
		cfg.LogDir = v
	}
	if v := os.Getenv("EKILIED_SOCKET_PATH"); v != "" {
		cfg.SocketPath = v
	}
	if v := os.Getenv("EKILIED_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("EKILIED_POLL_INTERVAL"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.PollInterval = i
		}
	}
	if v := os.Getenv("EKILIED_HEARTBEAT_INTERVAL"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.HeartbeatInterval = i
		}
	}
}
