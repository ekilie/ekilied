package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// Version and Commit are set at build time via -ldflags.
var (
	Version = "0.1.0"
	Commit  = "dev"
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
	AutoUpdate        *bool
	UpdateInterval    int
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
				cfg.DBPath = cfg.DataDir + "/ekilied.db"
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
		if overrides.AutoUpdate != nil {
			cfg.AutoUpdate = *overrides.AutoUpdate
			cfg.AutoUpdateSource = "flag"
		}
		if overrides.UpdateInterval > 0 {
			cfg.UpdateCheckInterval = overrides.UpdateInterval
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
	APIURL            string `yaml:"api_url"`
	WsURL             string `yaml:"ws_url"`
	PollInterval      int    `yaml:"poll_interval"`
	HeartbeatInterval int    `yaml:"heartbeat_interval"`

	// Storage
	DBPath     string `yaml:"db_path"`
	DataDir    string `yaml:"data_dir"`
	LogDir     string `yaml:"log_dir"`
	SocketPath string `yaml:"socket_path"`

	// Runtime
	LogLevel string `yaml:"log_level"`

	// Auto-update
	AutoUpdate          bool `yaml:"auto_update"`
	UpdateCheckInterval int  `yaml:"update_check_interval"`

	// AutoUpdateSource records where AutoUpdate came from: "default",
	// "config file", "flag", or "environment". It is informational (logged at
	// startup) and never written to the config file.
	AutoUpdateSource string `yaml:"-"`
}

func Defaults() *Config {
	return &Config{
		PollInterval:        1,
		HeartbeatInterval:   30,
		DBPath:              "/opt/ekilie/agent/agent.db",
		DataDir:             "/opt/ekilie/agent",
		LogDir:              "/var/log/ekilie",
		SocketPath:          "/var/run/ekilie/agent.sock",
		LogLevel:            "info",
		AutoUpdate:          true,
		UpdateCheckInterval: 86400,
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
	if c.UpdateCheckInterval == 0 {
		c.UpdateCheckInterval = d.UpdateCheckInterval
	}
	if c.AutoUpdateSource == "" {
		c.AutoUpdateSource = "default"
	}
}

// Load reads config from a YAML file, then applies env vars and options.
// The config file is optional: all settings can come from flags or env vars.
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
		return nil, fmt.Errorf("api_url is required: set --api-url, EKILIED_API_URL, or add api_url to agent.yml")
	}

	return cfg, nil
}

func (c *Config) NeedsRegistration() bool {
	return c.RegistrationToken != "" && c.SessionToken == ""
}

func (c *Config) HasSession() bool {
	return c.SessionToken != ""
}

// SaveSession writes agent_id and session_token back to the YAML config file
// after an in-process registration, so the agent can reconnect after a
// restart. All other keys, comments, and blank lines are preserved
// byte-for-byte; only the two credential lines are replaced (or appended
// when missing). The write is atomic (temp file + rename) and the file mode
// is forced to 0600 since it now holds credentials.
func SaveSession(path, agentID, sessionToken string) error {
	if path == "" {
		return fmt.Errorf("config path is empty")
	}
	if agentID == "" || sessionToken == "" {
		return fmt.Errorf("agent_id and session_token are required")
	}

	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(data), "\n")
	}

	seenAgent, seenSession := false, false
	out := make([]string, 0, len(lines)+2)
	for _, line := range lines {
		switch flatKeyOf(line) {
		case "agent_id":
			out = append(out, "agent_id: "+yamlScalar(agentID))
			seenAgent = true
		case "session_token":
			out = append(out, "session_token: "+yamlScalar(sessionToken))
			seenSession = true
		default:
			out = append(out, line)
		}
	}
	if !seenAgent {
		out = append(out, "agent_id: "+yamlScalar(agentID))
	}
	if !seenSession {
		out = append(out, "session_token: "+yamlScalar(sessionToken))
	}
	content := strings.Join(out, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent.yml.*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	// Best effort cleanup if anything below fails; on success the temp file
	// is renamed away and Remove is a no-op error we ignore.
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// flatKeyOf returns the mapping key of a top-level "key: value" YAML line,
// or "" for blank lines, comments, and indented (nested) lines.
func flatKeyOf(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
		return ""
	}
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

// yamlScalar renders a plain string as a YAML scalar, quoting it when it
// contains characters that would otherwise change its meaning.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, "#:\"' \t\n\r") || strings.HasPrefix(s, "-") {
		return strconv.Quote(s)
	}
	return s
}

// ── YAML parser (gopkg.in/yaml.v3) ───────────────────────────────────────

