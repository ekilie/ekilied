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

// generateSiteNginxConfig builds an HTTP-only nginx vhost that reverse-proxies
// the site's domain to the local app port. SSL is intentionally not configured
// here — a separate ssl_issue job obtains the certificate later, and the
// acme-challenge location is left as a passthrough so that job can answer the
// HTTP-01 challenge.
func generateSiteNginxConfig(siteName, domain string, port int) string {
	return fmt.Sprintf(`# Ekilie site: %s
server {
    listen 80;
    listen [::]:80;
    server_name %s;

    # ACME challenge passthrough for a later ssl_issue job.
    location ^~ /.well-known/acme-challenge/ {
        root /var/www/html;
        default_type "text/plain";
    }

    location / {
        proxy_pass http://127.0.0.1:%d;
        proxy_http_version 1.1;

        # WebSocket upgrade
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";

        # Forwarded identity
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
`, siteName, domain, port)
}
