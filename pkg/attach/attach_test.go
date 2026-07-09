//go:build unix

package attach

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/daemon"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
)

// relayExit captures runRelay's return values so a test goroutine can hand
// them back over one channel.
type relayExit struct {
	outcome Outcome
	paused  *Paused
	err     error
}

// safeBuf is a goroutine-safe accumulator for the relay's output.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.String()
}

// echoServer accepts one connection, consumes the Attach, then echoes each
// Input back as Output until the client detaches. It records whether it saw a
// Detach (so the test can confirm the menu key never leaked as input).
func echoServer(g got.G, addr string) (detached func() bool) {
	ln, err := proto.Listen(addr)
	g.E(err)
	g.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex

	sawDetach := false

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		c := proto.NewConn(conn)
		if mt, _, rerr := c.Read(); rerr != nil || mt != proto.MsgAttach {
			return
		}

		// A real daemon always ends the (here empty) replay with the marker; the
		// relay needs it to know the menu key means detach, not pause.
		_ = c.Write(proto.MsgReplayDone, nil)

		for {
			mt, payload, rerr := c.Read()
			if rerr != nil {
				return
			}

			switch mt {
			case proto.MsgInput:
				_ = c.Write(proto.MsgOutput, payload)
			case proto.MsgDetach:
				mu.Lock()
				sawDetach = true
				mu.Unlock()

				return
			}
		}
	}()

	return func() bool {
		mu.Lock()
		defer mu.Unlock()

		return sawDetach
	}
}

func TestRelayForwardsAndDetaches(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(10 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tma")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	addr := filepath.Join(rt, "s.sock")

	detached := echoServer(g, addr)

	inR, inW := io.Pipe()
	out := &safeBuf{}

	done := make(chan relayExit, 1)

	go func() {
		oc, _, p, rerr := runRelay(Options{Hist: proto.HistNone}, inR, out, 0, false,
			func(string) string { return addr }, "x", nil)
		done <- relayExit{outcome: oc, paused: p, err: rerr}
	}()

	// Input is forwarded and echoed back as output.
	_, err = inW.Write([]byte("ping"))
	g.E(err)
	g.True(waitFor(func() bool { return strings.Contains(out.String(), "ping") }, 5*time.Second))

	// The menu key ends the relay and is sent as Detach, not as input.
	_, err = inW.Write([]byte{DefaultMenuKey})
	g.E(err)

	select {
	case res := <-done:
		g.E(res.err)
		g.Eq(res.outcome, OutcomeMenu) // the menu key asks the caller to open the menu
	case <-time.After(5 * time.Second):
		g.Logf("relay did not return after menu key")
		g.FailNow()
	}

	// The relay returning OutcomeMenu only means it wrote the Detach; the echo
	// server reads it off the socket asynchronously, so wait rather than racing it.
	g.True(waitFor(detached, 5*time.Second))
	g.False(bytes.Contains([]byte(out.String()), []byte{DefaultMenuKey}))
}

// exitServer accepts one connection, consumes the Attach, then tells the relay
// the session's shell exited (MsgExit) and closes. It mimics a daemon whose shell
// ended: the relay should report OutcomeSessionExited so app.Run drops back to
// the top-level menu instead of leaving tm.
func exitServer(g got.G, addr string) {
	ln, err := proto.Listen(addr)
	g.E(err)
	g.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		c := proto.NewConn(conn)
		if mt, _, rerr := c.Read(); rerr != nil || mt != proto.MsgAttach {
			return
		}

		_ = c.Write(proto.MsgExit, proto.EncodeExit(0))
	}()
}

