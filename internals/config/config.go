package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	// Agent identity
	ServerID          uint   `yaml:"server_id"`
	AgentID           string `yaml:"agent_id"`
	SessionToken      string `yaml:"session_token"`
	RegistrationToken string `yaml:"registration_token"`

	// Control plane connection
	APIURL      string `yaml:"api_url"`
	WsURL       string `yaml:"ws_url"`
	PollInterval int   `yaml:"poll_interval"`

	// Database (SQLite)
	DBPath string

	// Paths
	DataDir    string
	LogDir     string
	SocketPath string
	LogLevel   string

	// Heartbeat
	HeartbeatInterval int
}

func Load(path string) (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		PollInterval:      getEnvInt("EKILIED_POLL_INTERVAL", 5),
		HeartbeatInterval: getEnvInt("EKILIED_HEARTBEAT_INTERVAL", 30),
		DBPath:            getEnv("EKILIED_DB_PATH", "/opt/ekilie/agent/agent.db"),
		DataDir:           getEnv("EKILIED_DATA_DIR", "/opt/ekilie/agent"),
		LogDir:            getEnv("EKILIED_LOG_DIR", "/var/log/ekilie"),
		SocketPath:        getEnv("EKILIED_SOCKET_PATH", "/var/run/ekilie/agent.sock"),
		LogLevel:          getEnv("EKILIED_LOG_LEVEL", "info"),
	}

	// Try loading from YAML config file if it exists
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := yamlUnmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}

	// Env vars override YAML (12-factor)
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

	if cfg.APIURL == "" {
		return nil, fmt.Errorf("api_url is required — set EKILIED_API_URL or configure agent.yml")
	}
	if cfg.WsURL == "" {
		cfg.WsURL = strings.Replace(cfg.APIURL, "https://", "wss://", 1) + "/agents/ws"
	}

	return cfg, nil
}

func (c *Config) NeedsRegistration() bool {
	return c.RegistrationToken != "" && c.SessionToken == ""
}

func (c *Config) HasSession() bool {
	return c.SessionToken != ""
}

func yamlUnmarshal(data []byte, cfg *Config) error {
	// Minimal YAML parser for config — we do this manually to avoid
	// adding a yaml dependency. The agent.yml is simple key-value.
	for _, line := range strings.Split(string(data), "\n") {
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
		val = strings.Trim(val, `"'`)

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
		}
	}
	return nil
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}
