package jobengine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// writeEnvFile writes the .env file for a site from control plane params.
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

// cloneRepo clones or pulls a git repository into the site's "current" directory.
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
