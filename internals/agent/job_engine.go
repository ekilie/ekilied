package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/dtos"
)

// ── Deploy lock (per-site) ──────────────────────────────────────────

type DeployLock struct {
	mu     sync.Mutex
	active map[string]uint
}

func NewDeployLock() *DeployLock {
	return &DeployLock{active: make(map[string]uint)}
}

func (dl *DeployLock) TryAcquire(siteName string, jobID uint) bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if _, exists := dl.active[siteName]; exists {
		return false
	}
	dl.active[siteName] = jobID
	return true
}

func (dl *DeployLock) Release(siteName string) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	delete(dl.active, siteName)
}

// ── Log batcher ─────────────────────────────────────────────────────

type LogBatcher struct {
	mu     sync.Mutex
	lines  []dtos.LogLine
	jobID  uint
	client *WSClient
	ctx    context.Context
	cancel context.CancelFunc
}

func NewLogBatcher(ctx context.Context, jobID uint, client *WSClient) *LogBatcher {
	ctx, cancel := context.WithCancel(ctx)
	lb := &LogBatcher{
		lines:  make([]dtos.LogLine, 0, 100),
		jobID:  jobID,
		client: client,
		ctx:    ctx,
		cancel: cancel,
	}
	go lb.flushLoop()
	return lb
}

func (lb *LogBatcher) Write(p []byte) (int, error) {
	return lb.append("stdout", p)
}

func (lb *LogBatcher) WriteErr(p []byte) (int, error) {
	return lb.append("stderr", p)
}

func (lb *LogBatcher) append(stream string, p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for _, line := range splitLines(string(p)) {
		if line == "" {
			continue
		}
		lb.lines = append(lb.lines, dtos.LogLine{
			Stream:   stream,
			Line:     line,
			TS:       time.Now().UTC().Format(time.RFC3339),
			Sequence: len(lb.lines) + 1,
		})
	}
	return len(p), nil
}

func (lb *LogBatcher) flushLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-lb.ctx.Done():
			lb.flushNow()
			return
		case <-ticker.C:
			lb.flushNow()
		}
	}
}

func (lb *LogBatcher) flushNow() {
	lb.mu.Lock()
	if len(lb.lines) == 0 {
		lb.mu.Unlock()
		return
	}
	batch := lb.lines
	lb.lines = nil
	lb.mu.Unlock()

	if err := lb.client.StreamLogs(lb.ctx, lb.jobID, batch); err != nil {
		log.Printf("log flush error: %v", err)
	}
}

