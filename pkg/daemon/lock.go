package daemon

import (
	"errors"
	"os"
)

// LockState is what ProbeLock learned about a session's daemon lock.
type LockState int

const (
	// LockUnknown means the lock can't tell: there is no lock file (the daemon
	// is still starting, or predates the lock) or the filesystem doesn't support
	// locking. The caller falls back to whatever else it knows (the PID).
	LockUnknown LockState = iota
	// LockHeld means a daemon holds the lock, so it is running.
	LockHeld
	// LockFree means the lock file exists but nothing holds it, so the daemon
	// that created it has exited — cleanly or not.
	LockFree
)

// errLockHeld is returned by holdLock when another daemon already holds the
// session's lock.
var errLockHeld = errors.New("another daemon already runs this session")

// holdLock creates the session's lock file and takes its exclusive lock, which
// the daemon keeps until it exits. Closing the returned file releases it, and
// so does the process dying any other way, which is the point: the lock's state
// is a liveness signal that a reused PID can't fake (see ProbeLock).
func holdLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	if err := tryLock(f); err != nil {
		_ = f.Close()

		if lockBusy(err) {
			return nil, errLockHeld
		}

		return nil, err
	}

	return f, nil
}

// ProbeLock reports whether a daemon holds the lock at path. It takes the lock
// only for an instant when it is free (closing the file drops it), and never
// creates the file, so probing can't stand in for a daemon or block one.
func ProbeLock(path string) LockState {
	f, err := os.Open(path)
	if err != nil {
		return LockUnknown
	}

	defer func() { _ = f.Close() }()

	err = tryLock(f)

	switch {
	case err == nil:
		return LockFree
	case lockBusy(err):
		return LockHeld
	default:
		return LockUnknown
	}
}
