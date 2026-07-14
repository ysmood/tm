//go:build unix

package daemon_test

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Cols: 80, Rows: 24})
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

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Cols: 80, Rows: 24})
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

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Cols: 80, Rows: 24})
	defer nc.Close()

	// Make sure the client is fully registered and serving before switching, so
	// the forward has a client to target.
	g.E(c.Write(proto.MsgInput, []byte("echo ready-marker\n")))
	g.True(readUntil(nc, c, "ready-marker", 10*time.Second))

	// A separate (non-attaching) connection requests the switch, then closes.
	ctl, derr := proto.Dial(d.Addr())
	g.E(derr)

	cc := proto.NewConn(ctl)
	g.E(cc.Write(proto.MsgSwitch, proto.SwitchTarget{ID: "dest", Name: "dest name"}.Encode()))

	_ = ctl.Close()

	// The attached client receives the forwarded target (reading past any shell
	// startup output), proving it was not displaced.
	tgt := readSwitchTo(g, nc, c, 5*time.Second)
	g.Eq(tgt.ID, "dest")
	g.Eq(tgt.Name, "dest name")
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

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Cols: 80, Rows: 24})
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

// A MsgClear connection wipes the session's recorded history without attaching
// or ending anything: the log file is truncated — so a later attach replays none
// of it (e.g. a secret echoed to the terminal) — while the shell keeps running.
// The requester's connection closes only once the wipe is done. While the wiped
// log is still empty, an attach that asked for a replay gets a dim hint instead
// of a blank screen (the shell prints its prompt only when asked, so there is no
// cursor or prompt to see); the hint retires as soon as new output is recorded,
// and a resume-style attach never shows it.
func TestClearHistory(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "clear1")

	// Seed on-disk history from "before this daemon" too, so the wipe is proven
	// against the whole log file, not just what this run appended.
	g.E(os.WriteFile(p.LogFile(sess.ID), []byte("SEEDED-SECRET\n"), 0o600))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Cols: 80, Rows: 24})
	defer nc.Close()

	// Produce live output; once it is read back it has been recorded (the daemon
	// writes scrollback before forwarding to the client). Then let the session go
	// quiet, so the wipe below lands after everything it printed is in the log.
	g.E(c.Write(proto.MsgInput, []byte("echo LIVE-SECRET\n")))
	g.True(readUntil(nc, c, "LIVE-SECRET", 10*time.Second))
	settle(nc, c)

	// A separate (non-attaching) connection requests the clear, then blocks until
	// the daemon closes it — the signal that the wipe finished.
	ctl, derr := proto.Dial(d.Addr())
	g.E(derr)

	defer ctl.Close()

	cc := proto.NewConn(ctl)
	g.E(cc.Write(proto.MsgClear, nil))

	_ = ctl.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _, rerr := cc.Read()
	g.Is(rerr, io.EOF) // not a timeout: the daemon closed after clearing

	// The log file on disk holds neither the seeded nor the live secret.
	data, ferr := os.ReadFile(p.LogFile(sess.ID))
	g.E(ferr)
	g.True(!strings.Contains(string(data), "SEEDED-SECRET"))
	g.True(!strings.Contains(string(data), "LIVE-SECRET"))

	// A resume-style attach asked for no replay, so it gets no hint either: its
	// replay is empty.
	ncn, cn := dialAttach(g, d.Addr(), proto.Attach{Cols: 80, Rows: 24})
	defer ncn.Close()

	g.Eq(readReplay(g, ncn, cn), "")

	// A fresh full-history attach replays nothing of the past — only the dim
	// hint explaining the blank history...
	nc2, c2 := dialAttach(g, d.Addr(), proto.Attach{Replay: true, Cols: 80, Rows: 24})
	defer nc2.Close()

	hist := readReplay(g, nc2, c2)
	g.True(!strings.Contains(hist, "SEEDED-SECRET"))
	g.True(!strings.Contains(hist, "LIVE-SECRET"))
	g.Has(hist, "history cleared here")

	// ...and the session survived the wipe: the shell still answers.
	g.E(c2.Write(proto.MsgInput, []byte("echo still-alive\n")))
	g.True(readUntil(nc2, c2, "still-alive", 10*time.Second))
	settle(nc2, c2)

	// With output recorded after the wipe there is real history to replay again,
	// so the hint retires.
	nc3, c3 := dialAttach(g, d.Addr(), proto.Attach{Replay: true, Cols: 80, Rows: 24})
	defer nc3.Close()

	hist = readReplay(g, nc3, c3)
	g.Has(hist, "still-alive")
	g.True(!strings.Contains(hist, "history cleared here"))
}

