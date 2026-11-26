//go:build unix

package app

// The controller's two daemon requests — /kill-session and /clear-history — in
// each of the four states a session can be in when one arrives: a live daemon that
// answers, a session whose process is gone, a session that is alive but whose
// socket has vanished, and a daemon that accepts the request and then goes silent.
// Together they are the contract of controller.request (see menu.go): only a
// process that is really gone counts as dead, and a daemon that never answers must
// not hang the menu.

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/daemon"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
)

// deadPID is above the typical process-id maximum, so nothing has it (see
// TestProcessAlive) — it stands for a session whose daemon is gone.
const deadPID = 1 << 30

// secretMark is content a test plants in a session's log so a clear can be shown
// to have removed it — the reason /clear-history exists.
const secretMark = "SECRET-MARK"

// ctl is a controller over an isolated store, with the paths behind it.
type ctl struct {
	got.G
	*controller

	paths config.Paths
	st    *store.Store
}

// newCtl builds a controller over its own store. It does not mark the test
// parallel: the tests that shorten the package-level killTimeout must not run
// alongside the ones that read it, and a non-parallel test runs to completion
// before any parallel one starts.
func newCtl(t *testing.T) *ctl {
	g := got.T(t)
	g.PanicAfter(30 * time.Second)

	// Sockets need a short path (the OS sun_path limit), so Runtime lives under
	// /tmp rather than in the test's temp dir.
	rt, err := os.MkdirTemp("/tmp", "tmctl")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	p := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(p.EnsureDirs())

	st := store.New(p)

	return &ctl{G: g, paths: p, st: st, controller: &controller{st: st}}
}

// setupCtl is newCtl for a test that leaves package state alone, so it can run
// alongside the others. It goes parallel first, so newCtl's deadline counts the
// test's own run, not the wait for a free slot behind slower tests.
func setupCtl(t *testing.T) *ctl {
	t.Parallel()

	return newCtl(t)
}

// save records a session with the given id and pid without starting anything, so a
// test can present the controller with a session in a state it could not reach by
// running one (a stale pid, a missing socket).
func (c *ctl) save(id string, pid int) {
	c.Helper()

	c.E(c.st.SaveSession(store.Session{
		ID: id, Name: id, Namespace: store.DefaultNamespace,
		PID: pid, CreatedAt: time.Unix(1, 0),
	}))
}

// startDaemon runs a real daemon for the already-recorded session id and returns
// it. The shell is left to the daemon's default; these tests never type into it.
func (c *ctl) startDaemon(id string) *daemon.Daemon {
	c.Helper()

	sess, err := c.st.GetSession(id)
	c.E(err)

	d, err := daemon.Start(c.paths, sess)
	c.E(err)
	c.Cleanup(func() { _ = d.Close() })

	return d
}

// wedgeDaemon stands in for a daemon that accepts a request and then never acts on
// it or closes: it listens on the session's socket and swallows whatever arrives.
// killTimeout is shortened for the test, since the point is that the request gives
// up at all, not how long it waits.
func (c *ctl) wedgeDaemon(id string) {
	c.Helper()

	ln, err := proto.Listen(proto.SockAddr(c.paths, id))
	c.E(err)
	c.Cleanup(func() { _ = ln.Close() })

	c.Go(func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		_, _ = io.Copy(io.Discard, conn) // ends when the requester closes its side
	})

	old := killTimeout
	killTimeout = 200 * time.Millisecond

	c.Cleanup(func() { killTimeout = old })
}

// plantSecret writes content into a session's log that a clear must remove. Written
// directly, so it is there before any daemon opens the file (which it does with
// O_APPEND, keeping it until the clear).
func (c *ctl) plantSecret(id string) {
	c.Helper()
	c.E(os.WriteFile(c.paths.LogFile(id), []byte(secretMark+"\n"), 0o600))
}

// log is the session's recorded history as it stands now.
func (c *ctl) log(id string) string {
	c.Helper()

	data, err := os.ReadFile(c.paths.LogFile(id))
	c.E(err)

	return string(data)
}

// KillSession on a live session asks its daemon to shut down: the shell terminates
// and the daemon removes the session's files itself; KillSession returns only once
// that teardown is done.
func TestControllerKillSessionLive(t *testing.T) {
	c := setupCtl(t)
	c.save("k1", os.Getpid()) // recorded live, so the unreachable fallback can't fire
	d := c.startDaemon("k1")

	c.E(c.KillSession("k1"))

	c.E(d.Wait()) // the daemon tore down in response to the kill

	_, err := c.st.GetSession("k1")
	c.Err(err)
}

// KillSession on a session whose daemon is unreachable (no socket to dial) and
// whose process is gone removes it from the store directly, so /kill-session also
// clears dead entries instead of erroring on them.
func TestControllerKillSessionDead(t *testing.T) {
	c := setupCtl(t)
	c.save("dead", deadPID)

	c.E(c.KillSession("dead"))

	_, err := c.st.GetSession("dead")
	c.Err(err)
}

