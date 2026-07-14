//go:build unix

package attach

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		oc, _, rerr := runRelay(Options{}, inR, out, 0, false,
			func(string) string { return addr }, "x")
		done <- relayExit{outcome: oc, err: rerr}
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

// Ctrl-C is ordinary input: it is forwarded to the session (the shell's SIGINT)
// rather than meaning anything to the relay. Only the menu key ends a relay.
func TestRelayForwardsCtrlC(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(10 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmcc")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	addr := filepath.Join(rt, "s.sock")

	detached := echoServer(g, addr)

	inR, inW := io.Pipe()
	out := &safeBuf{}

	done := make(chan relayExit, 1)

	go func() {
		oc, _, rerr := runRelay(Options{}, inR, out, 0, false,
			func(string) string { return addr }, "x")
		done <- relayExit{outcome: oc, err: rerr}
	}()

	const ctrlC = 0x03

	_, err = inW.Write([]byte{ctrlC})
	g.E(err)
	g.True(waitFor(func() bool {
		return strings.Contains(out.String(), string([]byte{ctrlC}))
	}, 5*time.Second))

	// The relay is still up; the menu key ends it as usual.
	_, err = inW.Write([]byte{DefaultMenuKey})
	g.E(err)

	select {
	case res := <-done:
		g.E(res.err)
		g.Eq(res.outcome, OutcomeMenu)
	case <-time.After(5 * time.Second):
		g.Logf("relay did not return after menu key")
		g.FailNow()
	}

	g.True(waitFor(detached, 5*time.Second))
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
		oc, _, rerr := runRelay(Options{}, inR, out, 0, false,
			func(string) string { return addr }, "x")
		done <- relayExit{outcome: oc, err: rerr}
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
		oc, _, rerr := runRelay(Options{}, inR, out, 0, false,
			func(id string) string {
				if id == "s2" {
					return addr2
				}

				return addr1
			}, "s1")
		done <- relayExit{outcome: oc, err: rerr}
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

// The whole flow against a real daemon: attaching replays the session's last
// window of recorded output — however long the log is, only its tail is drawn —
// and the session is live straight afterwards. This is the user-visible
// enter-a-session path end to end.
func TestRelayReplaysWindowRealDaemon(t *testing.T) {
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

	// Seed a log far larger than one window: only the lines at its end belong to
	// the window the attach should draw.
	const (
		oldMark  = "START-OF-BIG-HISTORY"
		tailMark = "END-OF-BIG-HISTORY"
	)

	seed := []byte(oldMark + "\n" + strings.Repeat("filler\n", 100_000) + tailMark + "\n")
	g.E(os.WriteFile(p.LogFile(sess.ID), seed, 0o600))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	inR, inW := io.Pipe()
	out := &safeBuf{}

	done := make(chan relayExit, 1)

	go func() {
		oc, _, rerr := runRelay(Options{Replay: true}, inR, out, 0, false,
			func(string) string { return d.Addr() }, sess.ID)
		done <- relayExit{outcome: oc, err: rerr}
	}()

	// The window's tail is drawn...
	g.True(waitFor(func() bool { return strings.Contains(out.String(), tailMark) }, 10*time.Second))
	// ...and the history far above it is not: it stays in the log file.
	g.False(strings.Contains(out.String(), oldMark))

	// The session is live right after the replay.
	_, err = inW.Write([]byte("echo relay-live\n"))
	g.E(err)
	g.True(waitFor(func() bool { return strings.Contains(out.String(), "relay-live") }, 10*time.Second))

	_, err = inW.Write([]byte{DefaultMenuKey})
	g.E(err)

	res := <-done
	g.E(res.err)
	g.Eq(res.outcome, OutcomeMenu)
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
