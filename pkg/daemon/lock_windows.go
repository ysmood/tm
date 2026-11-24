//go:build windows

package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes an exclusive lock on f's first byte without waiting. Windows
// locks belong to the handle, so a second handle on the same file — even in this
// process — conflicts, and the OS drops the lock when the handle closes or the
// process ends. Nothing reads the lock file, so the lock's mandatory nature is
// harmless.
func tryLock(f *os.File) error {
	var ol windows.Overlapped

	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}

// lockBusy reports whether tryLock failed because someone else holds the lock.
func lockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