// TestRelayReturnsToMenuOnSessionExit proves that when the session's shell exits
// (the daemon sends MsgExit), the relay stops with OutcomeSessionExited so the
// caller returns to the menu rather than treating it like a closed input pipe.
func TestRelayReturnsToMenuOnSessionExit(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(10 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmexit")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	addr := filepath.Join(rt, "s.sock")

	exitServer(g, addr)

	// The write end is never closed: input stays open so the only thing that ends
	// the relay is the session's MsgExit, not a closed pipe.
	inR, _ := io.Pipe()
	out := &safeBuf{}

	done := make(chan relayExit, 1)

	go func() {
		oc, _, p, rerr := runRelay(Options{Hist: proto.HistNone}, inR, out, 0, false,
			func(string) string { return addr }, "x", nil)
		done <- relayExit{outcome: oc, paused: p, err: rerr}
	}()

	select {
	case res := <-done:
		g.E(res.err)
		g.Eq(res.outcome, OutcomeSessionExited)
	case <-time.After(5 * time.Second):
		g.Logf("relay did not return after the session exited")
		g.FailNow()
	}
}

// switchServer accepts one connection, consumes the Attach, then asks the relay
// to switch to target. It mimics a daemon whose in-session tm picked another
// session: the relay should reset the terminal before re-attaching, since the
// session it is leaving may have left it in the alternate screen buffer.
func switchServer(g got.G, addr, target string) {
	ln, err := proto.Listen(addr)
	g.E(err)
	g.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		c := proto.NewConn(conn)
		if mt, _, rerr := c.Read(); rerr != nil || mt != proto.MsgAttach {
			return
		}

		_ = c.Write(proto.MsgReplayDone, nil)
		_ = c.Write(proto.MsgSwitchTo, proto.SwitchTarget{ID: target}.Encode())
	}()
}

