package jobengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSiteName(t *testing.T) {
	valid := []string{"a", "my-site", "abc123", "a-b-c", "trailing-", strings.Repeat("a", 63)}
	for _, name := range valid {
		if err := validateSiteName(name); err != nil {
			t.Errorf("validateSiteName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"", ".", "..", "../..", "../../etc", "../../../tmp/x", "/etc/passwd",
		"My-Site", "site_name", "site.name", "-leading",
		"a/b", `a\b`, "a b", strings.Repeat("a", 64),
	}
	for _, name := range invalid {
		if err := validateSiteName(name); err == nil {
			t.Errorf("validateSiteName(%q) = nil, want error", name)
		}
	}
}

func TestValidateDaemonName(t *testing.T) {
	for _, name := range []string{"web", "worker-1", "a_b", "a"} {
		if err := validateDaemonName(name); err != nil {
			t.Errorf("validateDaemonName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", ".", "..", "../x", "a/b", "-x", "A", "a b", strings.Repeat("a", 64)} {
		if err := validateDaemonName(name); err == nil {
			t.Errorf("validateDaemonName(%q) = nil, want error", name)
		}
	}
}

func TestSiteDirPathContainment(t *testing.T) {
	dir, err := siteDirPath("my-site")
	if err != nil {
		t.Fatalf("siteDirPath: %v", err)
	}
	if want := filepath.Join(sitesRoot, "my-site"); dir != want {
		t.Fatalf("siteDirPath = %q, want %q", dir, want)
	}

	for _, bad := range []string{"", "..", "../../etc", "/etc", "a/../../b"} {
		if got, err := siteDirPath(bad); err == nil {
			t.Errorf("siteDirPath(%q) = %q, want error", bad, got)
		}
	}
}

func TestResolveEnvPath(t *testing.T) {
	cases := []struct {
		name    string
		envPath string
		want    string
		wantErr bool
	}{
		{name: "default", want: "/opt/ekilie/sites/my-site/current/.env"},
		{name: "subdirectory", envPath: "apps/web", want: "/opt/ekilie/sites/my-site/current/apps/web/.env"},
		{name: "cleaned inside", envPath: "apps/../web", want: "/opt/ekilie/sites/my-site/current/web/.env"},
		{name: "absolute", envPath: "/etc/cron.d/x", wantErr: true},
		{name: "traversal", envPath: "../../etc", wantErr: true},
		{name: "deep traversal", envPath: "../../../../etc/cron.d", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{}
			if tc.envPath != "" {
				params["env_path"] = tc.envPath
			}
			got, err := resolveEnvPath("my-site", params)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveEnvPath = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEnvPath: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveEnvPath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateActionParams(t *testing.T) {
	if err := validateActionParams("site_delete", map[string]any{"site_name": "../../tmp/evil"}); err == nil {
		t.Fatal("traversing site name accepted")
	}
	if err := validateActionParams("site_delete", nil); err == nil {
		t.Fatal("missing site name accepted")
	}
	if err := validateActionParams("site_create", map[string]any{"site_name": "ok-site"}); err != nil {
		t.Fatalf("valid site name rejected: %v", err)
	}
	if err := validateActionParams("install_nginx", nil); err != nil {
		t.Fatalf("site-less action rejected: %v", err)
	}
	if err := validateActionParams("daemon_create", map[string]any{"site_name": "ok", "name": "../evil"}); err == nil {
		t.Fatal("traversing daemon name accepted")
	}
	if err := validateActionParams("daemon_create", map[string]any{"site_name": "ok", "name": "worker-1"}); err != nil {
		t.Fatalf("valid daemon rejected: %v", err)
	}
}

// Acceptance test for ekilie/ekilied#7: site_delete with a traversing site
// name must fail the job without touching the filesystem.
func TestExecuteRejectsTraversalSiteDelete(t *testing.T) {
	// A directory outside the sites root, plus the relative traversal that
	// would reach it from sitesRoot.
	victim, err := os.MkdirTemp("", "ekilied-traversal-victim-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(victim)

	rel, err := filepath.Rel(sitesRoot, victim)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("test setup: victim %s is not outside %s", victim, sitesRoot)
	}

	client := &fakeJobClient{}
	e := NewJobEngine(client)
	e.Execute(context.Background(), 93, "site_delete", map[string]any{"site_name": rel})

	comps := client.allCompletions()
	if len(comps) != 1 || comps[0].status != "failed" {
		t.Fatalf("completions = %+v, want one failed job", comps)
	}
	if !strings.Contains(comps[0].errMsg, "invalid site name") {
		t.Fatalf("error = %q, want invalid site name", comps[0].errMsg)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim directory was touched: %v", err)
	}
}

// Acceptance test: an absolute env_path must be rejected before any write.
func TestExecuteRejectsAbsoluteEnvPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "evil.env")

	client := &fakeJobClient{}
	e := NewJobEngine(client)
	e.Execute(context.Background(), 94, "update_env", map[string]any{
		"site_name": "my-site",
		"env_path":  target,
		"env":       map[string]any{"PWNED": "1"},
	})

	comps := client.allCompletions()
	if len(comps) != 1 || comps[0].status != "failed" {
		t.Fatalf("completions = %+v, want one failed job", comps)
	}
	if !strings.Contains(comps[0].errMsg, "env_path must be relative") {
		t.Fatalf("error = %q, want env_path rejection", comps[0].errMsg)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("absolute env_path wrote outside the site root")
	}
}
