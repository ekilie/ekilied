package jobengine

import (
	"strings"
	"testing"
)

func TestGenerateSiteNginxConfig(t *testing.T) {
	httpCfg := generateSiteNginxConfig("my-site", "my-site-1-2-3-4.sslip.io", 3000, false)
	for _, want := range []string{
		"listen 80;",
		"server_name my-site-1-2-3-4.sslip.io;",
		"proxy_pass http://127.0.0.1:3000;",
		"root /var/www/html;",
	} {
		if !strings.Contains(httpCfg, want) {
			t.Errorf("http vhost missing %q", want)
		}
	}
	if strings.Contains(httpCfg, "listen 443") {
		t.Error("http vhost must not listen on 443")
	}

	httpsCfg := generateSiteNginxConfig("my-site", "my-site-1-2-3-4.sslip.io", 3000, true)
	for _, want := range []string{
		"listen 443 ssl http2;",
		"return 301 https://$host$request_uri;",
		"ssl_certificate /etc/letsencrypt/live/my-site-1-2-3-4.sslip.io/fullchain.pem;",
		"ssl_certificate_key /etc/letsencrypt/live/my-site-1-2-3-4.sslip.io/privkey.pem;",
		"proxy_pass http://127.0.0.1:3000;",
	} {
		if !strings.Contains(httpsCfg, want) {
			t.Errorf("https vhost missing %q", want)
		}
	}
}

func TestGenerateStaticSiteNginxConfig(t *testing.T) {
	root := "/opt/ekilie/sites/my-site/current/dist"
	httpCfg := generateStaticSiteNginxConfig("my-site", "example.com", root, false)
	for _, want := range []string{
		"listen 80;",
		"root " + root + ";",
		"try_files $uri $uri/ /index.html;",
	} {
		if !strings.Contains(httpCfg, want) {
			t.Errorf("static http vhost missing %q", want)
		}
	}
	if strings.Contains(httpCfg, "listen 443") {
		t.Error("static http vhost must not listen on 443")
	}

	httpsCfg := generateStaticSiteNginxConfig("my-site", "example.com", root, true)
	for _, want := range []string{
		"listen 443 ssl http2;",
		"root " + root + ";",
		"try_files $uri $uri/ /index.html;",
		"ssl_certificate /etc/letsencrypt/live/example.com/fullchain.pem;",
	} {
		if !strings.Contains(httpsCfg, want) {
			t.Errorf("static https vhost missing %q", want)
		}
	}
}

func TestValidateDomain(t *testing.T) {
	for _, domain := range []string{
		"example.com",
		"my-site-109-199-119-13.sslip.io",
		"a.b.co",
	} {
		if err := validateDomain(domain); err != nil {
			t.Errorf("validateDomain(%q) = %v, want nil", domain, err)
		}
	}
	for _, domain := range []string{
		"",
		"Example.com",
		"*.example.com",
		"localhost",
		"example..com",
		"-example.com",
		"example.com/evil",
	} {
		if err := validateDomain(domain); err == nil {
			t.Errorf("validateDomain(%q) = nil, want error", domain)
		}
	}
}
