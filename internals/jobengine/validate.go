package jobengine

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// sitesRoot is where every site directory lives. Nothing site-scoped may
	// resolve outside it.
	sitesRoot = "/opt/ekilie/sites"
	// nginxSitesAvailable holds one config file per site.
	nginxSitesAvailable = "/etc/nginx/sites-available"
	// supervisorConfDir holds one program config per site daemon.
	supervisorConfDir = "/etc/supervisor/conf.d"
)

// validSiteName matches lowercase alphanumeric names with dashes, starting
// with an alphanumeric, up to 63 characters. It is the only shape allowed to
// reach filesystem paths, nginx config filenames, and supervisor program
// names.
var validSiteName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// validDaemonName matches supervisor program name segments: the site name
// alphabet plus underscores.
var validDaemonName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// siteScopedActions operate on a site directory and require a valid site_name.
var siteScopedActions = map[string]bool{
	"site_create":       true,
	"site_delete":       true,
	"site_sync":         true,
	"command":           true,
	"deploy":            true,
	"read_env":          true,
	"write_env":         true,
	"update_env":        true,
	"site_raw_nginx":    true,
	"read_nginx_config": true,
	"daemon_create":     true,
	"daemon_delete":     true,
	"daemon_restart":    true,
}

// daemonActions additionally require a valid daemon name.
var daemonActions = map[string]bool{
	"daemon_create":  true,
	"daemon_delete":  true,
	"daemon_restart": true,
}

// validateSiteName rejects any name that could escape the site root when
// interpolated into a path, a config filename, or a supervisor program name.
func validateSiteName(siteName string) error {
	if !validSiteName.MatchString(siteName) {
		return fmt.Errorf("invalid site name %q: must match %s", siteName, validSiteName.String())
	}
	return nil
}

// validateDaemonName rejects daemon names that could escape the supervisor
// config directory or inject supervisorctl arguments.
func validateDaemonName(name string) error {
	if !validDaemonName.MatchString(name) {
		return fmt.Errorf("invalid daemon name %q: must match %s", name, validDaemonName.String())
	}
	return nil
}

// validateActionParams rejects job params that would reach filesystem paths,
// nginx config filenames, or supervisor program names before any work starts.
// Path builders validate again defensively.
func validateActionParams(action string, params map[string]any) error {
	if siteScopedActions[action] {
		siteName, _ := params["site_name"].(string)
		if err := validateSiteName(siteName); err != nil {
			return err
		}
	}
	if daemonActions[action] {
		name, _ := params["name"].(string)
		if err := validateDaemonName(name); err != nil {
			return err
		}
	}
	return nil
}

// isContained reports whether target stays inside root after cleaning.
func isContained(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// siteDirPath returns the site directory, guaranteed to live inside sitesRoot.
func siteDirPath(siteName string) (string, error) {
	if err := validateSiteName(siteName); err != nil {
		return "", err
	}
	dir := filepath.Join(sitesRoot, siteName)
	if !isContained(sitesRoot, dir) {
		return "", fmt.Errorf("site path %q escapes %s", dir, sitesRoot)
	}
	return dir, nil
}

// siteRepoPath returns the checkout directory for a site.
func siteRepoPath(siteName string) (string, error) {
	dir, err := siteDirPath(siteName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "current"), nil
}
