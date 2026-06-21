package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

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
	client   *WSClient
	deployLk *DeployLock
}

func NewJobEngine(client *WSClient) *JobEngine {
	return &JobEngine{
		client:   client,
		deployLk: NewDeployLock(),
	}
}

func (e *JobEngine) Execute(ctx context.Context, jobID uint, action string, rawParams json.RawMessage) {
	log.Printf("executing job %d: action=%s", jobID, action)

	// Accept the job
	if err := e.client.AcceptJob(ctx, jobID); err != nil {
		log.Printf("accept job %d failed: %v", jobID, err)
		return
	}

	var params map[string]any
	json.Unmarshal(rawParams, &params)

	siteName, _ := params["site_name"].(string)

	lb := NewLogBatcher(ctx, jobID, e.client)
	defer lb.Close()

	writeLog := func(format string, args ...any) {
		lb.mu.Lock()
		lb.lines = append(lb.lines, dtos.LogLine{
			Stream: "stdout",
			Line:   fmt.Sprintf(format, args...),
			TS:     time.Now().UTC().Format(time.RFC3339),
		})
		lb.mu.Unlock()
	}

	var execErr error

	switch action {
	case "site_create":
		writeLog("[site] creating site %s...", siteName)
		execErr = createSiteDir(siteName)

	case "site_delete":
		writeLog("[site] deleting site %s...", siteName)
		execErr = removeSiteDir(siteName)

	case "install_nginx":
		writeLog("[system] installing nginx...")
		execErr = run("apt-get", "install", "-y", "nginx")

	case "install_node":
		writeLog("[system] installing node.js...")
		execErr = run("bash", "-c",
			"curl -fsSL https://deb.nodesource.com/setup_22.x | bash - && apt-get install -y nodejs && npm install -g pm2")

	case "deploy":
		writeLog("[deploy] deploying %s...", siteName)
		execErr = e.runDeployScript(siteName, params, lb, writeLog)

	case "update_env":
		execErr = writeEnvFile(siteName, params)

	case "site_raw_nginx":
		config, _ := params["raw_config"].(string)
		execErr = writeNginxConfig(siteName, config)

	case "ssl_issue":
		domain, _ := params["domain"].(string)
		email, _ := params["email"].(string)
		writeLog("[ssl] issuing certificate for %s...", domain)
		execErr = issueSSL(domain, email)

	case "ssh_key_add":
		publicKey, _ := params["public_key"].(string)
		execErr = addSSHKey(publicKey)

	case "service_restart":
		service, _ := params["service"].(string)
		execErr = run("systemctl", "restart", service)

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

func (e *JobEngine) runDeployScript(siteName string, params map[string]interface{}, lb *LogBatcher, logf func(string, ...interface{})) error {
	if !e.deployLk.TryAcquire(siteName, 0) {
		return fmt.Errorf("deploy already in progress for site: %s", siteName)
	}
	defer e.deployLk.Release(siteName)

	siteDir := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	repoDir := siteDir + "/repo"
	os.MkdirAll(siteDir, 0755)

	deployScript, _ := params["deploy_script"].(string)
	repoURL, _ := params["repository"].(string)
	branch, _ := params["branch"].(string)
	gitToken, _ := params["git_token"].(string)
	commitSHA, _ := params["commit_sha"].(string)

	if deployScript == "" {
		return fmt.Errorf("no deploy script provided")
	}

	// Write .env before running script
	writeEnvFile(siteName, params)

	// Clone or pull repository
	if repoURL != "" {
		logf("[deploy] cloning %s [%s]...", repoURL, orDefaultStr(branch, "main"))
		if err := cloneRepo(repoDir, repoURL, branch, gitToken, commitSHA, lb); err != nil {
			return fmt.Errorf("git: %w", err)
		}
		logf("[deploy] cloned successfully")
	}

	// Write deploy script to file
	scriptPath := siteDir + "/.ekilie-deploy"
	os.WriteFile(scriptPath, []byte(deployScript), 0755)

	// Run the deploy script from repo root
	logf("[deploy] running deploy script...")
	workDir := repoDir
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		workDir = siteDir
	}
	cmd := exec.Command("/bin/bash", scriptPath)
	cmd.Dir = workDir
	cmd.Stdout = lb
	cmd.Stderr = lb

	return cmd.Run()
}

func cloneRepo(repoDir, repoURL, branch, gitToken, commitSHA string, lb *LogBatcher) error {
	authenticatedURL := repoURL

	// Use git token for GitHub private repos
	if gitToken != "" && strings.Contains(repoURL, "github.com") {
		authenticatedURL = strings.Replace(
			repoURL, "https://",
			fmt.Sprintf("https://x-access-token:%s@", gitToken), 1,
		)
	}

	if _, err := os.Stat(repoDir + "/.git"); os.IsNotExist(err) {
		// Fresh clone
		args := []string{"clone", "--depth=1"}
		if branch != "" {
			args = append(args, "-b", branch)
		}
		args = append(args, authenticatedURL, repoDir)
		cmd := exec.Command("git", args...)
		cmd.Stdout = lb
		cmd.Stderr = lb
		return cmd.Run()
	}

	// Already cloned fetch and checkout
	cmd := exec.Command("git", "fetch", "--depth=1", "origin", branch)
	cmd.Dir = repoDir
	cmd.Stdout = lb
	cmd.Stderr = lb
	if err := cmd.Run(); err != nil {
		return err
	}

	checkoutRef := "origin/" + branch
	if commitSHA != "" {
		checkoutRef = commitSHA
	}

	cmd = exec.Command("git", "checkout", "-f", checkoutRef)
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

func createSiteDir(siteName string) error {
	return run("mkdir", "-p", fmt.Sprintf("/opt/ekilie/sites/%s", siteName))
}

func removeSiteDir(siteName string) error {
	return run("rm", "-rf", fmt.Sprintf("/opt/ekilie/sites/%s", siteName))
}

func writeNginxConfig(siteName, config string) error {
	path := fmt.Sprintf("/etc/nginx/sites-available/%s", siteName)
	if err := writeFile(path, config); err != nil {
		return err
	}
	// Validate and reload
	if out, err := exec.Command("nginx", "-t").CombinedOutput(); err != nil {
		return fmt.Errorf("nginx validation failed: %s", string(out))
	}
	return run("systemctl", "reload", "nginx")
}

func writeEnvFile(siteName string, params map[string]interface{}) error {
	envDir := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	envPath := envDir + "/.env"

	env, _ := params["env"].(map[string]interface{})
	if len(env) == 0 {
		return nil
	}

	var buf []byte
	for k, v := range env {
		buf = append(buf, []byte(fmt.Sprintf("%s=%v\n", k, v))...)
	}
	return writeFile(envPath, string(buf))
}

func issueSSL(domain, email string) error {
	args := []string{"--nginx", "--non-interactive", "--agree-tos", "--redirect"}
	if email != "" {
		args = append(args, "--email", email)
	}
	args = append(args, "-d", domain)
	return run("certbot", args...)
}

func addSSHKey(publicKey string) error {
	if publicKey == "" {
		return fmt.Errorf("public key is required")
	}
	sshDir := "/root/.ssh"
	os.MkdirAll(sshDir, 0700)

	f, err := os.OpenFile(sshDir+"/authorized_keys", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open authorized_keys: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(publicKey + "\n"); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", name, string(out))
	}
	return nil
}

func writeFile(path, content string) error {
	return exec.Command("bash", "-c", fmt.Sprintf("cat > %s << 'FILEEOF'\n%s\nFILEEOF", path, content)).Run()
}
