package jobengine

import (
	"context"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
)

// allowedSystemUnits are the exact platform units the control plane may
// restart. Everything else is rejected: restarting sshd or systemd units can
// lock operators out, and restarting ekilied would kill the job mid-flight.
var allowedSystemUnits = map[string]bool{
	"nginx.service":      true,
	"supervisor.service": true,
}

// siteUnitPattern matches per-site units created by deploy scripts, named
// ekilie-<site>.service with the same alphabet as site names.
var siteUnitPattern = regexp.MustCompile(`^ekilie-[a-z0-9][a-z0-9-]{0,62}\.service$`)

// unitNamePattern is the strict shape of a unit name we are willing to pass
// to systemctl. It rejects flags, paths, and shell-like values.
var unitNamePattern = regexp.MustCompile(`^[A-Za-z0-9@:_.-]+\.service$`)

// allowedServiceUnit validates and normalizes a service_restart target. Bare
// names are mapped to <name>.service, then the unit must be nginx, supervisor,
// or a per-site ekilie-<site>.service unit.
func allowedServiceUnit(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("service is required")
	}
	if strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("invalid service name %q", raw)
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid service name %q", raw)
	}
	if !strings.Contains(name, ".") {
		name += ".service"
	}
	if !unitNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid service name %q", raw)
	}
	if allowedSystemUnits[name] || siteUnitPattern.MatchString(name) {
		return name, nil
	}
	return "", fmt.Errorf("service %q is not allowed (allowed: nginx, supervisor, ekilie-<site>)", name)
}

// restartService restarts an allowlisted systemd service. Restarts are logged
// with the requesting job ID for audit.
func restartService(ctx context.Context, out io.Writer, jobID uint, name string) error {
	unit, err := allowedServiceUnit(name)
	if err != nil {
		return err
	}
	log.Printf("job %d: restarting service %s (allowed)", jobID, unit)
	return run(ctx, out, "systemctl", "restart", unit)
}
