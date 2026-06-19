package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ekilie/ekilied/internals/dtos"
)

// ── Deploy lock ──────────────────────────────────────────────────────────

type DeployLock struct {
	mu     sync.Mutex
	active map[string]uint // site_name → job_id
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

// ── Log batcher ──────────────────────────────────────────────────────────

type LogBatcher struct {
	mu      sync.Mutex
	lines   []dtos.LogLine
	jobID   uint
	client  *WSClient
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewLogBatcher(ctx context.Context, jobID uint, client *WSClient) *LogBatcher {
	ctx, cancel := context.WithCancel(ctx)
	lb := &LogBatcher{
		lines:   make([]dtos.LogLine, 0, 100),
		jobID:   jobID,
		client:  client,
		ctx:     ctx,
		cancel:  cancel,
	}
	go lb.flushLoop()
	return lb
}

func (lb *LogBatcher) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for _, line := range strings.Split(string(p), "\n") {
		if line == "" {
			continue
		}
		lb.lines = append(lb.lines, dtos.LogLine{
			Stream:   "stdout",
			Line:     line,
			TS:       time.Now().UTC().Format(time.RFC3339),
			Sequence: len(lb.lines) + 1,
		})
	}
	return len(p), nil
}

func (lb *LogBatcher) WriteErr(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for _, line := range strings.Split(string(p), "\n") {
		if line == "" {
			continue
		}
		lb.lines = append(lb.lines, dtos.LogLine{
			Stream:   "stderr",
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

// ── Job engine ───────────────────────────────────────────────────────────

type JobEngine struct {
	client    *WSClient
	deployLk *DeployLock
}

func NewJobEngine(client *WSClient) *JobEngine {
	return &JobEngine{
		client:    client,
		deployLk:  NewDeployLock(),
	}
}

func (e *JobEngine) Execute(ctx context.Context, jobID uint, action string, rawParams json.RawMessage) {
	log.Printf("executing job %d: action=%s", jobID, action)

	// Accept
	if err := e.client.AcceptJob(ctx, jobID); err != nil {
		log.Printf("accept job %d failed: %v", jobID, err)
		return
	}

	// Parse params
	var params map[string]interface{}
	json.Unmarshal(rawParams, &params)

	// Log batcher
	lb := NewLogBatcher(ctx, jobID, e.client)
	defer lb.Close()

	runStep := func(step string, fn func() error) error {
		lb.mu.Lock()
		lb.lines = append(lb.lines, dtos.LogLine{
			Stream: "stdout", Line: fmt.Sprintf("[%s] starting...", step),
			Step: step, TS: time.Now().UTC().Format(time.RFC3339),
		})
		lb.mu.Unlock()
		return fn()
	}

	var execErr error
	siteName, _ := params["site_name"].(string)
	repository, _ := params["repository"].(string)
	branch, _ := params["branch"].(string)
	branch = orDefault(branch, "main")

	switch action {
	case "site_create":
		execErr = runStep("site_create", func() error {
			return e.createSite(params, siteName)
		})

	case "site_delete":
		execErr = runStep("site_delete", func() error {
			return removeSite(siteName)
		})

	case "install_nginx":
		execErr = runStep("install_nginx", func() error {
			return installNginx()
		})

	case "install_node":
		execErr = runStep("install_node", func() error {
			return installNode()
		})

	case "deploy":
		fallthrough
	case "deploy_node":
		execErr = runStep("deploy_node", func() error {
			return e.deployNode(siteName, repository, branch, params, lb)
		})

	case "deploy_static":
		execErr = runStep("deploy_static", func() error {
			return e.deployStatic(siteName, repository, branch, params, lb)
		})

	case "deploy_custom":
		execErr = runStep("deploy_custom", func() error {
			return e.deployCustom(siteName, params, lb)
		})

	case "rollback":
		targetRelease, _ := params["target_release"].(string)
		execErr = runStep("rollback", func() error {
			return rollbackSite(siteName, targetRelease)
		})

	case "update_env":
		execErr = runStep("update_env", func() error {
			return writeEnvFile(siteName, params)
		})

	case "ssh_key_add":
		publicKey, _ := params["public_key"].(string)
		execErr = runStep("ssh_key_add", func() error {
			return addSSHKey(publicKey)
		})

	case "ssh_key_remove":
		execErr = runStep("ssh_key_remove", func() error {
			return removeSSHKey(params)
		})

	case "service_restart":
		service, _ := params["service"].(string)
		execErr = runStep("service_restart", func() error {
			return restartService(service)
		})

	default:
		execErr = fmt.Errorf("unknown action: %s", action)
	}

	// Complete
	status := "success"
	errMsg := ""
	failedStep := ""
	if execErr != nil {
		status = "failed"
		errMsg = execErr.Error()
		failedStep = action
		log.Printf("job %d failed: %v", jobID, execErr)
	}

	if err := e.client.CompleteJob(ctx, jobID, status, errMsg, failedStep, nil); err != nil {
		log.Printf("complete job %d failed: %v", jobID, err)
	}
}

// ── Recipe stubs (full implementations in Phase 1b/1c) ───────────────────

func (e *JobEngine) createSite(params map[string]interface{}, siteName string) error {
	// TODO: Phase 1b — create dirs, system user, nginx config
	log.Printf("create site: %s (stub)", siteName)
	return nil
}

func (e *JobEngine) deployNode(siteName, repo, branch string, params map[string]interface{}, lb *LogBatcher) error {
	if !e.deployLk.TryAcquire(siteName, 0) {
		return fmt.Errorf("deploy lock contention for site: %s", siteName)
	}
	defer e.deployLk.Release(siteName)
	return runDeploy(siteName, repo, branch, params, lb)
}

func (e *JobEngine) deployStatic(siteName, repo, branch string, params map[string]interface{}, lb *LogBatcher) error {
	if !e.deployLk.TryAcquire(siteName, 0) {
		return fmt.Errorf("deploy lock contention for site: %s", siteName)
	}
	defer e.deployLk.Release(siteName)
	return runDeploy(siteName, repo, branch, params, lb)
}

func (e *JobEngine) deployCustom(siteName string, params map[string]interface{}, lb *LogBatcher) error {
	if !e.deployLk.TryAcquire(siteName, 0) {
		return fmt.Errorf("deploy lock contention for site: %s", siteName)
	}
	defer e.deployLk.Release(siteName)

	sitePath := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	releasePath := fmt.Sprintf("%s/releases/%s", sitePath, time.Now().Format("20060102150405"))
	currentPath := sitePath + "/current"

	if err := os.MkdirAll(releasePath, 0755); err != nil {
		return err
	}

	tmpLink := sitePath + "/.current_tmp"
	os.Symlink(releasePath, tmpLink)
	os.Rename(tmpLink, currentPath)

	exec.Command("systemctl", "reload", "nginx").Run()
	return healthCheck(fmt.Sprintf("https://%s/health", siteName), 30*time.Second)
}

// ── Shared helpers ───────────────────────────────────────────────────────

func runDeploy(siteName, repo, branch string, params map[string]interface{}, lb *LogBatcher) error {
	sitePath := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	releasePath := fmt.Sprintf("%s/releases/%s", sitePath, time.Now().Format("20060102150405"))

	// 1. Clone
	if err := sh(lb, "git", "clone", "--depth=1", "-b", branch, repo, releasePath); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	// 2. Symlink .env
	sharedEnv := sitePath + "/shared/.env"
	if _, err := os.Stat(sharedEnv); err == nil {
		os.Symlink(sharedEnv, releasePath+"/.env")
	}

	// 3. Run deploy script
	deployScript, _ := params["deploy_script"].(string)
	if deployScript != "" {
		cmd := exec.Command("/bin/bash", "-c", deployScript)
		cmd.Dir = releasePath
		cmd.Stdout = lb
		cmd.Stderr = lb
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("deploy script: %w", err)
		}
	}

	// 4. Atomic symlink swap
	tmpLink := sitePath + "/.current_tmp"
	os.Symlink(releasePath, tmpLink)
	if err := os.Rename(tmpLink, sitePath+"/current"); err != nil {
		os.Remove(tmpLink)
		return fmt.Errorf("symlink swap: %w", err)
	}

	// 5. Health check
	domain, _ := params["domain"].(string)
	if domain != "" {
		if err := healthCheck("https://"+domain+"/health", 30*time.Second); err != nil {
			return fmt.Errorf("health check: %w", err)
		}
	}

	// 6. Cleanup (keep last 5)
	cleanupReleases(sitePath, 5)
	return nil
}

func sh(lb *LogBatcher, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = lb
	cmd.Stderr = lb
	return cmd.Run()
}

func healthCheck(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return nil
		}
		if err == nil {
			resp.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("health check timed out")
}

func removeSite(siteName string) error {
	// TODO: Phase 1b — remove nginx config, system user, files
	return exec.Command("rm", "-rf", fmt.Sprintf("/opt/ekilie/sites/%s", siteName)).Run()
}

func installNginx() error {
	return exec.Command("apt-get", "install", "-y", "nginx").Run()
}

func installNode() error {
	return sh(nil, "bash", "-c",
		"curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && apt-get install -y nodejs && npm install -g pm2")
}

func writeEnvFile(siteName string, params map[string]interface{}) error {
	envPath := fmt.Sprintf("/opt/ekilie/sites/%s/shared/.env", siteName)
	os.MkdirAll(envPath[:strings.LastIndex(envPath, "/")], 0755)

	env, _ := params["env"].(map[string]interface{})
	var buf bytes.Buffer
	for k, v := range env {
		buf.WriteString(fmt.Sprintf("%s=%v\n", k, v))
	}
	return os.WriteFile(envPath, buf.Bytes(), 0644)
}

func addSSHKey(publicKey string) error {
	home := "/root"
	sshDir := home + "/.ssh"
	os.MkdirAll(sshDir, 0700)

	f, err := os.OpenFile(sshDir+"/authorized_keys", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(publicKey + "\n")
	return err
}

func removeSSHKey(params map[string]interface{}) error {
	// TODO: Phase 1c — remove specific key by fingerprint
	return nil
}

func restartService(service string) error {
	return exec.Command("systemctl", "restart", service).Run()
}

func rollbackSite(siteName, targetRelease string) error {
	sitePath := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	targetPath := fmt.Sprintf("%s/releases/%s", sitePath, targetRelease)

	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return fmt.Errorf("release %s not found", targetRelease)
	}

	tmpLink := sitePath + "/.current_tmp"
	os.Symlink(targetPath, tmpLink)
	os.Rename(tmpLink, sitePath+"/current")

	exec.Command("systemctl", "restart", siteName+"-ekilie").Run()
	return healthCheck(fmt.Sprintf("https://%s/health", siteName), 30*time.Second)
}

func cleanupReleases(sitePath string, keep int) {
	entries, _ := os.ReadDir(sitePath + "/releases")
	if len(entries) <= keep {
		return
	}
	for _, e := range entries[:len(entries)-keep] {
		os.RemoveAll(sitePath + "/releases/" + e.Name())
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
