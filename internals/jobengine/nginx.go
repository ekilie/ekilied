package jobengine

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// acmeWebroot is where certbot's webroot authenticator writes challenges.
	// Every generated vhost serves /.well-known/acme-challenge/ from it.
	acmeWebroot = "/var/www/html"
	// letsencryptLive holds one directory per certificate lineage.
	letsencryptLive = "/etc/letsencrypt/live"
)

// installNginx installs nginx via apt.
func installNginx(ctx context.Context, out io.Writer) error {
	return run(ctx, out, "apt-get", "install", "-y", "nginx")
}

// nginxConfigPath returns the config file path for a site, guaranteed to live
// inside nginxSitesAvailable.
func nginxConfigPath(siteName string) (string, error) {
	if err := validateSiteName(siteName); err != nil {
		return "", err
	}
	path := filepath.Join(nginxSitesAvailable, siteName)
	if !isContained(nginxSitesAvailable, path) {
		return "", fmt.Errorf("nginx config path %q escapes %s", path, nginxSitesAvailable)
	}
	return path, nil
}

// writeNginxConfig writes an nginx site config, validates it, enables the
// symlink, and reloads. If validation or reload fails the previous config is
// restored and reloaded, so a bad deploy never leaves nginx unable to start
// on the next boot or reload.
func writeNginxConfig(ctx context.Context, out io.Writer, siteName, nginxConfig string) error {
	path, err := nginxConfigPath(siteName)
	if err != nil {
		return err
	}
	previous, readErr := os.ReadFile(path)
	hadPrevious := readErr == nil

	// Write through a temp file so nginx never reads a half-written config.
	tmp := path + ".tmp"
	if err := writeFile(tmp, nginxConfig); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}

	// Create symlink in sites-enabled (removes first to handle re-creates)
	symlink := filepath.Join("/etc/nginx/sites-enabled", siteName)
	os.Remove(symlink)
	if err := os.Symlink(path, symlink); err != nil {
		restoreNginxConfig(path, symlink, previous, hadPrevious)
		return fmt.Errorf("create symlink: %w", err)
	}
	if testOut, err := exec.CommandContext(ctx, "nginx", "-t").CombinedOutput(); err != nil {
		restoreNginxConfig(path, symlink, previous, hadPrevious)
		return fmt.Errorf("nginx validation failed: %s", strings.TrimSpace(string(testOut)))
	}
	exec.CommandContext(ctx, "systemctl", "enable", "nginx").Run()
	if err := run(ctx, out, "systemctl", "reload-or-restart", "nginx"); err != nil {
		restoreNginxConfig(path, symlink, previous, hadPrevious)
		return fmt.Errorf("nginx reload failed: %w", err)
	}
	return nil
}

// restoreNginxConfig puts the previous file back (or removes a brand new site
// config and its symlink) and makes a best-effort reload so the running nginx
// matches what is on disk.
func restoreNginxConfig(path, symlink string, previous []byte, existed bool) {
	if existed {
		_ = os.WriteFile(path, previous, 0644)
	} else {
		_ = os.Remove(path)
		_ = os.Remove(symlink)
	}
	_ = exec.Command("systemctl", "reload-or-restart", "nginx").Run()
}

// removeNginxConfig removes a site's vhost and symlink, then reloads nginx so
// a deleted site stops serving immediately.
func removeNginxConfig(ctx context.Context, out io.Writer, siteName string) error {
	path, err := nginxConfigPath(siteName)
	if err != nil {
		return err
	}
	symlink := filepath.Join("/etc/nginx/sites-enabled", siteName)
	hadConfig := false
	if _, err := os.Stat(path); err == nil {
		hadConfig = true
	}
	os.Remove(symlink)
	os.Remove(path)
	if !hadConfig {
		return nil
	}
	exec.CommandContext(ctx, "systemctl", "enable", "nginx").Run()
	return run(ctx, out, "systemctl", "reload-or-restart", "nginx")
}

// readNginxConfig reads the nginx site config from disk and returns its content.
func readNginxConfig(siteName string) (string, error) {
	path, err := nginxConfigPath(siteName)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read nginx config: %w", err)
	}
	return string(data), nil
}

