package jobengine

import "context"

// restartService restarts a systemd service.
func restartService(ctx context.Context, name string) error {
	return run(ctx, "systemctl", "restart", name)
}
