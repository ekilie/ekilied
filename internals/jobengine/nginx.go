package jobengine

import (
	"context"
	"fmt"
	"os/exec"
)

// installNginx installs nginx via apt.
func installNginx(ctx context.Context) error {
	return run(ctx, "apt-get", "install", "-y", "nginx")
}

// writeNginxConfig writes an nginx site config, validates it, enables nginx, and reloads.
func writeNginxConfig(ctx context.Context, siteName, nginxConfig string) error {
	path := fmt.Sprintf("/etc/nginx/sites-available/%s", siteName)
	if err := writeFile(path, nginxConfig); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "nginx", "-t").CombinedOutput(); err != nil {
		return fmt.Errorf("nginx validation failed: %s", string(out))
	}
	exec.CommandContext(ctx, "systemctl", "enable", "nginx").Run()
	return run(ctx, "systemctl", "reload-or-restart", "nginx")
}

// issueSSL issues an SSL certificate via certbot (nginx mode).
func issueSSL(ctx context.Context, domain, email string) error {
	args := []string{"--nginx", "--non-interactive", "--agree-tos", "--redirect"}
	if email != "" {
		args = append(args, "--email", email)
	}
	args = append(args, "-d", domain)
	return run(ctx, "certbot", args...)
}
