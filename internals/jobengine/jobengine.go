// Package jobengine provides the job execution engine for the Ekilie agent.
// It handles receiving job triggers (via WebSocket or HTTP poll),
// claiming jobs, executing actions, streaming logs, and reporting completion.
package jobengine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/dtos"
)

// JobClient is the interface the job engine needs to communicate
// with the control plane. Implemented by agent.WSClient.
type JobClient interface {
	ClaimJob(ctx context.Context, jobID uint) (*dtos.JobItem, error)
	StreamLogs(ctx context.Context, jobID uint, lines []dtos.LogLine) error
	CompleteJob(ctx context.Context, jobID uint, status, errorMsg, step string, result any) error
}

// ── Deploy lock (per-site) ──────────────────────────────────────────

// DeployLock ensures only one deploy runs per site at a time.
type DeployLock struct {
	mu     sync.Mutex
	active map[string]uint
}

// NewDeployLock creates a new DeployLock.
func NewDeployLock() *DeployLock {
	return &DeployLock{active: make(map[string]uint)}
}

// TryAcquire attempts to acquire the deploy lock for a site.
// Returns false if a deploy is already in progress for that site.
func (dl *DeployLock) TryAcquire(siteName string, jobID uint) bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if _, exists := dl.active[siteName]; exists {
		return false
	}
	dl.active[siteName] = jobID
	return true
}

// Release releases the deploy lock for a site.
func (dl *DeployLock) Release(siteName string) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	delete(dl.active, siteName)
}

// ── Log batcher ─────────────────────────────────────────────────────

// LogBatcher buffers log lines and flushes them to the control plane in batches.
type LogBatcher struct {
	mu     sync.Mutex
	lines  []dtos.LogLine
	jobID  uint
	client JobClient
	ctx    context.Context
	cancel context.CancelFunc
}

// NewLogBatcher creates a new LogBatcher for the given job and starts the flush loop.
func NewLogBatcher(ctx context.Context, jobID uint, client JobClient) *LogBatcher {
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

// Write implements io.Writer for stdout log lines.
func (lb *LogBatcher) Write(p []byte) (int, error) {
	return lb.append("stdout", p)
}

// WriteErr implements writing for stderr log lines.
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

// flushNow flushes all buffered log lines to the control plane.
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

// Close stops the flush loop and performs a final flush.
func (lb *LogBatcher) Close() {
	lb.cancel()
}

// ── Job engine ──────────────────────────────────────────────────────

// JobEngine receives job triggers, claims them from the control plane,
// executes the requested action, and reports results.
type JobEngine struct {
	client     JobClient
	deployLk   *DeployLock
	active     sync.Map // map[uint]struct{} — tracks actively executing job IDs
	dispatched sync.Map // map[uint]struct{} — tracks jobs already WS-triggered so poll skips them
}

// IsDispatched reports whether a job has already been dispatched (via WS or poll).
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

// NewJobEngine creates a new JobEngine with the given control plane client.
func NewJobEngine(client JobClient) *JobEngine {
	return &JobEngine{
		client:   client,
		deployLk: NewDeployLock(),
	}
}

// HandleJobTrigger is the entry point when a job trigger arrives (via WS or poll).
// It atomically claims the job from the control plane and executes it.
func (e *JobEngine) HandleJobTrigger(ctx context.Context, jobID uint) {
	// Mark as dispatched so the poll loop won't also pick it up.
	// If already dispatched (race between WS and poll), skip entirely.
	if !e.markDispatched(jobID) {
		log.Printf("job %d already dispatched, skipping duplicate trigger", jobID)
		return
	}

	// Atomically claim the job (marks as accepted on backend) and get full details.
	job, err := e.client.ClaimJob(ctx, jobID)
	if err != nil {
		log.Printf("handle job trigger %d: claim failed: %v", jobID, err)
		e.clearDispatched(jobID)
		return
	}

	raw, _ := json.Marshal(job.Params)
	e.Execute(ctx, job.ID, job.Action, raw)
}

// Execute runs a job action with the given parameters.
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
		execErr = installNginx(ctx)

	case "install_node":
		execErr = installNode(ctx, writeLog)

	case "deploy":
		writeLog("[deploy] deploying %s...", siteName)
		execErr = e.runDeployScript(ctx, siteName, params, lb, writeLog)

	case "update_env":
		execErr = writeEnvFile(siteName, params)

	case "site_raw_nginx":
		rawCfg, _ := params["raw_config"].(string)
		execErr = writeNginxConfig(ctx, siteName, rawCfg)

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
		execErr = restartService(ctx, service)

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

// runDeployScript performs the full deploy workflow: acquire lock, clone/pull repo,
// copy env.example, write .env, write deploy script, and run it.
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