// yamlConfig mirrors the agent.yml keys with pointers so a missing key can be
// told apart from a key set to a zero value. Unknown keys are rejected by the
// decoder, and type errors and unknown fields are reported with line numbers.
type yamlConfig struct {
	ServerID          *uint   `yaml:"server_id"`
	AgentID           *string `yaml:"agent_id"`
	SessionToken      *string `yaml:"session_token"`
	RegistrationToken *string `yaml:"registration_token"`

	APIURL            *string `yaml:"api_url"`
	WsURL             *string `yaml:"ws_url"`
	PollInterval      *int    `yaml:"poll_interval"`
	HeartbeatInterval *int    `yaml:"heartbeat_interval"`

	DBPath     *string `yaml:"db_path"`
	DataDir    *string `yaml:"data_dir"`
	LogDir     *string `yaml:"log_dir"`
	SocketPath *string `yaml:"socket_path"`
	LogLevel   *string `yaml:"log_level"`

	AutoUpdate          *bool `yaml:"auto_update"`
	UpdateCheckInterval *int  `yaml:"update_check_interval"`
}

// parseYAML decodes agent.yml on top of the defaults already present in cfg.
// Values with inline comments, quoting, or colons parse the way YAML says they
// should, and a malformed file fails instead of silently defaulting.
func parseYAML(data string, cfg *Config) error {
	dec := yaml.NewDecoder(strings.NewReader(data))
	dec.KnownFields(true)

	var yc yamlConfig
	if err := dec.Decode(&yc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty file, keep defaults
		}
		return decorateYAMLError(err, data)
	}
	applyYAML(cfg, yc)
	return nil
}

// yamlLineRe extracts the line number yaml.v3 puts in its error messages.
var yamlLineRe = regexp.MustCompile(`line (\d+):`)

// decorateYAMLError appends the offending source line to a yaml.v3 error.
// The library reports line numbers but not the key text, and an operator
// fixing a typo needs to see which line is wrong.
func decorateYAMLError(err error, data string) error {
	m := yamlLineRe.FindStringSubmatch(err.Error())
	if len(m) != 2 {
		return err
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return err
	}
	lines := strings.Split(data, "\n")
	if n < 1 || n > len(lines) {
		return err
	}
	return fmt.Errorf("%w (offending line: %q)", err, strings.TrimSpace(lines[n-1]))
}

func applyYAML(cfg *Config, yc yamlConfig) {
	if yc.ServerID != nil {
		cfg.ServerID = *yc.ServerID
	}
	if yc.AgentID != nil {
		cfg.AgentID = *yc.AgentID
	}
	if yc.SessionToken != nil {
		cfg.SessionToken = *yc.SessionToken
	}
	if yc.RegistrationToken != nil {
		cfg.RegistrationToken = *yc.RegistrationToken
	}
	if yc.APIURL != nil {
		cfg.APIURL = *yc.APIURL
	}
	if yc.WsURL != nil {
		cfg.WsURL = *yc.WsURL
	}
	if yc.PollInterval != nil {
		cfg.PollInterval = *yc.PollInterval
	}
	if yc.HeartbeatInterval != nil {
		cfg.HeartbeatInterval = *yc.HeartbeatInterval
	}
	if yc.DBPath != nil {
		cfg.DBPath = *yc.DBPath
	}
	if yc.DataDir != nil {
		cfg.DataDir = *yc.DataDir
	}
	if yc.LogDir != nil {
		cfg.LogDir = *yc.LogDir
	}
	if yc.SocketPath != nil {
		cfg.SocketPath = *yc.SocketPath
	}
	if yc.LogLevel != nil {
		cfg.LogLevel = *yc.LogLevel
	}
	if yc.AutoUpdate != nil {
		cfg.AutoUpdate = *yc.AutoUpdate
		cfg.AutoUpdateSource = "config file"
	}
	if yc.UpdateCheckInterval != nil {
		cfg.UpdateCheckInterval = *yc.UpdateCheckInterval
	}
}

// SetupConfig is the minimal configuration written by `ekilied --setup`. It
// marshals with yaml.v3, so tokens and URLs containing special characters are
// quoted correctly instead of producing an unparseable file.
type SetupConfig struct {
	ServerID     uint   `yaml:"server_id"`
	AgentID      string `yaml:"agent_id"`
	SessionToken string `yaml:"session_token"`
	APIURL       string `yaml:"api_url"`
	WsURL        string `yaml:"ws_url"`
	PollInterval int    `yaml:"poll_interval"`
}

// MarshalSetup renders the setup configuration as YAML.
func MarshalSetup(cfg SetupConfig) ([]byte, error) {
	return yaml.Marshal(cfg)
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
	if v := os.Getenv("EKILIED_AUTO_UPDATE"); v != "" {
		cfg.AutoUpdate = v == "true" || v == "1" || v == "yes"
		cfg.AutoUpdateSource = "environment"
	}
	if v := os.Getenv("EKILIED_UPDATE_INTERVAL"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.UpdateCheckInterval = i
		}
	}
}
