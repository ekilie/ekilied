package jobengine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// supervisorConfDir is the directory where supervisor program configs are stored.
const supervisorConfDir = "/etc/supervisor/conf.d"

// installSupervisor installs supervisor via apt and enables/starts the service.
func installSupervisor(ctx context.Context) error {
	if err := run(ctx, "apt-get", "install", "-y", "supervisor"); err != nil {
		return err
	}
	if err := run(ctx, "systemctl", "enable", "supervisor"); err != nil {
		return err
	}
	return run(ctx, "systemctl", "start", "supervisor")
}

// daemonProgramName returns the supervisor program name for a site daemon.
func daemonProgramName(siteName, name string) string {
	return fmt.Sprintf("%s-%s", siteName, name)
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

	ensureUser("ekilie")

	progName := daemonProgramName(siteName, name)
	siteDir := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	logDir := siteDir + "/logs"
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
%s`, progName, command, siteDir, scale,
		logDir, name, logDir, name, envSection)

	path := filepath.Join(supervisorConfDir, progName+".conf")
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
	progName := daemonProgramName(siteName, name)
	path := filepath.Join(supervisorConfDir, progName+".conf")

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
func restartSupervisorProgram(ctx context.Context, siteName, name string) error {
	progName := daemonProgramName(siteName, name)
	return run(ctx, "supervisorctl", "restart", progName)
}

// cleanupSupervisorForSite removes all supervisor programs and configs
// associated with the given site name. Called when a site is deleted.
func cleanupSupervisorForSite(siteName string) {
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
