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
	"github.com/ysmood/tm/pkg/proto"
)

// relayExit captures runRelay's two return values so a test goroutine can hand
// both back over one channel.
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
		oc, rerr := runRelay(Options{Hist: proto.HistNone}, inR, out, 0, false,
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

	g.True(detached())
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
		oc, rerr := runRelay(Options{Hist: proto.HistNone}, inR, out, 0, false,
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
		oc, rerr := runRelay(Options{Hist: proto.HistNone}, inR, out, 0, false,
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