func (lb *LogBatcher) Close() {
	lb.cancel()
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// ── Job engine ──────────────────────────────────────────────────────

type JobEngine struct {
	client     *WSClient
	deployLk   *DeployLock
	active     sync.Map // map[uint]struct{} — tracks actively executing job IDs
	dispatched sync.Map // map[uint]struct{} — tracks jobs already WS-triggered so poll skips them
}

func (e *JobEngine) IsDispatched(jobID uint) bool {
	_, loaded := e.dispatched.Load(jobID)
	return loaded
}

func (e *JobEngine) markDispatched(jobID uint) bool {
	_, loaded := e.dispatched.LoadOrStore(jobID, struct{}{})
	return !loaded // true if this goroutine was first to mark it
}

func (e *JobEngine) clearDispatched(jobID uint) {
	e.dispatched.Delete(jobID)
}

func NewJobEngine(client *WSClient) *JobEngine {
	return &JobEngine{
		client:   client,
		deployLk: NewDeployLock(),
	}
}

func (e *JobEngine) HandleJobTrigger(ctx context.Context, jobID uint) {
	// Mark as dispatched so the poll loop won't also pick it up.
	// If already dispatched (race between WS and poll), skip entirely.
	if !e.markDispatched(jobID) {
		log.Printf("job %d already dispatched, skipping duplicate trigger", jobID)
		return
	}

	// Atomically claim the job (marks as accepted on backend) and get full details.
	// Single HTTP call — backend is the source of truth.
	job, err := e.client.ClaimJob(ctx, jobID)
	if err != nil {
		log.Printf("handle job trigger %d: claim failed: %v", jobID, err)
		e.clearDispatched(jobID)
		return
	}

	raw, _ := json.Marshal(job.Params)
	e.Execute(ctx, job.ID, job.Action, raw)
}

func (e *JobEngine) Execute(ctx context.Context, jobID uint, action string, rawParams json.RawMessage) {
	log.Printf("executing job %d: action=%s", jobID, action)

	// Dedup: skip if this job is already being executed
	if _, loaded := e.active.LoadOrStore(jobID, struct{}{}); loaded {
		log.Printf("job %d already executing, skipping duplicate", jobID)
		return
	}
	defer func() {
		e.active.Delete(jobID)
		e.clearDispatched(jobID)
	}()

	// Job is already claimed (accepted) by HandleJobTrigger → ClaimJob.
	// The backend status is the source of truth.

	var params map[string]any
	json.Unmarshal(rawParams, &params)

	siteName, _ := params["site_name"].(string)

	lb := NewLogBatcher(ctx, jobID, e.client)
	defer lb.Close()

	writeLog := func(format string, args ...any) {
		lb.mu.Lock()
		lb.lines = append(lb.lines, dtos.LogLine{
			Stream:   "stdout",
			Line:     fmt.Sprintf(format, args...),
			TS:       time.Now().UTC().Format(time.RFC3339),
			Sequence: len(lb.lines) + 1,
		})
		lb.mu.Unlock()
	}

	var execErr error

	switch action {
	case "site_create":
		writeLog("[site] creating site %s...", siteName)
		execErr = createSiteDir(ctx, siteName)

	case "site_delete":
		writeLog("[site] deleting site %s...", siteName)
		cleanupSupervisorForSite(siteName)
		execErr = removeSiteDir(ctx, siteName)

	case "install_nginx":
		writeLog("[system] installing nginx...")
		execErr = run(ctx, "apt-get", "install", "-y", "nginx")

	case "install_node":
		writeLog("[system] adding NodeSource repository...")
		if err := run(ctx, "bash", "-c", "curl -fsSL https://deb.nodesource.com/setup_22.x | bash -"); err != nil {
			execErr = fmt.Errorf("nodesource setup: %w", err)
			break
		}
		writeLog("[system] installing node.js...")
		if err := run(ctx, "apt-get", "install", "-y", "nodejs"); err != nil {
			execErr = fmt.Errorf("nodejs install: %w", err)
			break
		}
		writeLog("[system] installing pm2...")
		execErr = run(ctx, "npm", "install", "-g", "pm2")

	case "deploy":
		writeLog("[deploy] deploying %s...", siteName)
		execErr = e.runDeployScript(ctx, siteName, params, lb, writeLog)

	case "update_env":
		execErr = writeEnvFile(siteName, params)

	case "site_raw_nginx":
		config, _ := params["raw_config"].(string)
		execErr = writeNginxConfig(ctx, siteName, config)

	case "ssl_issue":
		domain, _ := params["domain"].(string)
		email, _ := params["email"].(string)
		writeLog("[ssl] issuing certificate for %s...", domain)
		execErr = issueSSL(ctx, domain, email)

	case "ssh_key_add":
		publicKey, _ := params["public_key"].(string)
		execErr = addSSHKey(publicKey)

	case "service_restart":
		service, _ := params["service"].(string)
		execErr = run(ctx, "systemctl", "restart", service)

	case "daemon_install_supervisor":
		writeLog("[daemon] installing supervisor...")
		execErr = installSupervisor(ctx)

	case "daemon_create":
		name, _ := params["name"].(string)
		command, _ := params["command"].(string)
		scale, _ := params["scale"].(float64)
		execErr = createSupervisorConfig(siteName, name, command, int(scale), params)

	case "daemon_delete":
		name, _ := params["name"].(string)
		execErr = deleteSupervisorConfig(siteName, name)

	case "daemon_restart":
		name, _ := params["name"].(string)
		execErr = restartSupervisorProgram(ctx, siteName, name)

	case "self_update":
		writeLog("[update] checking for updates...")
		repo := "ekilie/ekilied"
		release, available, err := CheckForUpdate(repo, config.Version)
		if err != nil {
			execErr = fmt.Errorf("check failed: %w", err)
			break
		}
		if !available {
			writeLog("[update] already up to date (current: %s)", config.Version)
			break
		}
		writeLog("[update] found %s (current: %s), downloading...", release.TagName, config.Version)
		if err := SelfUpdate(repo, release); err != nil {
			execErr = err
			break
		}
		// Binary replaced — complete job before restarting so the CP gets the result.
		writeLog("[update] updated successfully, restarting...")
		lb.flushNow()
		if err := e.client.CompleteJob(ctx, jobID, "success", "", action, nil); err != nil {
			log.Printf("complete job %d failed: %v", jobID, err)
		}
		exec.Command("systemctl", "restart", "ekilied").Start()
		return

	default:
		execErr = fmt.Errorf("unknown action: %s", action)
	}

	status := "success"
	errMsg := ""
	if execErr != nil {
		status = "failed"
		errMsg = execErr.Error()
		log.Printf("job %d failed: %v", jobID, execErr)
	}

	writeLog("[complete] %s", status)
	lb.flushNow()

	if err := e.client.CompleteJob(ctx, jobID, status, errMsg, action, nil); err != nil {
		log.Printf("complete job %d failed: %v", jobID, err)
	}
}

func (e *JobEngine) runDeployScript(ctx context.Context, siteName string, params map[string]any, lb *LogBatcher, logf func(string, ...any)) error {
	if !e.deployLk.TryAcquire(siteName, 0) {
		return fmt.Errorf("deploy already in progress for site: %s", siteName)
	}
	defer e.deployLk.Release(siteName)

	siteDir := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	repoDir := siteDir + "/current"
	os.MkdirAll(siteDir, 0755)

	deployScript, _ := params["deploy_script"].(string)
	repoURL, _ := params["repository"].(string)
	branch, _ := params["branch"].(string)
	gitToken, _ := params["git_token"].(string)
	commitSHA, _ := params["commit_sha"].(string)

	if deployScript == "" {
		return fmt.Errorf("no deploy script provided")
	}

	// Clone or pull repository first so repo/ directory exists
	if repoURL != "" {
		logf("[deploy] cloning %s [%s]...", repoURL, orDefaultStr(branch, "main"))
		if err := cloneRepo(ctx, repoDir, repoURL, branch, gitToken, commitSHA, lb); err != nil {
			return fmt.Errorf("git: %w", err)
		}
		logf("[deploy] cloned successfully")

		// Copy env.example to .env as a starting point (if .env doesn't already exist)
		envExamplePath := filepath.Join(repoDir, "env.example")
		envPath := filepath.Join(repoDir, ".env")
		if _, err := os.Stat(envExamplePath); err == nil {
			if _, err := os.Stat(envPath); os.IsNotExist(err) {
				logf("[deploy] copying env.example to .env...")
				if data, readErr := os.ReadFile(envExamplePath); readErr == nil {
					os.WriteFile(envPath, data, 0644)
				}
			}
		}
	}

	// Write .env from control plane params
	writeEnvFile(siteName, params)

	// Write deploy script to file
	scriptPath := siteDir + "/.ekilie-deploy"
	os.WriteFile(scriptPath, []byte(deployScript), 0755)

	// Run the deploy script from repo root
	logf("[deploy] running deploy script...")
	workDir := repoDir
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		workDir = siteDir
	}
	cmd := exec.CommandContext(ctx, "/bin/bash", scriptPath)
	cmd.Dir = workDir
	cmd.Stdout = lb
	cmd.Stderr = lb

	return cmd.Run()
}