// replayQuiet is how long readReplay waits for another frame before calling the
// replay finished. The daemon sends the window as one frame right after the
// attach and nothing marks its end, so the end of the replay is simply the point
// where the stream goes quiet — a session sitting at a prompt sends nothing on
// its own.
const (
	replayQuiet = 500 * time.Millisecond
	// replayWait bounds a whole readReplay, so a daemon that never stops sending
	// fails the test instead of hanging it.
	replayWait = 10 * time.Second
)

// readReplay accumulates an attach's replay: the Output frames that arrive
// before the stream goes quiet (see replayQuiet), bounded by replayWait overall.
// Call settle on the previous attachment first, or a session still printing will
// have its live output counted as replay.
func readReplay(g got.G, nc net.Conn, c *proto.Conn) string {
	g.Helper()

	deadline := time.Now().Add(replayWait)

	var hist strings.Builder

	for {
		next := time.Now().Add(replayQuiet)
		if next.After(deadline) {
			next = deadline
		}

		_ = nc.SetReadDeadline(next)

		mt, payload, err := c.Read()
		if err != nil {
			return hist.String() // quiet (or closed): the replay is over
		}

		if mt == proto.MsgOutput {
			hist.Write(payload)
		}
	}
}

// settle drains the attachment until the session stops producing output, so
// everything it printed is recorded before the test looks at the log or attaches
// again. readUntil returns on the first frame carrying its marker, which for a
// typed command is the tty's echo of the input — the shell's own output and the
// next prompt are still on their way, and would otherwise land on (and be
// mistaken for the replay of) whatever attaches next.
func settle(nc net.Conn, c *proto.Conn) {
	for {
		_ = nc.SetReadDeadline(time.Now().Add(replayQuiet))

		if _, _, err := c.Read(); err != nil {
			return
		}
	}
}

// An attach replays the session's last window of output; a resume-style attach
// (Replay off) replays nothing at all, since the screen it left is still up.
func TestAttachReplaysWindow(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "window1")

	// Seed recorded history so the replay is non-empty.
	g.E(os.WriteFile(p.LogFile(sess.ID), []byte("SEEDED-HISTORY\n"), 0o600))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Replay: true, Cols: 80, Rows: 24})
	defer nc.Close()

	g.Has(readReplay(g, nc, c), "SEEDED-HISTORY")

	// The session is live after the replay: input echoes as ordinary output.
	g.E(c.Write(proto.MsgInput, []byte("echo live-after-replay\n")))
	g.True(readUntil(nc, c, "live-after-replay", 10*time.Second))
	settle(nc, c)

	// A resume-style attach gets no replay: its screen never went away, so
	// redrawing the window would print a second copy of it.
	nc2, c2 := dialAttach(g, d.Addr(), proto.Attach{Cols: 80, Rows: 24})
	defer nc2.Close()

	g.Eq(readReplay(g, nc2, c2), "")
}

