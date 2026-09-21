//go:build windows

package jobengine

import "fmt"

// reexecSelf is unsupported on Windows: Go's syscall.Exec is not implemented
// there. Restart the ekilied service manually after an update.
func reexecSelf() error {
	return fmt.Errorf("re-exec not supported on windows: restart the ekilied service manually to apply the update")
}
