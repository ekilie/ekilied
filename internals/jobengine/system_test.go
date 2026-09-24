package jobengine

import (
	"context"
	"strings"
	"testing"
)

// Regression tests for ekilie/ekilied#11: service_restart must only accept an
// allowlist of units, so a compromised or buggy control plane cannot restart
// sshd, ekilied, or arbitrary systemd units as root.
func TestAllowedServiceUnit(t *testing.T) {
	valid := map[string]string{
		"nginx":                  "nginx.service",
		"nginx.service":          "nginx.service",
		"supervisor":             "supervisor.service",
		"supervisor.service":     "supervisor.service",
		"ekilie-my-site":         "ekilie-my-site.service",
		"ekilie-my-site.service": "ekilie-my-site.service",
		"  nginx  ":              "nginx.service",
	}
	for in, want := range valid {
		got, err := allowedServiceUnit(in)
		if err != nil {
			t.Errorf("allowedServiceUnit(%q) = error %v, want %q", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("allowedServiceUnit(%q) = %q, want %q", in, got, want)
		}
	}

	invalid := []string{
		"",
		"ekilied",
		"ekilied.service",
		"sshd",
		"ssh.service",
		"docker",
		"systemd-journald",
		"systemd-journald.service",
		"dbus",
		"cron",
		"-h",
		"../evil",
		"foo/bar",
		"nginx/../sshd",
		`foo\bar`,
		"foo@bar",
		"ekilie-",
		"ekilie--x",
		"ekilie-../../sshd",
		"EKILIE-my-site",
		"ssh.service",
	}
	for _, in := range invalid {
		if got, err := allowedServiceUnit(in); err == nil {
			t.Errorf("allowedServiceUnit(%q) = %q, want an error", in, got)
		}
	}
}

// A disallowed restart fails the job before systemctl is ever invoked.
func TestExecuteRejectsDisallowedServiceRestart(t *testing.T) {
	client := &fakeJobClient{}
	e := NewJobEngine(client)

	e.Execute(context.Background(), 101, "service_restart", map[string]any{"service": "ekilied"})

	comps := client.allCompletions()
	if len(comps) != 1 || comps[0].status != "failed" {
		t.Fatalf("completions = %+v, want one failed job", comps)
	}
	if !strings.Contains(comps[0].errMsg, "not allowed") {
		t.Fatalf("error = %q, want the allowlist rejection", comps[0].errMsg)
	}
}
