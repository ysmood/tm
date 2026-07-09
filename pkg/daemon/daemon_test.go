//go:build unix

package daemon_test

import (
	"io"
	"net"
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

func setupDaemon(t *testing.T) (got.G, *store.Store, config.Paths) {
	g := got.T(t)
	g.PanicAfter(15 * time.Second)
	// Sockets need a short path (sun_path limit), so keep Runtime under /tmp;
	// metadata/logs can live in the deeper test temp dir.
	rt, err := os.MkdirTemp("/tmp", "tmd")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	p := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(p.EnsureDirs())

	return g, store.New(p), p
}

func makeSession(g got.G, st *store.Store, id string) store.Session {
	sess := store.Session{
		ID:        id,
		Name:      id,
		Namespace: store.DefaultNamespace,
		Shell:     "/bin/sh",
		PID:       1,
		CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))

	return sess
}

// dialAttach connects, sends an Attach, and returns the framed conn plus the raw
// net.Conn (for read deadlines).
func dialAttach(g got.G, addr string, att proto.Attach) (net.Conn, *proto.Conn) {
	nc, err := proto.Dial(addr)
	g.E(err)

	c := proto.NewConn(nc)
	g.E(c.Write(proto.MsgAttach, att.Encode()))

	return nc, c
}

// readUntil reports whether Output containing want arrives before an Exit/timeout.
func readUntil(nc net.Conn, c *proto.Conn, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	var acc strings.Builder

	for {
		_ = nc.SetReadDeadline(deadline)

		mt, payload, err := c.Read()
		if err != nil {
			return false
		}

		switch mt {
		case proto.MsgOutput:
			acc.Write(payload)

			if strings.Contains(acc.String(), want) {
				return true
			}
		case proto.MsgExit:
			return strings.Contains(acc.String(), want)
		}
	}
}

func TestAttachInputOutputAndExit(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "echo1")

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistNone, Cols: 80, Rows: 24})
	defer nc.Close()

	g.E(c.Write(proto.MsgInput, []byte("echo hello-tm\n")))
	found := readUntil(nc, c, "hello-tm", 10*time.Second)
	g.True(found)

	// Exiting the shell ends the session and removes its metadata.
	g.E(c.Write(proto.MsgInput, []byte("exit\n")))
	g.E(d.Wait())

	_, gerr := st.GetSession(sess.ID)
	g.Is(gerr, store.ErrNotFound)
}

// The daemon exports the session id to its shell as config.EnvSession, so a tm
// launched inside the session can tell which session it is running in.
func TestSessionShellHasSessionEnv(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "envid")

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistNone, Cols: 80, Rows: 24})
	defer nc.Close()

	// The expanded value (ENVMARK-envid-END) appears only in the command's output,
	// not the terminal echo of the unexpanded command, so a match proves the shell
	// saw the variable set.
	g.E(c.Write(proto.MsgInput, []byte("echo ENVMARK-$"+config.EnvSession+"-END\n")))
	g.True(readUntil(nc, c, "ENVMARK-envid-END", 10*time.Second))
}

// A MsgSwitch connection tells the daemon to hand its attached client (the
// relay) to another session: the daemon forwards it as MsgSwitchTo and does not
// displace the client.
func TestSwitchForwardedToClient(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "swsrc")

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistNone, Cols: 80, Rows: 24})
	defer nc.Close()

	// Make sure the client is fully registered and serving before switching, so
	// the forward has a client to target.
	g.E(c.Write(proto.MsgInput, []byte("echo ready-marker\n")))
	g.True(readUntil(nc, c, "ready-marker", 10*time.Second))

	// A separate (non-attaching) connection requests the switch, then closes.
	ctl, derr := proto.Dial(d.Addr())
	g.E(derr)

	cc := proto.NewConn(ctl)
	g.E(cc.Write(proto.MsgSwitch, proto.SwitchTarget{ID: "dest", Hist: proto.HistAll, Lines: 7}.Encode()))

	_ = ctl.Close()

	// The attached client receives the forwarded target (reading past any shell
	// startup output), proving it was not displaced.
	tgt := readSwitchTo(g, nc, c, 5*time.Second)
	g.Eq(tgt.ID, "dest")
	g.Eq(tgt.Hist, proto.HistAll)
	g.Eq(int(tgt.Lines), 7)
}

