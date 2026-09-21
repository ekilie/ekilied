//go:build !unix && !windows

package jobengine

import "fmt"

// reexecSelf is only implemented on unix (syscall.Exec) with an explicit
// fallback error on windows. Any other platform must restart manually.
func reexecSelf() error {
	return fmt.Errorf("re-exec not supported on this platform: restart ekilied manually to apply the update")
}
