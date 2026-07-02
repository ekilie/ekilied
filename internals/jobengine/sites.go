package jobengine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// createSiteDir creates the site directory.
func createSiteDir(ctx context.Context, siteName string) error {
	return run(ctx, "mkdir", "-p", fmt.Sprintf("/opt/ekilie/sites/%s", siteName))
}

// removeSiteDir removes the site directory and all its contents.
func removeSiteDir(ctx context.Context, siteName string) error {
	return run(ctx, "rm", "-rf", fmt.Sprintf("/opt/ekilie/sites/%s", siteName))
}

// createSite performs full first-time site setup: creates the site and repo
// directories, writes the initial .env, and installs an HTTP-only nginx vhost
// reverse-proxying the domain to the local app port. It does not clone the repo
// or issue SSL — those are handled by separate deploy and ssl_issue jobs.
// Idempotent: safe to re-run for an existing site.
func createSite(ctx context.Context, siteName string, params map[string]any, logf func(string, ...any)) error {
	siteDir := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	repoDir := siteDir + "/current"

	logf("[site] creating directories %s...", siteDir)
	if err := createSiteDir(ctx, siteName); err != nil {
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
	if err := writeNginxConfig(ctx, siteName, cfg); err != nil {
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

	siteDir := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	repoDir := siteDir + "/current"
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
// The env param is written to .env and also injected into the command's real
// environment, so the command sees the variables whether or not it loads .env.
func runSiteCommand(ctx context.Context, siteName string, params map[string]any, lb *LogBatcher) error {
	siteDir := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	if err := os.MkdirAll(siteDir, 0755); err != nil {
		return fmt.Errorf("mkdir site: %w", err)
	}

	if err := writeEnvFile(siteName, params); err != nil {
		return fmt.Errorf("write env: %w", err)
	}

	command, _ := params["command"].(string)
	if command == "" {
		return fmt.Errorf("command is required")
	}

	cmdEnv := os.Environ()
	if envRaw, ok := params["env"].(map[string]any); ok {
		for k, v := range envRaw {
			cmdEnv = append(cmdEnv, fmt.Sprintf("%s=%v", k, v))
		}
	}

	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", command)
	cmd.Dir = siteDir
	cmd.Stdout = lb
	cmd.Stderr = lb
	cmd.Env = cmdEnv
	return cmd.Run()
}