// A MsgKill connection ends the session without attaching: the shell is
// terminated, the attached client is told the session is over (MsgExit), the
// session's metadata is removed, and the killer's connection closes only once
// teardown is done.
func TestKillSession(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "kill1")

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistNone, Cols: 80, Rows: 24})
	defer nc.Close()

	// Make sure the client is fully registered before the kill, so the MsgExit
	// notification has a client to reach.
	g.E(c.Write(proto.MsgInput, []byte("echo kill-ready\n")))
	g.True(readUntil(nc, c, "kill-ready", 10*time.Second))

	// A separate (non-attaching) connection requests the kill, then blocks until
	// the daemon closes it — the signal that teardown finished.
	ctl, derr := proto.Dial(d.Addr())
	g.E(derr)

	defer ctl.Close()

	cc := proto.NewConn(ctl)
	g.E(cc.Write(proto.MsgKill, nil))

	_ = ctl.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _, rerr := cc.Read()
	g.Is(rerr, io.EOF) // not a timeout: the daemon closed after tearing down

	g.E(d.Wait())

	// The attached client was told the session is over, not silently dropped.
	g.True(readExit(nc, c, 10*time.Second))

	// And the session's metadata is gone.
	_, gerr := st.GetSession(sess.ID)
	g.Is(gerr, store.ErrNotFound)
}

// readExit reads frames until a MsgExit arrives, reporting whether it did.
func readExit(nc net.Conn, c *proto.Conn, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for {
		_ = nc.SetReadDeadline(deadline)

		mt, _, err := c.Read()
		if err != nil {
			return false
		}

		if mt == proto.MsgExit {
			return true
		}
	}
}

// readSwitchTo reads frames until a MsgSwitchTo arrives, skipping output frames.
func readSwitchTo(g got.G, nc net.Conn, c *proto.Conn, timeout time.Duration) proto.SwitchTarget {
	deadline := time.Now().Add(timeout)

	for {
		_ = nc.SetReadDeadline(deadline)

		mt, payload, err := c.Read()
		g.E(err)

		if mt == proto.MsgSwitchTo {
			tgt, derr := proto.DecodeSwitchTarget(payload)
			g.E(derr)

			return tgt
		}
	}
}

// TestAttachAllHistoryExceedsMaxPayload guards the chunked replay: a session
// whose scrollback is larger than a single frame (proto.MaxPayload) must still
// replay in full on a HistAll attach. Sending it as one frame would be rejected
// as "payload too large", drop the connection, and bounce the attach back to the
// menu — making a busy session impossible to enter.
func TestAttachAllHistoryExceedsMaxPayload(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "bighist")

	// Seed the log with a marker placed beyond the single-frame limit, so it can
	// only arrive if the replay spans multiple frames.
	const tailMarker = "TAIL-BEYOND-MAXPAYLOAD"

	seed := append(make([]byte, 0, 2*proto.MaxPayload+len(tailMarker)),
		[]byte(strings.Repeat("x", 2*proto.MaxPayload))...)
	seed = append(seed, []byte(tailMarker+"\n")...)
	g.E(os.WriteFile(p.LogFile(sess.ID), seed, 0o600))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistAll, Cols: 80, Rows: 24})
	defer nc.Close()

	g.True(readUntil(nc, c, tailMarker, 10*time.Second))
}

func TestDetachThenReattach(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "persist1")

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	// First attach: produce a marker, then detach.
	nc1, c1 := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistNone, Cols: 80, Rows: 24})
	g.E(c1.Write(proto.MsgInput, []byte("echo first-attach\n")))
	found := readUntil(nc1, c1, "first-attach", 10*time.Second)
	g.True(found)
	g.E(c1.Write(proto.MsgDetach, nil))
	nc1.Close()

	// Session still alive: reattach with full history and see the earlier marker.
	nc2, c2 := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistAll, Cols: 80, Rows: 24})
	defer nc2.Close()

	found = readUntil(nc2, c2, "first-attach", 10*time.Second)
	g.True(found)
}