// A session that is alive but unreachable — its socket vanished (e.g. to a /tmp
// cleaner) while the daemon runs on — is NOT treated as dead: the kill errors and
// keeps the record, so a running daemon and shell are never orphaned by a store
// deletion. (Reap keeps such sessions for the same reason.)
func TestControllerKillSessionUnreachableAlive(t *testing.T) {
	c := setupCtl(t)
	c.save("lost", os.Getpid())

	err := c.KillSession("lost")
	c.Err(err)
	c.Has(err.Error(), "alive but unreachable")

	_, gerr := c.st.GetSession("lost")
	c.E(gerr) // the record survives
}

// A daemon that accepts the kill request but never tears down must not hang the
// menu: KillSession gives up after killTimeout and reports it, leaving the record
// in place (the daemon may still be mid-shutdown).
func TestControllerKillSessionWedged(t *testing.T) {
	c := newCtl(t)
	c.save("wedge", os.Getpid())
	c.wedgeDaemon("wedge")

	err := c.KillSession("wedge")
	c.Err(err)
	c.Has(err.Error(), "timed out")

	_, gerr := c.st.GetSession("wedge")
	c.E(gerr) // the record survives: the session was not silently dropped
}

// ClearHistory on a live session asks its daemon to wipe the recorded scrollback:
// the log file is truncated while the session — record, daemon and shell — keeps
// running.
func TestControllerClearHistoryLive(t *testing.T) {
	c := setupCtl(t)
	c.save("c1", os.Getpid())
	c.plantSecret("c1")
	c.startDaemon("c1")

	c.E(c.ClearHistory("c1"))

	// The secret is gone from the log (the shell may have printed a prompt since, so
	// absence is checked rather than emptiness) and the session survives.
	c.Desc("the cleared log still holds the secret").
		False(strings.Contains(c.log("c1"), secretMark))

	_, err := c.st.GetSession("c1")
	c.E(err)
}

// ClearHistory on a session whose daemon is unreachable and whose process is gone
// truncates the leftover log file directly — the only place a dead session's
// history lives — and keeps the record for Reap.
func TestControllerClearHistoryDead(t *testing.T) {
	c := setupCtl(t)
	c.save("c2", deadPID)
	c.plantSecret("c2")

	c.E(c.ClearHistory("c2"))

	c.Len(c.log("c2"), 0)

	_, err := c.st.GetSession("c2")
	c.E(err) // the record survives: clearing is not a kill
}

// A session that is alive but unreachable — its socket vanished while the daemon
// runs on — is NOT half-cleared: the daemon keeps appending to the log behind our
// back, so truncating the file without it would leave the session in an odd
// half-wiped state. The clear errors and leaves everything in place.
func TestControllerClearHistoryUnreachableAlive(t *testing.T) {
	c := setupCtl(t)
	c.save("c3", os.Getpid())
	c.plantSecret("c3")

	err := c.ClearHistory("c3")
	c.Err(err)
	c.Has(err.Error(), "alive but unreachable")

	c.Has(c.log("c3"), secretMark) // untouched: no silent half-clear
}

// A daemon that accepts the clear request but never acts must not hang the menu:
// ClearHistory gives up after killTimeout and reports it.
func TestControllerClearHistoryWedged(t *testing.T) {
	c := newCtl(t)
	c.save("c4", os.Getpid())
	c.wedgeDaemon("c4")

	err := c.ClearHistory("c4")
	c.Err(err)
	c.Has(err.Error(), "timed out")
}

// staleLock leaves the lock file a dead daemon of this tm would: present, but
// held by nothing.
func (c *ctl) staleLock(id string) {
	c.Helper()
	c.E(os.WriteFile(c.paths.LockFile(id), nil, 0o600))
}

// A dead session whose recorded PID now belongs to an unrelated live process —
// after a reboot, or once PIDs wrap — is still known dead by its free lock, so
// /kill-session removes it instead of refusing it as "alive but unreachable".
// This process's own PID stands in for the reused one.
func TestControllerKillSessionReusedPID(t *testing.T) {
	c := setupCtl(t)
	c.save("ghost", os.Getpid())
	c.staleLock("ghost")

	c.E(c.KillSession("ghost"))

	_, err := c.st.GetSession("ghost")
	c.Err(err)
}

// Reap (which the menu runs every time it opens) drops a dead session with a
// reused PID along with its directory and log, and keeps a live one.
func TestControllerReapReusedPID(t *testing.T) {
	c := setupCtl(t)
	c.save("ghost", os.Getpid())
	c.staleLock("ghost")
	c.plantSecret("ghost")

	c.save("live", os.Getpid())
	c.startDaemon("live")

	c.Eq(c.Reap(), 1)

	_, err := os.Stat(c.paths.SessionDir("ghost"))
	c.True(os.IsNotExist(err))

	_, err = c.st.GetSession("live")
	c.E(err)
}

// A live daemon that lost its socket (say, to a /tmp cleaner) still holds its
// lock, so it is kept as alive — the lock never turns a running session dead.
func TestControllerKillSessionLostSocket(t *testing.T) {
	c := setupCtl(t)
	c.save("lost", os.Getpid())
	c.startDaemon("lost")
	c.E(os.Remove(c.paths.SockFile("lost")))

	err := c.KillSession("lost")
	c.Err(err)
	c.Has(err.Error(), "alive but unreachable")

	c.Eq(c.Reap(), 0)
}