// certificatePaths returns the live fullchain and private key paths for a
// certificate lineage named after the domain.
func certificatePaths(domain string) (certPath, keyPath string) {
	base := filepath.Join(letsencryptLive, domain)
	return filepath.Join(base, "fullchain.pem"), filepath.Join(base, "privkey.pem")
}

// hasCertificate reports whether both cert and key exist for the domain.
func hasCertificate(domain string) bool {
	certPath, keyPath := certificatePaths(domain)
	if _, err := os.Stat(certPath); err != nil {
		return false
	}
	if _, err := os.Stat(keyPath); err != nil {
		return false
	}
	return true
}

// certificateExpiry reads the leaf certificate and returns its NotAfter.
func certificateExpiry(domain string) (time.Time, error) {
	certPath, _ := certificatePaths(domain)
	data, err := os.ReadFile(certPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("read certificate: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, fmt.Errorf("decode certificate: no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse certificate: %w", err)
	}
	return cert.NotAfter, nil
}

// ensureWebrootRenewal rewrites certbot's renewal config so renewals use the
// webroot authenticator and reload nginx, even for lineages originally created
// with the nginx authenticator/installer. It is a no-op when the file is
// missing or already correct.
func ensureWebrootRenewal(domain string) error {
	path := filepath.Join("/etc/letsencrypt/renewal", domain+".conf")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	text := string(data)

	// Drop the keys that made renewals depend on certbot's nginx plugin.
	drop := regexp.MustCompile(`(?m)^(?:authenticator|installer|renew_hook)\s*=.*\n?`)
	text = drop.ReplaceAllString(text, "")

	renewal := fmt.Sprintf("[renewalparams]\nauthenticator = webroot\nwebroot_path = %s,\nrenew_hook = systemctl reload nginx", acmeWebroot)
	if !strings.Contains(text, "[renewalparams]") {
		return fmt.Errorf("renewal config missing [renewalparams] section")
	}
	text = strings.Replace(text, "[renewalparams]", renewal, 1)

	if !strings.Contains(text, "[[webroot_map]]") {
		text = strings.TrimRight(text, "\n") + fmt.Sprintf("\n\n[[webroot_map]]\n%s = %s\n", domain, acmeWebroot)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// applySiteNginx generates and writes the site vhost for its current mode
// (proxy or static) with or without TLS.
func applySiteNginx(ctx context.Context, out io.Writer, siteName, domain string, params map[string]any, withSSL bool) error {
	cfg, err := siteVhostConfig(siteName, domain, params, withSSL)
	if err != nil {
		return err
	}
	return writeNginxConfig(ctx, out, siteName, cfg)
}

// siteVhostConfig builds the vhost for a site from its job params.
func siteVhostConfig(siteName, domain string, params map[string]any, withSSL bool) (string, error) {
	static, _ := params["static"].(bool)
	if static {
		publishDir, _ := params["publish_dir"].(string)
		root, err := staticRootPath(siteName, publishDir)
		if err != nil {
			return "", err
		}
		return generateStaticSiteNginxConfig(siteName, domain, root, withSSL), nil
	}
	portF, _ := params["port"].(float64)
	port := int(portF)
	if port == 0 {
		port = 3000
	}
	return generateSiteNginxConfig(siteName, domain, port, withSSL), nil
}

// issueSSL obtains a certificate with certbot's webroot authenticator, then
// writes our own TLS vhost. It deliberately does not use certbot's nginx
// installer: that installer rewrites the vhost and fails when it cannot find a
// matching server block, which is exactly what turned a successful issuance
// into a failed job. Certonly keeps issuance and installation independent.
//
// The deploy hook reloads nginx after renewals so the live symlinks are
// picked up automatically.
func issueSSL(ctx context.Context, out io.Writer, siteName string, params map[string]any) (map[string]string, error) {
	domain, _ := params["domain"].(string)
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := validateDomain(domain); err != nil {
		return nil, err
	}
	email, _ := params["email"].(string)
	email = strings.TrimSpace(email)

	// Make sure the HTTP vhost exists and serves the ACME challenge before
	// asking Let's Encrypt to validate. A missing vhost is a common reason
	// for a challenge failure. Keep an existing certificate installed so a
	// retry never drops HTTPS while certbot runs.
	if err := applySiteNginx(ctx, out, siteName, domain, params, hasCertificate(domain)); err != nil {
		return nil, fmt.Errorf("prepare nginx for ACME: %w", err)
	}

	args := []string{
		"certonly",
		"--webroot", "-w", acmeWebroot,
		"--cert-name", domain,
		"--non-interactive",
		"--agree-tos",
		"--keep-until-expiring",
		"--deploy-hook", "systemctl reload nginx",
	}
	if email != "" {
		args = append(args, "-m", email)
	} else {
		args = append(args, "--register-unsafely-without-email")
	}
	args = append(args, "-d", domain)
	if err := run(ctx, out, "certbot", args...); err != nil {
		return nil, fmt.Errorf("certbot: %w", err)
	}

	if !hasCertificate(domain) {
		return nil, fmt.Errorf("certbot reported success but no certificate exists at %s", letsencryptLive)
	}
	// Certificates first issued through certbot's nginx installer carry
	// authenticator/installer = nginx in their renewal config. Future renewals
	// would hit the same "could not find a matching server block" failure, so
	// migrate the lineage to webroot before installing it.
	if err := ensureWebrootRenewal(domain); err != nil {
		return nil, fmt.Errorf("update renewal config: %w", err)
	}
	if err := applySiteNginx(ctx, out, siteName, domain, params, true); err != nil {
		return nil, fmt.Errorf("install certificate in nginx: %w", err)
	}
	expiresAt, err := certificateExpiry(domain)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"domain":     domain,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	}, nil
}

// generateStaticSiteNginxConfig builds an nginx vhost that serves a directory
// from the site's checkout. With TLS it adds a 443 server and redirects HTTP.
// Single-page apps fall back to index.html so client-side routes survive a
// refresh.
func generateStaticSiteNginxConfig(siteName, domain, rootDir string, withSSL bool) string {
	if !withSSL {
		return fmt.Sprintf(`# Ekilie static site: %s
server {
    listen 80;
    listen [::]:80;
    server_name %s;

    # ACME challenge for certbot webroot issuance and renewals.
    location ^~ /.well-known/acme-challenge/ {
        root %s;
        default_type "text/plain";
    }

    root %s;
    index index.html;

    location / {
        try_files $uri $uri/ /index.html;
    }
}
`, siteName, domain, acmeWebroot, rootDir)
	}
	certPath, keyPath := certificatePaths(domain)
	return fmt.Sprintf(`# Ekilie static site: %s
server {
    listen 80;
    listen [::]:80;
    server_name %s;

    # ACME challenge for certbot webroot renewals.
    location ^~ /.well-known/acme-challenge/ {
        root %s;
        default_type "text/plain";
    }

    location / {
        return 301 https://$host$request_uri;
    }
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name %s;

    ssl_certificate %s;
    ssl_certificate_key %s;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_prefer_server_ciphers off;

    root %s;
    index index.html;

    location / {
        try_files $uri $uri/ /index.html;
    }
}
`, siteName, domain, acmeWebroot, domain, certPath, keyPath, rootDir)
}

// generateSiteNginxConfig builds an nginx vhost that reverse-proxies the
// site's domain to the local app port. With TLS it adds a 443 server and
// redirects HTTP.
func generateSiteNginxConfig(siteName, domain string, port int, withSSL bool) string {
	if !withSSL {
		return fmt.Sprintf(`# Ekilie site: %s
server {
    listen 80;
    listen [::]:80;
    server_name %s;

    # ACME challenge for certbot webroot issuance and renewals.
    location ^~ /.well-known/acme-challenge/ {
        root %s;
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
`, siteName, domain, acmeWebroot, port)
	}
	certPath, keyPath := certificatePaths(domain)
	return fmt.Sprintf(`# Ekilie site: %s
server {
    listen 80;
    listen [::]:80;
    server_name %s;

    # ACME challenge for certbot webroot renewals.
    location ^~ /.well-known/acme-challenge/ {
        root %s;
        default_type "text/plain";
    }

    location / {
        return 301 https://$host$request_uri;
    }
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name %s;

    ssl_certificate %s;
    ssl_certificate_key %s;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_prefer_server_ciphers off;

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
`, siteName, domain, acmeWebroot, domain, certPath, keyPath, port)
}
