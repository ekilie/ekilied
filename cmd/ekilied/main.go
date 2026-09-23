package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/ekilie/ekilied/internals/agent"
	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/jobengine"
	"github.com/ekilie/ekilied/internals/models"
	"github.com/ekilie/ekilied/pkg/database"
)

type Flags struct {
	ConfigPath        string
	APIURL            string
	WsURL             string
	Token             string
	ServerID          uint
	DBPath            string
	DataDir           string
	LogLevel          string
	PollInterval      int
	HeartbeatInterval int
	AutoUpdate        optionalBool
	UpdateInterval    int
	Setup             bool
	UpdateFlag        bool
	VersionFlag       bool
	HelpFlag          bool
}

// optionalBool is a tri-state boolean flag: it remembers whether the flag was
// explicitly provided. Layered config must only override a file value when the
// operator asked for it, and flag.BoolVar cannot express "not set" because it
// always yields its default. Use flag.Var with this type for any future
// boolean that can also come from agent.yml or the environment.
type optionalBool struct {
	set   bool
	value bool
}

func (b *optionalBool) String() string {
	if !b.set {
		return ""
	}
	return strconv.FormatBool(b.value)
}

func (b *optionalBool) Set(v string) error {
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("invalid boolean %q", v)
	}
	b.set = true
	b.value = parsed
	return nil
}

// IsBoolFlag lets `--auto-update` work without an explicit value, while
// `--auto-update=false` still parses.
func (b *optionalBool) IsBoolFlag() bool { return true }

// autoUpdateOverride returns the value to pass into config.Load, or nil when
// the flag was not explicitly set so the config file and environment decide.
func autoUpdateOverride(f Flags) *bool {
	if !f.AutoUpdate.set {
		return nil
	}
	v := f.AutoUpdate.value
	return &v
}