func cloneRepo(ctx context.Context, repoDir, repoURL, branch, gitToken, commitSHA string, lb *LogBatcher) error {
	authenticatedURL := repoURL

	// Use git token for GitHub private repos
	if gitToken != "" && strings.Contains(repoURL, "github.com") {
		authenticatedURL = strings.Replace(
			repoURL, "https://",
			fmt.Sprintf("https://x-access-token:%s@", gitToken), 1,
		)
	}

	// Normalise empty branch to "HEAD" so both fresh-clone and already-cloned paths
	// behave consistently (checkout the remote's default branch).
	ref := branch
	if ref == "" {
		ref = "HEAD"
	}

	if _, err := os.Stat(repoDir + "/.git"); os.IsNotExist(err) {
		// Fresh clone
		args := []string{"clone", "--depth=1"}
		if ref != "HEAD" {
			args = append(args, "-b", ref)
		}
		args = append(args, authenticatedURL, repoDir)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Stdout = lb
		cmd.Stderr = lb
		return cmd.Run()
	}

	// Already cloned — fetch and checkout
	cmd := exec.CommandContext(ctx, "git", "fetch", "--depth=1", "origin", ref)
	cmd.Dir = repoDir
	cmd.Stdout = lb
	cmd.Stderr = lb
	if err := cmd.Run(); err != nil {
		return err
	}

	checkoutRef := "origin/" + ref
	if commitSHA != "" {
		checkoutRef = commitSHA
	}

	cmd = exec.CommandContext(ctx, "git", "checkout", "-f", checkoutRef)
	cmd.Dir = repoDir
	cmd.Stdout = lb
	cmd.Stderr = lb
	return cmd.Run()
}

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ── Helpers ─────────────────────────────────────────────────────────

