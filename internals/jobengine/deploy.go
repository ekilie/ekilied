package jobengine

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	// Resolve inside the site's checkout directory; env_path may not be
	// absolute or escape it.
	envPath, err := resolveEnvPath(siteName, params)
	if err != nil {
		return err
	}

	// Ensure parent directory exists
	parentDir := filepath.Dir(envPath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("mkdir env dir: %w", err)
	}

	var buf []byte
	for k, v := range env {
		buf = append(buf, fmt.Appendf(nil, "%s=%v\n", k, v)...)
	}
	return os.WriteFile(envPath, buf, 0644)
}

// cloneRepo clones or pulls a git repository into the site's "current" directory.
//
// Authentication is passed to git through environment config
// (GIT_CONFIG_COUNT + http.extraheader) so the installation token never lands
// in .git/config, process arguments, or git output. The remote is always kept
// as the clean URL, which also repairs checkouts created by older agents that
// embedded an expiring token in origin.
func cloneRepo(ctx context.Context, repoDir, repoURL, branch, gitToken, commitSHA string, lb *LogBatcher) error {
	cleanURL, isGitHub, err := sanitizeRepoURL(repoURL)
	if err != nil {
		return err
	}

	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	}
	if gitToken != "" && isGitHub {
		env = append(env, gitAuthEnv(gitToken)...)
	}
	out := newRedactingWriter(lb, gitToken)

	ref := branch
	if ref == "" {
		ref = "HEAD"
	}

	// Fresh clone.
	if _, err := os.Stat(repoDir + "/.git"); os.IsNotExist(err) {
		args := []string{"clone", "--depth=1"}
		if ref != "HEAD" {
			args = append(args, "-b", ref)
		}
		args = append(args, cleanURL, repoDir)
		if err := runGit(ctx, "", args, env, out); err != nil {
			return err
		}
		// Defensive: never keep credentials in origin, even if a future git
		// version rewrites the URL.
		if err := runGit(ctx, repoDir, []string{"remote", "set-url", "origin", cleanURL}, env, out); err != nil {
			return err
		}
		if commitSHA != "" {
			if err := runGit(ctx, repoDir, []string{"checkout", "-f", "-q", commitSHA}, env, out); err != nil {
				if err := runGit(ctx, repoDir, []string{"fetch", "--depth=1", "origin", commitSHA}, env, out); err != nil {
					return err
				}
				return runGit(ctx, repoDir, []string{"checkout", "-f", "-q", commitSHA}, env, out)
			}
		}
		return nil
	}

	// Existing checkout: drop any stale token from origin first, then fetch
	// with a fresh token from the environment.
	if err := runGit(ctx, repoDir, []string{"remote", "set-url", "origin", cleanURL}, env, out); err != nil {
		return err
	}

	if commitSHA != "" {
		if err := runGit(ctx, repoDir, []string{"fetch", "--depth=1", "origin", ref}, env, out); err == nil {
			if err := runGit(ctx, repoDir, []string{"checkout", "-f", "-q", commitSHA}, env, out); err == nil {
				return nil
			}
		}
		if err := runGit(ctx, repoDir, []string{"fetch", "--depth=1", "origin", commitSHA}, env, out); err != nil {
			return err
		}
		return runGit(ctx, repoDir, []string{"checkout", "-f", "-q", commitSHA}, env, out)
	}

	if ref == "HEAD" {
		if err := runGit(ctx, repoDir, []string{"fetch", "--depth=1", "origin"}, env, out); err != nil {
			return err
		}
		return runGit(ctx, repoDir, []string{"checkout", "-f", "-q", "FETCH_HEAD"}, env, out)
	}

	if err := runGit(ctx, repoDir, []string{"fetch", "--depth=1", "origin", ref}, env, out); err != nil {
		return err
	}
	return runGit(ctx, repoDir, []string{"checkout", "-f", "-q", "origin/" + ref}, env, out)
}

// runGit executes git with the supplied environment and a redacting writer.
func runGit(ctx context.Context, dir string, args, env []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// gitAuthEnv builds the git config environment for an installation token.
// The token is sent as an HTTP header and is never written to disk or argv.
func gitAuthEnv(token string) []string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}

// sanitizeRepoURL removes embedded credentials and normalizes GitHub SSH URLs
// to HTTPS so the installation token can be used. It reports whether the host
// is github.com.
func sanitizeRepoURL(raw string) (string, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, fmt.Errorf("repository is required")
	}

	if strings.HasPrefix(raw, "git@github.com:") {
		path := strings.TrimPrefix(raw, "git@github.com:")
		path = strings.TrimSuffix(path, ".git")
		return "https://github.com/" + strings.TrimSuffix(path, "/") + ".git", true, nil
	}
	if strings.HasPrefix(raw, "git@") {
		return raw, false, nil
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false, fmt.Errorf("invalid repository URL")
	}
	isGitHub := strings.EqualFold(u.Hostname(), "github.com")
	u.User = nil
	return u.String(), isGitHub, nil
}

var (
	urlCredentialPattern = regexp.MustCompile(`(?i)(https?://)[^@\s/]+@`)
	// token patterns seen in git errors and old config files
	headerPattern = regexp.MustCompile(`(?i)(Authorization:\s*(?:Basic|Bearer)\s+)[A-Za-z0-9+/=._-]+`)
	tokenPattern  = regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9_]{20,}|x-access-token:)[^\s@]+`)
)

// redactingWriter strips tokens and credential URLs from git output before it
// reaches the job log stream. Chunks are handled as they arrive; git emits
// errors line by line, so the token never spans a write in practice.
type redactingWriter struct {
	w      io.Writer
	secret string
}

func newRedactingWriter(w io.Writer, token string) io.Writer {
	return &redactingWriter{w: w, secret: token}
}

func (r *redactingWriter) Write(p []byte) (int, error) {
	s := string(p)
	if r.secret != "" {
		s = strings.ReplaceAll(s, r.secret, "[REDACTED]")
	}
	s = urlCredentialPattern.ReplaceAllString(s, "${1}[REDACTED]@")
	s = headerPattern.ReplaceAllString(s, "${1}[REDACTED]")
	s = tokenPattern.ReplaceAllString(s, "[REDACTED]")
	_, err := io.WriteString(r.w, s)
	return len(p), err
}