func parseFlags() Flags {
	var f Flags

	flag.StringVar(&f.ConfigPath, "config", "/etc/ekilie/agent.yml", "path to config file")
	flag.StringVar(&f.ConfigPath, "c", "/etc/ekilie/agent.yml", "path to config file (shorthand)")

	flag.StringVar(&f.APIURL, "api-url", "", "control plane API URL (e.g. https://engine.ekilie.cloud)")
	flag.StringVar(&f.APIURL, "a", "", "control plane API URL (shorthand)")

	flag.StringVar(&f.WsURL, "ws-url", "", "WebSocket URL (defaults to api-url + /agents/ws)")

	flag.StringVar(&f.Token, "token", "", "registration or session token")
	flag.StringVar(&f.Token, "t", "", "registration or session token (shorthand)")

	flag.UintVar(&f.ServerID, "server-id", 0, "server / instance ID")
	flag.UintVar(&f.ServerID, "s", 0, "server / instance ID (shorthand)")

	flag.StringVar(&f.DBPath, "db-path", "", "path to SQLite database (default: <data-dir>/agent.db)")
	flag.StringVar(&f.DataDir, "data-dir", "", "data directory (default: /opt/ekilie/agent)")
	flag.StringVar(&f.LogLevel, "log-level", "", "log level: debug, info, warn, error")

	flag.IntVar(&f.PollInterval, "poll-interval", 0, "job poll interval in seconds (default: 5)")
	flag.IntVar(&f.HeartbeatInterval, "heartbeat-interval", 0, "heartbeat interval in seconds (default: 30)")

	flag.Var(&f.AutoUpdate, "auto-update", "enable automatic self-update (overrides auto_update from the config file only when passed)")
	flag.IntVar(&f.UpdateInterval, "update-interval", 86400, "update check interval in seconds (default: 86400 = 24h)")

	flag.BoolVar(&f.Setup, "setup", false, "run setup mode: register with token and write config")
	flag.BoolVar(&f.VersionFlag, "version", false, "print version and exit")
	flag.BoolVar(&f.VersionFlag, "v", false, "print version and exit (shorthand)")
	flag.BoolVar(&f.UpdateFlag, "update", false, "check for update and replace binary")
	flag.BoolVar(&f.HelpFlag, "help", false, "print usage and exit")
	flag.BoolVar(&f.HelpFlag, "h", false, "print usage and exit (shorthand)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `ekilied %s (%s) => Ekilie Cloud Platform Agent

Usage:
  ekilied [flags]                    Start the agent daemon
  ekilied --setup [flags]           Run one-time registration setup
  ekilied --update                  Check for update and replace binary
  ekilied --version                 Print version and exit

Flags:
`, config.Version, config.Commit)
		flag.PrintDefaults()
	}

	flag.Parse()
	return f
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	f := parseFlags()

	if f.HelpFlag {
		flag.Usage()
		os.Exit(0)
	}

	if f.VersionFlag {
		fmt.Printf("ekilied %s (%s)\n", config.Version, config.Commit)
		os.Exit(0)
	}

	if f.Setup {
		if err := runSetup(f); err != nil {
			log.Fatalf("setup failed: %v", err)
		}
		os.Exit(0)
	}

	if f.UpdateFlag {
		repo := "ekilie/ekilied"
		log.Printf("[update] checking for updates (current: %s)...", config.Version)
		release, available, err := jobengine.CheckForUpdate(repo, config.Version)
		if err != nil {
			log.Fatalf("[update] check failed: %v", err)
		}
		if !available {
			log.Printf("[update] already up to date (%s)", config.Version)
			os.Exit(0)
		}
		log.Printf("[update] found %s, downloading...", release.TagName)
		if err := jobengine.SelfUpdate(repo, release); err != nil {
			log.Fatalf("[update] failed: %v", err)
		}
		log.Printf("[update] updated to %s — restart the agent to apply", release.TagName)
		fmt.Printf("Updated to %s. Restart the agent: systemctl restart ekilied\n", release.TagName)
		os.Exit(0)
	}

	log.Printf("ekilied %s (%s) starting", config.Version, config.Commit)

	cfg, err := config.Load(f.ConfigPath, config.WithFlags(config.FlagOverrides{
		APIURL:            f.APIURL,
		WsURL:             f.WsURL,
		Token:             f.Token,
		ServerID:          f.ServerID,
		DBPath:            f.DBPath,
		DataDir:           f.DataDir,
		LogLevel:          f.LogLevel,
		PollInterval:      f.PollInterval,
		HeartbeatInterval: f.HeartbeatInterval,
		AutoUpdate:        autoUpdateOverride(f),
		UpdateInterval:    f.UpdateInterval,
	}))
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	autoUpdateState := "disabled"
	if cfg.AutoUpdate {
		autoUpdateState = "enabled"
	}
	log.Printf("auto-update %s (source: %s)", autoUpdateState, cfg.AutoUpdateSource)

	dbCfg := database.DefaultConfig(cfg.DBPath)
	if err := database.Connect(dbCfg); err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer database.Close()

	if err := database.AutoMigrate(models.AllModels()...); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	// Heal agents whose registration succeeded but was never written back to
	// the config file: adopt the saved session instead of attempting a doomed
	// re-registration with an already-burned token.
	if !cfg.HasSession() {
		if err := restoreSessionFromDB(cfg); err != nil {
			log.Printf("no saved session in local db: %v", err)
		} else {
			log.Printf("resumed saved session from local db: agent_id=%s", cfg.AgentID)
		}
	}

	if cfg.NeedsRegistration() {
		log.Println("performing one-time registration handshake...")
		tmp, err := agent.New(cfg, database.GetDB())
		if err != nil {
			log.Fatalf("failed to initialize: %v", err)
		}
		if err := tmp.RegisterAndSave(); err != nil {
			log.Fatalf("registration failed: %v", err)
		}
		// Update cfg with the session token from registration
		cfg.SessionToken = tmp.Config().SessionToken
		cfg.AgentID = tmp.Config().AgentID
		log.Printf("registration complete: agent_id=%s", cfg.AgentID)
		// Persist the session so the agent reconnects after a restart.
		// A failure here is fatal: continuing would strand the agent on
		// the next restart once the registration token is burned.
		if err := config.SaveSession(f.ConfigPath, cfg.AgentID, cfg.SessionToken); err != nil {
			log.Fatalf("failed to persist session to %s: %v", f.ConfigPath, err)
		}
		tmp.Stop()
	}

	e, err := agent.New(cfg, database.GetDB())
	if err != nil {
		log.Fatalf("failed to initialize: %v", err)
	}

	if err := e.Start(); err != nil {
		log.Fatalf("failed to start: %v", err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down...")
	e.Stop()
}

// restoreSessionFromDB adopts the most recently saved identity (agent_id +
// session_token, plus server_id when unset) into cfg. It heals agents whose
// registration succeeded but was never persisted to the config file.
func restoreSessionFromDB(cfg *config.Config) error {
	var ident models.Identity
	if err := database.GetDB().Order("id desc").First(&ident).Error; err != nil {
		return fmt.Errorf("load identity: %w", err)
	}
	if ident.SessionToken == "" {
		return fmt.Errorf("saved identity has no session token")
	}
	cfg.AgentID = ident.AgentID
	cfg.SessionToken = ident.SessionToken
	if cfg.ServerID == 0 {
		cfg.ServerID = ident.ServerID
	}
	return nil
}

func runSetup(f Flags) error {
	if f.Token == "" {
		return fmt.Errorf("--token is required for setup")
	}
	if f.APIURL == "" {
		return fmt.Errorf("--api-url is required for setup")
	}
	if f.ServerID == 0 {
		return fmt.Errorf("--server-id is required for setup")
	}

	cfg := &config.Config{
		ServerID:          f.ServerID,
		RegistrationToken: f.Token,
		APIURL:            f.APIURL,
		WsURL:             f.WsURL,
		PollInterval:      f.PollInterval,
		HeartbeatInterval: f.HeartbeatInterval,
		DBPath:            f.DBPath,
		DataDir:           f.DataDir,
		LogLevel:          f.LogLevel,
	}
	cfg.SetDefaults()

	dbCfg := database.DefaultConfig(cfg.DBPath)
	if err := database.Connect(dbCfg); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer database.Close()
	database.AutoMigrate(models.AllModels()...)

	e, err := agent.New(cfg, database.GetDB())
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}

	sessionToken, agentID, err := e.Register()
	if err != nil {
		return fmt.Errorf("registration: %w", err)
	}

	configDir := filepath.Dir(f.ConfigPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	yml := fmt.Sprintf(`server_id: %d
agent_id: %s
session_token: %s
api_url: %s
ws_url: %s
poll_interval: %d
`, cfg.ServerID, agentID, sessionToken, cfg.APIURL, cfg.WsURL, cfg.PollInterval)

	if err := os.WriteFile(f.ConfigPath, []byte(yml), 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Printf("setup complete — config written to %s\n", f.ConfigPath)
	fmt.Printf("  agent_id:     %s\n", agentID)
	fmt.Printf("  api_url:      %s\n", cfg.APIURL)
	fmt.Printf("  ws_url:       %s\n", cfg.WsURL)
	fmt.Printf("  poll_interval: %ds\n", cfg.PollInterval)
	return nil
}
