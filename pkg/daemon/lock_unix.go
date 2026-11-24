//go:build !windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive flock on f without waiting. flock locks belong to
// the open file, so a second open of the same path — even in this process —
// conflicts, and the kernel drops the lock when the last descriptor closes.
func tryLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// lockBusy reports whether tryLock failed because someone else holds the lock.
func lockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK)
}