// TestRelayRestoresTerminalOnSwitch proves that switching sessions resets the
// outer terminal between them. The leaving session may have left the terminal in
// the alternate screen buffer with the cursor hidden (tm's menu runs there), and
// nothing else resets it on this path — so the relay must, before the target
// session's output lands, or the screen never clears and the cursor stays hidden.
func TestRelayRestoresTerminalOnSwitch(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(10 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmsw")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	addr1 := filepath.Join(rt, "s1.sock")
	addr2 := filepath.Join(rt, "s2.sock")

	switchServer(g, addr1, "s2")
	detached := echoServer(g, addr2)

	inR, inW := io.Pipe()
	out := &safeBuf{}

	done := make(chan relayExit, 1)

	go func() {
		oc, _, p, rerr := runRelay(Options{Hist: proto.HistNone}, inR, out, 0, false,
			func(id string) string {
				if id == "s2" {
					return addr2
				}

				return addr1
			}, "s1", nil)
		done <- relayExit{outcome: oc, paused: p, err: rerr}
	}()

	// Once the relay has re-attached to the target, its output must carry the
	// terminal reset emitted on the way out of the first session.
	g.True(waitFor(func() bool {
		return strings.Contains(out.String(), string(TerminalRestore))
	}, 5*time.Second))

	// The reset must include leaving the alternate screen buffer and showing the
	// cursor — the two symptoms of an un-reset switch.
	o := out.String()
	g.Has(o, "\x1b[?1049l")
	g.Has(o, "\x1b[?25h")

	// The relay is now driving the target session; the menu key ends it cleanly.
	_, err = inW.Write([]byte{DefaultMenuKey})
	g.E(err)

	select {
	case res := <-done:
		g.E(res.err)
	case <-time.After(5 * time.Second):
		g.Logf("relay did not return after menu key")
		g.FailNow()
	}

	g.True(detached())
}

// replayServer mimics a daemon mid-"All history" replay: it accepts one
// connection, consumes the Attach, writes head as history, then waits for a
// signal on more before writing tail, the MsgReplayDone marker, and echoing
// input like a live session. closed() reports whether the relay's side of the
// connection went away (an aborted replay).
func replayServer(g got.G, addr string, head, tail []byte) (more chan<- struct{}, closed func() bool) {
	ln, err := proto.Listen(addr)
	g.E(err)
	g.Cleanup(func() { _ = ln.Close() })

	sig := make(chan struct{})

	var dead atomic.Bool

	go func() {
		defer dead.Store(true)

		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		c := proto.NewConn(conn)
		if mt, _, rerr := c.Read(); rerr != nil || mt != proto.MsgAttach {
			return
		}

		if c.Write(proto.MsgOutput, head) != nil {
			return
		}

		<-sig

		if c.Write(proto.MsgOutput, tail) != nil ||
			c.Write(proto.MsgReplayDone, nil) != nil {
			return
		}

		for {
			mt, payload, rerr := c.Read()
			if rerr != nil || mt == proto.MsgDetach {
				return
			}

			if mt == proto.MsgInput {
				_ = c.Write(proto.MsgOutput, payload)
			}
		}
	}()

	return sig, dead.Load
}

// pauseRelay starts a relay against addr, waits until the replay's head reached
// the output, and pauses it with the menu key mid-replay (the server is holding
// the tail back, so the replay cannot have finished). It returns the suspended
// attachment.
func pauseRelay(g got.G, addr string, out *safeBuf, head []byte) *Paused {
	inR, inW := io.Pipe()

	done := make(chan relayExit, 1)

	go func() {
		oc, _, p, rerr := runRelay(Options{Hist: proto.HistAll}, inR, out, 0, false,
			func(string) string { return addr }, "x", nil)
		done <- relayExit{outcome: oc, paused: p, err: rerr}
	}()

	g.True(waitFor(func() bool { return strings.Contains(out.String(), string(head)) }, 5*time.Second))

	_, err := inW.Write([]byte{DefaultMenuKey})
	g.E(err)

	select {
	case res := <-done:
		g.E(res.err)
		g.Eq(res.outcome, OutcomeMenu)
		g.Desc("the menu key mid-replay must suspend, not detach").NotNil(res.paused)

		return res.paused
	case <-time.After(5 * time.Second):
		g.Logf("relay did not pause after menu key")
		g.FailNow()

		return nil
	}
}

// The menu key during a history replay pauses it — the relay returns for the
// menu immediately, holding the attachment — and Resume continues the replay
// exactly where it stopped, ending with a live session that detaches normally.
func TestRelayPausesReplayAndResumes(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(15 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmpr")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	addr := filepath.Join(rt, "s.sock")
	head, tail := []byte("HEAD-OF-HISTORY\n"), []byte("TAIL-OF-HISTORY\n")

	more, _ := replayServer(g, addr, head, tail)

	out := &safeBuf{}
	paused := pauseRelay(g, addr, out, head)

	// While paused nothing more is read or rendered; release the tail and resume:
	// the rest of the history flows, then the session is live (echo works) and the
	// menu key now detaches instead of pausing.
	close(more)

	inR2, inW2 := io.Pipe()

	done := make(chan relayExit, 1)

	go func() {
		oc, _, p, rerr := runRelay(paused.opt, inR2, out, 0, false, paused.addrOf, paused.id, paused)
		done <- relayExit{outcome: oc, paused: p, err: rerr}
	}()

	g.True(waitFor(func() bool { return strings.Contains(out.String(), string(tail)) }, 5*time.Second))

	_, err = inW2.Write([]byte("ping"))
	g.E(err)
	g.True(waitFor(func() bool { return strings.Contains(out.String(), "ping") }, 5*time.Second))

	_, err = inW2.Write([]byte{DefaultMenuKey})
	g.E(err)

	select {
	case res := <-done:
		g.E(res.err)
		g.Eq(res.outcome, OutcomeMenu)
		g.Nil(res.paused) // live now: the menu key detaches, nothing to resume
	case <-time.After(5 * time.Second):
		g.Logf("relay did not return after menu key")
		g.FailNow()
	}
}

// Aborting a paused replay closes the attachment: the daemon side sees the
// connection drop (so it stops loading history) and nothing more is rendered.
func TestRelayPausedReplayAbort(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(15 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmpa")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	addr := filepath.Join(rt, "s.sock")
	head, tail := []byte("HEAD-OF-HISTORY\n"), []byte("TAIL-NEVER-SHOWN\n")

	more, closed := replayServer(g, addr, head, tail)

	out := &safeBuf{}
	paused := pauseRelay(g, addr, out, head)

	paused.Abort()

	// Even once the server tries to send the rest, its writes fail against the
	// closed connection and the tail never reaches the screen.
	close(more)
	g.True(waitFor(closed, 5*time.Second))
	g.False(strings.Contains(out.String(), string(tail)))
}

// throttledBuf slows each write down, standing in for a real terminal —
// rendering is the slow side of the relay — so a multi-megabyte replay cannot
// finish before the test pauses it.
type throttledBuf struct{ safeBuf }

func (t *throttledBuf) Write(p []byte) (int, error) {
	time.Sleep(time.Millisecond)

	return t.safeBuf.Write(p)
}

// The whole flow against a real daemon: a seeded multi-megabyte "All history"
// replay is paused mid-stream — the daemon stalls on backpressure rather than
// pushing the rest — then resumed, delivering the remaining history and a live
// session. This is the user-visible Ctrl-\-during-replay path end to end.
func TestRelayPauseResumeRealDaemon(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(30 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmprd")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	p := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(p.EnsureDirs())
	st := store.New(p)

	sess := store.Session{
		ID: "big1", Name: "big1", Namespace: store.DefaultNamespace,
		Shell: "/bin/sh", PID: 1, CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))

	// Seed a history far larger than the socket buffers, ending in a marker, so
	// the daemon is still blocked mid-replay when the pause lands and the tail
	// can only appear if the resume really continues the replay.
	const tailMark = "END-OF-BIG-HISTORY"

	seed := append(bytes.Repeat([]byte("x"), 2<<20), []byte(tailMark+"\n")...)
	g.E(os.WriteFile(p.LogFile(sess.ID), seed, 0o600))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	addrOf := func(string) string { return d.Addr() }
	out := &throttledBuf{}
	paused := pauseRelayVia(g, addrOf, out)

	g.False(strings.Contains(out.String(), tailMark)) // the tail is still unloaded

	// Resume: the rest of the history flows, then the session is live.
	inR, inW := io.Pipe()

	done := make(chan relayExit, 1)

	go func() {
		oc, _, pz, rerr := runRelay(paused.opt, inR, out, 0, false, paused.addrOf, paused.id, paused)
		done <- relayExit{outcome: oc, paused: pz, err: rerr}
	}()

	g.True(waitFor(func() bool { return strings.Contains(out.String(), tailMark) }, 10*time.Second))

	_, err = inW.Write([]byte("echo relay-live\n"))
	g.E(err)
	g.True(waitFor(func() bool { return strings.Contains(out.String(), "relay-live") }, 10*time.Second))

	_, err = inW.Write([]byte{DefaultMenuKey})
	g.E(err)

	res := <-done
	g.E(res.err)
	g.Eq(res.outcome, OutcomeMenu)
	g.Nil(res.paused) // live now: the menu key detaches, nothing to resume
}

// pauseRelayVia is pauseRelay for an addrOf resolver: it starts a relay, waits
// for the first replayed bytes, and pauses it with the menu key mid-replay.
func pauseRelayVia(g got.G, addrOf func(string) string, out interface {
	io.Writer
	String() string
},
) *Paused {
	inR, inW := io.Pipe()

	done := make(chan relayExit, 1)

	go func() {
		oc, _, pz, rerr := runRelay(Options{Hist: proto.HistAll}, inR, out, 0, false, addrOf, "big1", nil)
		done <- relayExit{outcome: oc, paused: pz, err: rerr}
	}()

	g.True(waitFor(func() bool { return out.String() != "" }, 5*time.Second))

	_, err := inW.Write([]byte{DefaultMenuKey})
	g.E(err)

	res := <-done
	g.E(res.err)
	g.Eq(res.outcome, OutcomeMenu)
	g.Desc("the menu key mid-replay must suspend, not detach").NotNil(res.paused)

	return res.paused
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}

		time.Sleep(10 * time.Millisecond)
	}

	return false
}