func createSiteDir(ctx context.Context, siteName string) error {
	return run(ctx, "mkdir", "-p", fmt.Sprintf("/opt/ekilie/sites/%s", siteName))
}

func removeSiteDir(ctx context.Context, siteName string) error {
	return run(ctx, "rm", "-rf", fmt.Sprintf("/opt/ekilie/sites/%s", siteName))
}

func writeNginxConfig(ctx context.Context, siteName, config string) error {
	path := fmt.Sprintf("/etc/nginx/sites-available/%s", siteName)
	if err := writeFile(path, config); err != nil {
		return err
	}
	// Validate config
	if out, err := exec.CommandContext(ctx, "nginx", "-t").CombinedOutput(); err != nil {
		return fmt.Errorf("nginx validation failed: %s", string(out))
	}
	// Enable on boot and reload/start
	exec.CommandContext(ctx, "systemctl", "enable", "nginx").Run()
	return run(ctx, "systemctl", "reload-or-restart", "nginx")
}

func writeEnvFile(siteName string, params map[string]any) error {
	envRaw, ok := params["env"]
	if !ok {
		return nil
	}
	env, ok := envRaw.(map[string]any)
	if !ok || len(env) == 0 {
		return nil
	}

	// Default: write .env at site root (widely expected location)
	envPath := fmt.Sprintf("/opt/ekilie/sites/%s/.env", siteName)

	// Allow deploy script to specify a custom path via env_path param
	if customPath, ok := params["env_path"].(string); ok && customPath != "" {
		envPath = customPath
	}

	// Ensure parent directory exists
	parentDir := filepath.Dir(envPath)
	os.MkdirAll(parentDir, 0755)

	var buf []byte
	for k, v := range env {
		buf = append(buf, fmt.Appendf(nil, "%s=%v\n", k, v)...)
	}
	return os.WriteFile(envPath, buf, 0644)
}

func issueSSL(ctx context.Context, domain, email string) error {
	args := []string{"--nginx", "--non-interactive", "--agree-tos", "--redirect"}
	if email != "" {
		args = append(args, "--email", email)
	}
	args = append(args, "-d", domain)
	return run(ctx, "certbot", args...)
}

