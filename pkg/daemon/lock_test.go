package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ysmood/got"
)

// The lock's three states, as a menu probing a session sees them: no file yet
// (unknown), a running daemon holding it (held), and a daemon gone (free) —
// closing the file is how any exit, clean or not, releases it.
func TestLockStates(t *testing.T) {
	g := got.T(t)
	path := filepath.Join(t.TempDir(), "daemon.lock")

	g.Eq(ProbeLock(path), LockUnknown)

	f, err := holdLock(path)
	g.E(err)

	g.Eq(ProbeLock(path), LockHeld)
	g.Eq(ProbeLock(path), LockHeld) // probing never takes a held lock away

	g.E(f.Close())
	g.Eq(ProbeLock(path), LockFree)

	_, err = os.Stat(path)
	g.E(err) // the file outlives its holder; only its lock state changes
}

// A second daemon for the same session is refused rather than running alongside
// the first.
func TestLockRefusesSecondHolder(t *testing.T) {
	g := got.T(t)
	path := filepath.Join(t.TempDir(), "daemon.lock")

	f, err := holdLock(path)
	g.E(err)
	g.Cleanup(func() { _ = f.Close() })

	_, err = holdLock(path)
	g.Is(err, errLockHeld)
}
