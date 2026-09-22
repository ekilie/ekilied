package jobengine

import (
	"context"
	"io"
)

// restartService restarts a systemd service.
func restartService(ctx context.Context, out io.Writer, name string) error {
	return run(ctx, out, "systemctl", "restart", name)
}
