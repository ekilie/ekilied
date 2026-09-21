//go:build unix

package jobengine

import (
	"fmt"
	"os"
	"syscall"
)

// reexecSelf replaces the running process image with the current binary.
// On success it never returns. The binary was just swapped into place by
// SelfUpdate; the stat check below catches a missing/non-executable file
// before we abandon the working old process.
func reexecSelf() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}
	fi, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("stat %s: %w", self, err)
	}
	if fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", self)
	}
	if err := syscall.Exec(self, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", self, err)
	}
	return nil // unreachable
}