// A kill must end even a shell that traps SIGHUP and SIGTERM: closing the PTY
// only delivers SIGHUP to the foreground process group, and shutdown deletes
// the session's record — anything surviving the kill would live on as an orphan
// nothing tracks. The daemon therefore SIGKILLs the shell's process group,
// which also sweeps up children that inherited the ignored SIGHUP (here a
// background sleep sharing the shell's group).
func TestKillSessionStubbornShell(t *testing.T) {
	g, st, p := setupDaemon(t)

	// A "shell" that ignores the catchable termination signals, starts a child
	// that inherits those dispositions, and reports both pids.
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	script := filepath.Join(dir, "stubborn.sh")
	g.E(os.WriteFile(script, []byte(
		"#!/bin/sh\ntrap '' HUP TERM\nsleep 120 &\necho \"$$ $!\" > "+pidFile+"\nwait\n"), 0o700))

	sess := makeSession(g, st, "stub1")
	sess.Shell = script
	g.E(st.SaveSession(sess))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	pids := awaitPIDs(g, pidFile)
	g.Len(pids, 2) // the shell and its sleep

	ctl, derr := proto.Dial(d.Addr())
	g.E(derr)

	defer ctl.Close()

	cc := proto.NewConn(ctl)
	g.E(cc.Write(proto.MsgKill, nil))

	_ = ctl.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _, rerr := cc.Read()
	g.Is(rerr, io.EOF) // teardown done

	g.E(d.Wait())

	// Both must be dead — polled, since the daemon reaps the shell asynchronously.
	for _, pid := range pids {
		g.Desc("HUP/TERM-immune process %d must not outlive the kill", pid).
			True(awaitGone(pid, 10*time.Second))
	}
}

// awaitPIDs polls for the pid file the stubborn shell writes at startup and
// parses the space-separated pids in it.
func awaitPIDs(g got.G, path string) []int {
	deadline := time.Now().Add(10 * time.Second)

	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			pids := make([]int, 0, 2) // the file holds "$$ $!"

			for f := range strings.FieldsSeq(string(b)) {
				pid, perr := strconv.Atoi(f)
				g.E(perr)

				pids = append(pids, pid)
			}

			return pids
		}

		if time.Now().After(deadline) {
			g.Fatal("the shell never wrote its pids")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// awaitGone reports whether pid stops existing before the timeout. Signal 0
// probes existence; the pid lingers as a zombie until the daemon reaps it, so
// existence is polled rather than checked once.
func awaitGone(pid int, timeout time.Duration) bool {
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

// A session with a huge log replays only its last window, so the replay stays a
// single frame well under proto.MaxPayload however long the session has run: the
// old output is on disk, not on the wire. An oversized frame would be rejected as
// "payload too large", drop the connection, and bounce the attach back to the
// menu — making a busy session impossible to enter.
func TestAttachHugeLogReplaysOnlyTheWindow(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "bighist")

	// Seed a log far larger than a single frame: many lines of old output, then the
	// window the attach should actually see.
	const (
		oldMarker  = "OLD-BEYOND-THE-WINDOW"
		tailMarker = "TAIL-IN-THE-WINDOW"
	)

	seed := []byte(oldMarker + "\n" + strings.Repeat("filler\n", 2*proto.MaxPayload/7) + tailMarker + "\n")
	g.E(os.WriteFile(p.LogFile(sess.ID), seed, 0o600))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Replay: true, Cols: 80, Rows: 24})
	defer nc.Close()

	hist := readReplay(g, nc, c)
	g.Has(hist, tailMarker)
	g.True(!strings.Contains(hist, oldMarker)) // scrolled out of the window
	g.Lte(len(hist), proto.MaxPayload)
}

func TestDetachThenReattach(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "persist1")

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	// First attach: produce a marker, then detach.
	nc1, c1 := dialAttach(g, d.Addr(), proto.Attach{Cols: 80, Rows: 24})
	g.E(c1.Write(proto.MsgInput, []byte("echo first-attach\n")))
	found := readUntil(nc1, c1, "first-attach", 10*time.Second)
	g.True(found)
	g.E(c1.Write(proto.MsgDetach, nil))
	nc1.Close()

	// Session still alive: reattach with full history and see the earlier marker.
	nc2, c2 := dialAttach(g, d.Addr(), proto.Attach{Replay: true, Cols: 80, Rows: 24})
	defer nc2.Close()

	found = readUntil(nc2, c2, "first-attach", 10*time.Second)
	g.True(found)
}
