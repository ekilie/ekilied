package jobengine

import (
	"context"
	"fmt"
)

// installBun installs Bun (JavaScript runtime + package manager) on Debian/Ubuntu-based systems.
func installBun(ctx context.Context, logf func(string, ...any)) error {
	// Install prerequisites
	logf("[system] installing prerequisites (unzip, curl)...")
	if err := run(ctx, "apt-get", "update", "-qq"); err != nil {
		return fmt.Errorf("apt update: %w", err)
	}
	if err := run(ctx, "apt-get", "install", "-y", "curl", "unzip"); err != nil {
		return fmt.Errorf("prerequisites install: %w", err)
	}

	// Install Bun using the official installer
	logf("[system] installing Bun via official script...")
	if err := run(ctx, "bash", "-c", "curl -fsSL https://bun.sh/install | bash"); err != nil {
		return fmt.Errorf("bun install script: %w", err)
	}

	// Make Bun available system-wide (for all users)
	logf("[system] making Bun available system-wide...")
	if err := run(ctx, "bash", "-c", `
		if [ -f "$HOME/.bun/bin/bun" ]; then
			ln -sf "$HOME/.bun/bin/bun" /usr/local/bin/bun
		fi
	`); err != nil {
		logf("[warn] could not create system-wide symlink for bun: %v", err)
		// Non-fatal, we continue
	}

	logf("[system] Bun installation completed successfully")
	return nil
}