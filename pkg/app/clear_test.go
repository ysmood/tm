//go:build unix

package app

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

// ClearHistory on a live session asks its daemon to wipe the recorded
// scrollback: the log file is truncated (and the in-memory ring emptied) while
// the session — record, daemon and shell — keeps running.
func TestControllerClearHistoryLive(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(15 * time.Second)

	// Sockets need a short path (sun_path limit), so keep Runtime under /tmp.
	rt, err := os.MkdirTemp("/tmp", "tmc")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	p := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(p.EnsureDirs())
	st := store.New(p)

	// Shell is left empty so the daemon falls back to its default shell; the test
	// never types into it.
	sess := store.Session{
		ID: "c1", Name: "c1", Namespace: store.DefaultNamespace,
		PID: os.Getpid(), CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))

	// History from before this daemon run; O_APPEND keeps it until the clear.
	g.E(os.WriteFile(p.LogFile(sess.ID), []byte("SECRET-MARK\n"), 0o600))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	ctrl := &controller{st: st}
	g.E(ctrl.ClearHistory("c1"))

	// The secret is gone from the log (the shell may have printed a prompt since,
	// so absence is checked rather than emptiness) and the session survives.
	data, rerr := os.ReadFile(p.LogFile(sess.ID))
	g.E(rerr)
	g.True(!strings.Contains(string(data), "SECRET-MARK"))

	_, gerr := st.GetSession("c1")
	g.E(gerr)
}

// ClearHistory on a session whose daemon is unreachable (no socket to dial) and
// whose process is gone truncates the leftover log file directly — the only
// place a dead session's history lives — and keeps the record for Reap.
func TestControllerClearHistoryDead(t *testing.T) {
	g := got.T(t)

	p := config.Paths{Home: t.TempDir(), Runtime: t.TempDir()}
	g.E(p.EnsureDirs())
	st := store.New(p)
	// A pid above the typical maximum should not exist (see TestProcessAlive).
	g.E(st.SaveSession(store.Session{ID: "c2", Name: "c2", Namespace: store.DefaultNamespace, PID: 1 << 30}))
	g.E(os.WriteFile(p.LogFile("c2"), []byte("SECRET-MARK\n"), 0o600))

	ctrl := &controller{st: st}
	g.E(ctrl.ClearHistory("c2"))

	data, rerr := os.ReadFile(p.LogFile("c2"))
	g.E(rerr)
	g.Len(data, 0)

	_, gerr := st.GetSession("c2")
	g.E(gerr) // the record survives: clearing is not a kill
}

// A session that is alive but unreachable — its socket vanished while the
// daemon runs on — is NOT half-cleared: the daemon's in-memory ring cannot be
// reached, so truncating the file alone would still leave the history
// replayable. The clear errors and leaves everything in place.
func TestControllerClearHistoryUnreachableAlive(t *testing.T) {
	g := got.T(t)

	p := config.Paths{Home: t.TempDir(), Runtime: t.TempDir()}
	g.E(p.EnsureDirs())
	st := store.New(p)
	g.E(st.SaveSession(store.Session{
		ID: "c3", Name: "c3", Namespace: store.DefaultNamespace, PID: os.Getpid(),
	}))
	g.E(os.WriteFile(p.LogFile("c3"), []byte("SECRET-MARK\n"), 0o600))

	ctrl := &controller{st: st}
	err := ctrl.ClearHistory("c3")
	g.Err(err)
	g.Has(err.Error(), "alive but unreachable")

	data, rerr := os.ReadFile(p.LogFile("c3"))
	g.E(rerr)
	g.Has(string(data), "SECRET-MARK") // untouched: no silent half-clear
}

// A daemon that accepts the clear request but never acts must not hang the
// menu: ClearHistory gives up after killTimeout and reports it.
func TestControllerClearHistoryWedged(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(15 * time.Second)

	// Sockets need a short path (sun_path limit), so keep Runtime under /tmp.
	rt, err := os.MkdirTemp("/tmp", "tmcw")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	p := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(p.EnsureDirs())
	st := store.New(p)
	g.E(st.SaveSession(store.Session{
		ID: "c4", Name: "c4", Namespace: store.DefaultNamespace, PID: os.Getpid(),
	}))

	// A wedged daemon: accepts the connection and swallows frames, but never
	// acts on the clear or closes.
	ln, err := proto.Listen(proto.SockAddr(p, "c4"))
	g.E(err)

	defer func() { _ = ln.Close() }()

	g.Go(func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		_, _ = io.Copy(io.Discard, conn) // ends when ClearHistory closes its side
	})

	old := killTimeout
	killTimeout = 200 * time.Millisecond

	g.Cleanup(func() { killTimeout = old })

	ctrl := &controller{st: st}
	cerr := ctrl.ClearHistory("c4")
	g.Err(cerr)
	g.Has(cerr.Error(), "timed out")
}
