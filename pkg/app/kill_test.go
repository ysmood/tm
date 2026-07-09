//go:build unix

package app

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/daemon"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
)

// KillSession on a live session asks its daemon to shut down: the shell
// terminates and the daemon removes the session's files itself; KillSession
// returns only once that teardown is done.
func TestControllerKillSessionLive(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(15 * time.Second)

	// Sockets need a short path (sun_path limit), so keep Runtime under /tmp.
	rt, err := os.MkdirTemp("/tmp", "tmk")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	p := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(p.EnsureDirs())
	st := store.New(p)

	sess := store.Session{
		ID: "k1", Name: "k1", Namespace: store.DefaultNamespace,
		Shell: "/bin/sh", PID: os.Getpid(), CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	ctrl := &controller{st: st}
	g.E(ctrl.KillSession("k1"))

	g.E(d.Wait()) // the daemon tore down in response to the kill

	_, gerr := st.GetSession("k1")
	g.Is(gerr, store.ErrNotFound)
}

// KillSession on a session whose daemon is unreachable (no socket to dial) and
// whose process is gone removes it from the store directly, so [kill session]
// also clears dead entries instead of erroring on them.
func TestControllerKillSessionDead(t *testing.T) {
	g := got.T(t)

	p := config.Paths{Home: t.TempDir(), Runtime: t.TempDir()}
	g.E(p.EnsureDirs())
	st := store.New(p)
	// A pid above the typical maximum should not exist (see TestProcessAlive).
	g.E(st.SaveSession(store.Session{ID: "dead", Name: "dead", Namespace: store.DefaultNamespace, PID: 1 << 30}))

	ctrl := &controller{st: st}
	g.E(ctrl.KillSession("dead"))

	_, err := st.GetSession("dead")
	g.Is(err, store.ErrNotFound)
}

// A session that is alive but unreachable — its socket vanished (e.g. to a /tmp
// cleaner) while the daemon runs on — is NOT treated as dead: the kill errors
// and keeps the record, so a running daemon and shell are never orphaned by a
// store deletion. (Reap keeps such sessions for the same reason.)
func TestControllerKillSessionUnreachableAlive(t *testing.T) {
	g := got.T(t)

	p := config.Paths{Home: t.TempDir(), Runtime: t.TempDir()}
	g.E(p.EnsureDirs())
	st := store.New(p)
	g.E(st.SaveSession(store.Session{
		ID: "lost", Name: "lost", Namespace: store.DefaultNamespace, PID: os.Getpid(),
	}))

	ctrl := &controller{st: st}
	err := ctrl.KillSession("lost")
	g.Err(err)
	g.Has(err.Error(), "alive but unreachable")

	_, gerr := st.GetSession("lost")
	g.E(gerr) // the record survives
}

// A daemon that accepts the kill request but never tears down must not hang the
// menu: KillSession gives up after killTimeout and reports it, leaving the
// record in place (the daemon may still be mid-shutdown).
func TestControllerKillSessionWedged(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(15 * time.Second)

	// Sockets need a short path (sun_path limit), so keep Runtime under /tmp.
	rt, err := os.MkdirTemp("/tmp", "tmw")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	p := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(p.EnsureDirs())
	st := store.New(p)
	g.E(st.SaveSession(store.Session{
		ID: "wedge", Name: "wedge", Namespace: store.DefaultNamespace, PID: os.Getpid(),
	}))

	// A wedged daemon: accepts the connection and swallows frames, but never
	// acts on the kill or closes.
	ln, err := proto.Listen(proto.SockAddr(p, "wedge"))
	g.E(err)

	defer func() { _ = ln.Close() }()

	g.Go(func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		_, _ = io.Copy(io.Discard, conn) // ends when KillSession closes its side
	})

	old := killTimeout
	killTimeout = 200 * time.Millisecond

	g.Cleanup(func() { killTimeout = old })

	ctrl := &controller{st: st}
	kerr := ctrl.KillSession("wedge")
	g.Err(kerr)
	g.Has(kerr.Error(), "timed out")

	_, gerr := st.GetSession("wedge")
	g.E(gerr) // the record survives: the session was not silently dropped
}
