//go:build unix

package app_test

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

// spawnAndEnter starts a session aaa, runs the real tm binary under a PTY, and
// enters the session with all history. It returns the PTY, the accumulated
// output, the store, the session daemon's pid, and the binary path.
func spawnAndEnter(g got.G, t *testing.T) (gopty.Pty, *safeBuilder, *store.Store, int, string) {
	rt, err := os.MkdirTemp("/tmp", "tmkc")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)

	p, err := config.New()
	g.E(err)
	g.E(p.EnsureDirs())
	st := store.New(p)
	sess := store.Session{
		ID: "aaa", Name: "aaa", Namespace: store.DefaultNamespace,
		Shell: "/bin/sh", CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))
	g.E(app.SpawnWith(bin, p, sess))

	// The daemon records its pid before signalling ready, so it is set by now.
	spawned, err := st.GetSession("aaa")
	g.E(err)
	g.Gt(spawned.PID, 0)

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))
	g.Cleanup(func() { _ = pt.Close() })

	c := pt.Command(bin)
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	g.True(waitForText(buf, "aaa", 10*time.Second))

	_, err = pt.Write([]byte("\r"))
	g.E(err)
	time.Sleep(400 * time.Millisecond)

	_, err = pt.Write([]byte("\r")) // "All history"
	g.E(err)
	g.True(waitForText(buf, "[tm entered session", 10*time.Second))

	return pt, buf, st, spawned.PID, bin
}

// pidGone reports whether pid stops existing before the timeout (the daemon is
// reaped by init asynchronously, so existence is polled).
func pidGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// recordGone reports whether the session's store record disappears before the
// timeout (the daemon deletes it during its teardown).
func recordGone(st *store.Store, id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for {
		if _, err := st.GetSession(id); errors.Is(err, store.ErrNotFound) {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// TestKillCurrentSessionFromMenu drives the real binary: [kill session] aimed at
// the session the menu was opened over (Ctrl-\) ends it — shell and daemon — and
// drops back to the top-level menu with a kill notice, instead of resuming a
// dead session or leaving the terminal in a broken state.
func TestKillCurrentSessionFromMenu(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	pt, buf, st, pid, _ := spawnAndEnter(g, t)

	// Ctrl-\ opens the menu over the session; the fresh render is matched past a
	// mark since the first top-level menu already showed the same labels.
	mark := len(buf.String())

	_, err := pt.Write([]byte{0x1c})
	g.E(err)
	g.True(waitForTextFrom(buf, mark, "[kill session]", 10*time.Second))

	_, err = pt.Write([]byte("ks\r"))
	g.E(err)
	time.Sleep(400 * time.Millisecond)

	menuMark := len(buf.String())

	_, err = pt.Write([]byte("\r")) // the only row: the current session
	g.E(err)

	// The kill is noted on the terminal and the top-level menu reopens.
	g.True(waitForText(buf, "[tm killed session", 10*time.Second))
	g.True(waitForTextFrom(buf, menuMark, "[new session]", 10*time.Second))

	// Session record and daemon are gone.
	g.True(recordGone(st, "aaa", 10*time.Second))
	g.True(pidGone(pid, 10*time.Second))

	// The reopened menu is the top level: esc leaves tm.
	_, err = pt.Write([]byte{0x1b})
	g.E(err)
	g.True(waitForText(buf, "[tm exited]", 10*time.Second))
}

// TestKillCurrentSessionFromWithin drives the hairiest path: a tm run from
// INSIDE the session's shell kills its own session. The kill takes the shell —
// and that tm process — with it; the outer relay sees the session end and falls
// back to the top-level menu, so the terminal is never left stranded.
func TestKillCurrentSessionFromWithin(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	pt, buf, st, pid, bin := spawnAndEnter(g, t)

	// Make sure the shell is at its prompt before typing a command.
	_, err := pt.Write([]byte("echo SH-READY\r"))
	g.E(err)
	g.True(waitForText(buf, "SH-READY", 10*time.Second))

	// Run tm inside the session; its menu is framed inside aaa.
	mark := len(buf.String())

	_, err = pt.Write([]byte(bin + "\r"))
	g.E(err)
	g.True(waitForTextFrom(buf, mark, "[kill session]", 10*time.Second))

	_, err = pt.Write([]byte("ks\r"))
	g.E(err)
	time.Sleep(400 * time.Millisecond)

	menuMark := len(buf.String())

	_, err = pt.Write([]byte("\r")) // the only row: the session tm runs inside
	g.E(err)

	// The session ends out from under the inner tm: the outer relay reports the
	// exit and falls back to the top-level menu.
	sawExit := waitForText(buf, "[tm exited session", 10*time.Second)
	g.Desc("the outer relay must report the session's end; fresh output: %q",
		buf.String()[menuMark:]).True(sawExit)
	g.True(waitForTextFrom(buf, menuMark, "[new session]", 10*time.Second))

	g.True(recordGone(st, "aaa", 10*time.Second))
	g.True(pidGone(pid, 10*time.Second))

	_, err = pt.Write([]byte{0x1b})
	g.E(err)

	sawBye := waitForText(buf, "[tm exited]", 10*time.Second)
	g.Desc("esc at the top-level menu must leave tm; output after the kill: %q",
		buf.String()[menuMark:]).True(sawBye)
}
