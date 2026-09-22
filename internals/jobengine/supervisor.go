package jobengine

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// installSupervisor installs supervisor via apt and enables/starts the service.
func installSupervisor(ctx context.Context, out io.Writer) error {
	if err := run(ctx, out, "apt-get", "install", "-y", "supervisor"); err != nil {
		return err
	}
	if err := run(ctx, out, "systemctl", "enable", "supervisor"); err != nil {
		return err
	}
	return run(ctx, out, "systemctl", "start", "supervisor")
}

// daemonProgramName returns the supervisor program name for a site daemon.
func daemonProgramName(siteName, name string) string {
	return fmt.Sprintf("%s-%s", siteName, name)
}

// daemonProgramPath returns the supervisor config file for a site daemon,
// guaranteed to live inside supervisorConfDir.
func daemonProgramPath(siteName, name string) (string, error) {
	if err := validateSiteName(siteName); err != nil {
		return "", err
	}
	if err := validateDaemonName(name); err != nil {
		return "", err
	}
	path := filepath.Join(supervisorConfDir, daemonProgramName(siteName, name)+".conf")
	if !isContained(supervisorConfDir, path) {
		return "", fmt.Errorf("supervisor config path %q escapes %s", path, supervisorConfDir)
	}
	return path, nil
}

// ensureUser creates a system user if it doesn't already exist.
func ensureUser(username string) {
	if exec.Command("id", "-u", username).Run() == nil {
		return // user already exists
	}
	exec.Command("useradd", "-r", "-s", "/bin/false", "-d", "/opt/ekilie", username).Run()
}

// createSupervisorConfig writes a supervisor program config and reloads supervisor.
func createSupervisorConfig(siteName, name, command string, scale int, params map[string]any) error {
	if command == "" {
		return fmt.Errorf("command is required")
	}
	if scale < 1 {
		scale = 1
	}

	path, err := daemonProgramPath(siteName, name)
	if err != nil {
		return err
	}
	dir, err := siteDirPath(siteName)
	if err != nil {
		return err
	}

	ensureUser("ekilie")

	progName := daemonProgramName(siteName, name)
	logDir := filepath.Join(dir, "logs")
	os.MkdirAll(logDir, 0755)

	// Build environment vars from params
	var envParts []string
	if envRaw, ok := params["env"].(map[string]any); ok {
		for k, v := range envRaw {
			envParts = append(envParts, fmt.Sprintf("%s=%q", k, fmt.Sprintf("%v", v)))
		}
	}

	var envSection string
	if len(envParts) > 0 {
		envSection = fmt.Sprintf("environment=%s\n", strings.Join(envParts, ","))
	}

	conf := fmt.Sprintf(`[program:%s]
command=%s
	directory=%s/current
user=ekilie
numprocs=%d
autostart=true
autorestart=true
stopwaitsecs=10
startretries=3
stdout_logfile=%s/%s.log
stderr_logfile=%s/%s-error.log
%s`, progName, command, dir, scale,
		logDir, name, logDir, name, envSection)

	if err := os.WriteFile(path, []byte(conf), 0644); err != nil {
		return fmt.Errorf("write supervisor conf: %w", err)
	}

	// Reread and update supervisor
	if out, err := exec.Command("supervisorctl", "reread").CombinedOutput(); err != nil {
		return fmt.Errorf("supervisorctl reread failed: %s", string(out))
	}
	if out, err := exec.Command("supervisorctl", "update").CombinedOutput(); err != nil {
		return fmt.Errorf("supervisorctl update failed: %s", string(out))
	}
	return nil
}

// deleteSupervisorConfig stops and removes a supervisor program and its config.
func deleteSupervisorConfig(siteName, name string) error {
	path, err := daemonProgramPath(siteName, name)
	if err != nil {
		return err
	}
	progName := daemonProgramName(siteName, name)

	// Stop and remove the program
	exec.Command("supervisorctl", "stop", progName).Run()
	exec.Command("supervisorctl", "remove", progName).Run()

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove supervisor conf: %w", err)
	}

	if out, err := exec.Command("supervisorctl", "update").CombinedOutput(); err != nil {
		return fmt.Errorf("supervisorctl update failed: %s", string(out))
	}
	return nil
}

// restartSupervisorProgram restarts a supervisor-managed program.
func restartSupervisorProgram(ctx context.Context, out io.Writer, siteName, name string) error {
	if err := validateSiteName(siteName); err != nil {
		return err
	}
	if err := validateDaemonName(name); err != nil {
		return err
	}
	progName := daemonProgramName(siteName, name)
	return run(ctx, out, "supervisorctl", "restart", progName)
}

// cleanupSupervisorForSite removes all supervisor programs and configs
// associated with the given site name. Called when a site is deleted.
func cleanupSupervisorForSite(siteName string) {
	if err := validateSiteName(siteName); err != nil {
		log.Printf("supervisor cleanup skipped: %v", err)
		return
	}
	pattern := filepath.Join(supervisorConfDir, siteName+"-*.conf")
	files, _ := filepath.Glob(pattern)
	for _, f := range files {
		progName := strings.TrimSuffix(filepath.Base(f), ".conf")
		exec.Command("supervisorctl", "stop", progName).Run()
		exec.Command("supervisorctl", "remove", progName).Run()
		os.Remove(f)
	}
	if len(files) > 0 {
		exec.Command("supervisorctl", "update").Run()
	}
}
