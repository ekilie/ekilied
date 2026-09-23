package jobengine

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// createSiteDir creates the site directory. Native calls replace the old
// mkdir/rm shell-outs: no extra process, no captured output.
func createSiteDir(siteName string) error {
	dir, err := siteDirPath(siteName)
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0755)
}

// removeSiteDir removes the site directory and all its contents.
func removeSiteDir(siteName string) error {
	dir, err := siteDirPath(siteName)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// createSite performs full first-time site setup: creates the site and repo
// directories, writes the initial .env, and installs an HTTP-only nginx vhost
// reverse-proxying the domain to the local app port. It does not clone the repo
// or issue SSL; those are handled by separate deploy and ssl_issue jobs.
// Idempotent: safe to re-run for an existing site.
func createSite(ctx context.Context, out io.Writer, siteName string, params map[string]any, logf func(string, ...any)) error {
	dir, err := siteDirPath(siteName)
	if err != nil {
		return err
	}
	repoDir, err := siteRepoPath(siteName)
	if err != nil {
		return err
	}

	logf("[site] creating directories %s...", dir)
	if err := createSiteDir(siteName); err != nil {
		return fmt.Errorf("mkdir site: %w", err)
	}
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		return fmt.Errorf("mkdir current: %w", err)
	}

	logf("[site] writing .env...")
	if err := writeEnvFile(siteName, params); err != nil {
		return fmt.Errorf("write env: %w", err)
	}

	domain, _ := params["domain"].(string)
	if domain == "" {
		domain = siteName + ".local"
	}
	// JSON numbers arrive as float64; default to 3000 when unset.
	portF, _ := params["port"].(float64)
	port := int(portF)
	if port == 0 {
		port = 3000
	}

	logf("[site] writing nginx vhost for %s -> 127.0.0.1:%d...", domain, port)
	cfg := generateSiteNginxConfig(siteName, domain, port)
	if err := writeNginxConfig(ctx, out, siteName, cfg); err != nil {
		return fmt.Errorf("nginx: %w", err)
	}

	logf("[site] site %s created", siteName)
	return nil
}

// syncSite performs a manual repository sync (clone or fetch+checkout) without
// running a deploy script. It shares the per-site deploy lock with deploys so a
// sync cannot race an in-progress deploy. It does not write .env or run scripts.
func (e *JobEngine) syncSite(ctx context.Context, siteName string, params map[string]any, lb *LogBatcher, logf func(string, ...any)) error {
	if !e.deployLk.TryAcquire(siteName, 0) {
		return fmt.Errorf("deploy already in progress for site: %s", siteName)
	}
	defer e.deployLk.Release(siteName)

	repoDir, err := siteRepoPath(siteName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		return fmt.Errorf("mkdir site: %w", err)
	}

	repoURL, _ := params["repository"].(string)
	branch, _ := params["branch"].(string)
	gitToken, _ := params["git_token"].(string)
	commitSHA, _ := params["commit_sha"].(string)
	if repoURL == "" {
		return fmt.Errorf("repository is required")
	}

	logf("[sync] syncing %s [%s]...", repoURL, orDefaultStr(branch, "main"))
	if err := cloneRepo(ctx, repoDir, repoURL, branch, gitToken, commitSHA, lb); err != nil {
		return fmt.Errorf("git: %w", err)
	}
	logf("[sync] synced successfully")
	return nil
}

// runSiteCommand runs an arbitrary shell command inside the site directory.
// Env is loaded from the .env file on disk (if it exists).
func runSiteCommand(ctx context.Context, siteName string, params map[string]any, lb *LogBatcher) error {
	repoDir, err := siteRepoPath(siteName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		return fmt.Errorf("mkdir site: %w", err)
	}

	// Ensure .env exists
	envPath, err := resolveEnvPath(siteName, params)
	if err != nil {
		return err
	}
	parentDir := filepath.Dir(envPath)
	os.MkdirAll(parentDir, 0755)
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		os.WriteFile(envPath, []byte{}, 0644)
	}

	command, _ := params["command"].(string)
	if command == "" {
		return fmt.Errorf("command is required")
	}

	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", command)
	cmd.Dir = repoDir
	cmd.Stdout = lb
	cmd.Stderr = lb
	return cmd.Run()
}
