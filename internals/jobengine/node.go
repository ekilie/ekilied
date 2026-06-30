package jobengine

import (
	"context"
	"fmt"
)

// installNode installs Node.js 22.x and pm2 via NodeSource and apt.
func installNode(ctx context.Context, logf func(string, ...any)) error {
	logf("[system] adding NodeSource repository...")
	if err := run(ctx, "bash", "-c", "curl -fsSL https://deb.nodesource.com/setup_22.x | bash -"); err != nil {
		return fmt.Errorf("nodesource setup: %w", err)
	}
	logf("[system] installing node.js...")
	if err := run(ctx, "apt-get", "install", "-y", "nodejs"); err != nil {
		return fmt.Errorf("nodejs install: %w", err)
	}
	logf("[system] installing pm2...")
	return run(ctx, "npm", "install", "-g", "pm2")
}
