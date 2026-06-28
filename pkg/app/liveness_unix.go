//go:build !windows

package app

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with the given pid currently exists.
// Signal 0 performs error checking without delivering a signal: nil means the
// process exists and is ours; EPERM means it exists but is owned by another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}
