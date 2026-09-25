//go:build !windows

package memory

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid exists on this host. Signal 0 checks
// existence without delivering anything; EPERM means it exists but belongs to
// another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