func addSSHKey(publicKey string) error {
	if publicKey == "" {
		return fmt.Errorf("public key is required")
	}
	sshDir := "/root/.ssh"
	os.MkdirAll(sshDir, 0700)

	authorizedKeysPath := sshDir + "/authorized_keys"

	// Read existing keys to avoid duplicates
	if data, err := os.ReadFile(authorizedKeysPath); err == nil {
		if strings.Contains(string(data), publicKey) {
			// Key already exists no-op
			return nil
		}
	}

	f, err := os.OpenFile(authorizedKeysPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open authorized_keys: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(publicKey + "\n"); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

// ── Supervisor daemon management ───────────────────────────

const supervisorConfDir = "/etc/supervisor/conf.d"

func installSupervisor(ctx context.Context) error {
	if err := run(ctx, "apt-get", "install", "-y", "supervisor"); err != nil {
		return err
	}
	if err := run(ctx, "systemctl", "enable", "supervisor"); err != nil {
		return err
	}
	return run(ctx, "systemctl", "start", "supervisor")
}

func daemonProgramName(siteName, name string) string {
	return fmt.Sprintf("%s-%s", siteName, name)
}

func ensureUser(username string) {
	if exec.Command("id", "-u", username).Run() == nil {
		return // user already exists
	}
	exec.Command("useradd", "-r", "-s", "/bin/false", "-d", "/opt/ekilie", username).Run()
}

func createSupervisorConfig(siteName, name, command string, scale int, params map[string]any) error {
	if command == "" {
		return fmt.Errorf("command is required")
	}
	if scale < 1 {
		scale = 1
	}

	ensureUser("ekilie")

	progName := daemonProgramName(siteName, name)
	siteDir := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	logDir := siteDir + "/logs"
	os.MkdirAll(logDir, 0755)

	// Build environment vars from params
	var envParts []string
	if envRaw, ok := params["env"].(map[string]any); ok {
		for k, v := range envRaw {
			envParts = append(envParts, fmt.Sprintf("%s=%q", k, fmt.Sprintf("%v", v)))
		}
	}

	var envSection string
	if len(envParts) > 0 {
		envSection = fmt.Sprintf("environment=%s\n", strings.Join(envParts, ","))
	}

	conf := fmt.Sprintf(`[program:%s]
command=%s
	directory=%s/current
user=ekilie
numprocs=%d
autostart=true
autorestart=true
stopwaitsecs=10
startretries=3
stdout_logfile=%s/%s.log
stderr_logfile=%s/%s-error.log
%s`, progName, command, siteDir, scale,
		logDir, name, logDir, name, envSection)

	path := filepath.Join(supervisorConfDir, progName+".conf")
	if err := os.WriteFile(path, []byte(conf), 0644); err != nil {
		return fmt.Errorf("write supervisor conf: %w", err)
	}

	// Reread and update supervisor
	if out, err := exec.Command("supervisorctl", "reread").CombinedOutput(); err != nil {
		return fmt.Errorf("supervisorctl reread failed: %s", string(out))
	}
	if out, err := exec.Command("supervisorctl", "update").CombinedOutput(); err != nil {
		return fmt.Errorf("supervisorctl update failed: %s", string(out))
	}
	return nil
}

func deleteSupervisorConfig(siteName, name string) error {
	progName := daemonProgramName(siteName, name)
	path := filepath.Join(supervisorConfDir, progName+".conf")

	// Stop and remove the program
	exec.Command("supervisorctl", "stop", progName).Run()
	exec.Command("supervisorctl", "remove", progName).Run()

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove supervisor conf: %w", err)
	}

	if out, err := exec.Command("supervisorctl", "update").CombinedOutput(); err != nil {
		return fmt.Errorf("supervisorctl update failed: %s", string(out))
	}
	return nil
}

func restartSupervisorProgram(ctx context.Context, siteName, name string) error {
	progName := daemonProgramName(siteName, name)
	return run(ctx, "supervisorctl", "restart", progName)
}

// cleanupSupervisorForSite removes all supervisor programs and configs
// associated with the given site name. Called when a site is deleted.
func cleanupSupervisorForSite(siteName string) {
	pattern := filepath.Join(supervisorConfDir, siteName+"-*.conf")
	files, _ := filepath.Glob(pattern)
	for _, f := range files {
		progName := strings.TrimSuffix(filepath.Base(f), ".conf")
		exec.Command("supervisorctl", "stop", progName).Run()
		exec.Command("supervisorctl", "remove", progName).Run()
		os.Remove(f)
	}
	if len(files) > 0 {
		exec.Command("supervisorctl", "update").Run()
	}
}

func run(ctx context.Context, name string, args ...string) error {
	// Combine parent context (for agent shutdown / job cancellation) with a timeout.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", name, string(out))
	}
	return nil
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}
