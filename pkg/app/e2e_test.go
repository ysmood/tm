//go:build unix

package app_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
)

type safeBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *safeBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

func buildTM(g got.G, t *testing.T) string {
	bin := filepath.Join(t.TempDir(), "tm")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/ysmood/tm").CombinedOutput()
	g.Desc("%s", string(out)).E(err)

	return bin
}

// TestMenuRendersUnderPTY runs the real binary inside a pseudo-terminal so
// Bubble Tea starts in full TTY mode, confirms the command palette renders, then
// quits with Ctrl-C.
func TestMenuRendersUnderPTY(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(90 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tme")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("XDG_RUNTIME_DIR", rt)

	bin := buildTM(g, t)

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40)) // the v2 cell renderer needs a non-zero screen size

	defer func() { _ = pt.Close() }()

	c := pt.Command(bin)
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	g.Desc("menu output: %q", buf.String()).True(
		waitForText(buf, "new session", 10*time.Second))

	_, _ = pt.Write([]byte{0x03}) // Ctrl-C quits the menu

	g.E(c.Wait())
}

// TestRelaySwitchesSessions proves the real relay switches sessions in place
// instead of nesting: a relay attached to session aaa is told (via aaa's daemon,
// exactly as an in-session tm would) to hand over to bbb, and afterwards the same
// terminal is driving bbb. Each session reports its identity via $TM_SESSION, so
// the WHO= markers come from the session's own shell, not the echoed input.
func TestRelaySwitchesSessions(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmsw")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("XDG_RUNTIME_DIR", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)

	p, err := config.New()
	g.E(err)
	g.E(p.EnsureDirs())
	st := store.New(p)

	for _, id := range []string{"aaa", "bbb"} {
		sess := store.Session{
			ID: id, Name: id, Namespace: store.DefaultNamespace,
			Shell: "/bin/sh", CreatedAt: time.Unix(1, 0),
		}
		g.E(st.SaveSession(sess))
		g.E(app.SpawnWith(bin, p, sess))
	}

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))

	defer func() { _ = pt.Close() }()

	c := pt.Command(bin, "__attach", "--id", "aaa")
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	time.Sleep(800 * time.Millisecond)
	_, err = pt.Write([]byte("echo WHO=$TM_SESSION\r"))
	g.E(err)
	g.Desc("relay should start on aaa: %q", buf.String()).True(waitForText(buf, "WHO=aaa", 10*time.Second))

	// Ask aaa's daemon to hand the relay to bbb — what controller.Switch does.
	nc, derr := proto.Dial(proto.SockAddr(p, "aaa"))
	g.E(derr)
	conn := proto.NewConn(nc)
	g.E(conn.Write(proto.MsgSwitch, proto.SwitchTarget{ID: "bbb"}.Encode()))
	_, _, _ = conn.Read() // block until the daemon forwards and closes
	_ = nc.Close()

	time.Sleep(1 * time.Second) // let the relay re-attach to bbb
	_, err = pt.Write([]byte("echo WHO=$TM_SESSION\r"))
	g.E(err)
	g.Desc("relay should have switched to bbb: %q", buf.String()).True(waitForText(buf, "WHO=bbb", 10*time.Second))

	_, _ = pt.Write([]byte{0x1c}) // detach -> the relay exits
	_ = c.Wait()
}

func waitForText(buf *safeBuilder, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return true
		}

		time.Sleep(50 * time.Millisecond)
	}

	return strings.Contains(buf.String(), want)
}

// killLeftoverDaemons registers a cleanup that kills any session daemons still
// running, so a detached (still-alive) session doesn't leak past the test.
func killLeftoverDaemons(g got.G) {
	g.Cleanup(func() {
		p, err := config.New()
		if err != nil {
			return
		}

		sessions, _ := store.New(p).ListSessions()
		for _, s := range sessions {
			if s.PID > 0 {
				_ = syscall.Kill(s.PID, syscall.SIGKILL)
			}
		}
	})
}

// TestRelayUnderPTY runs the relay directly under a PTY (no menu) to confirm the
// relay forwards input and output correctly, isolating it from the Bubble Tea
// ExecProcess integration.
func TestRelayUnderPTY(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(90 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmr")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("XDG_RUNTIME_DIR", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)
	p, err := config.New()
	g.E(err)
	g.E(p.EnsureDirs())
	st := store.New(p)
	sess := store.Session{
		ID: "rly", Name: "rly", Namespace: store.DefaultNamespace,
		Shell: "/bin/sh", CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))
	g.E(app.SpawnWith(bin, p, sess))

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40)) // the v2 cell renderer needs a non-zero screen size

	defer func() { _ = pt.Close() }()

	c := pt.Command(bin, "__attach", "--id", "rly", "--hist", "1") // hist=1 -> HistAll

	c.Env = os.Environ() // no CI=1 workaround needed on Bubble Tea v2
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	time.Sleep(500 * time.Millisecond)

	_, err = pt.Write([]byte("echo ok-$((6*7))\r"))
	g.E(err)
	g.Desc("relay output: %q", buf.String()).True(waitForText(buf, "ok-42", 15*time.Second))

	_, err = pt.Write([]byte{0x1c})
	g.E(err)
	g.E(c.Wait())
}

// TestMenuCreateAttachDetach drives the real menu through the whole flow: create
// a session, attach, run a command in its shell, then detach — which leaves tm
// for the launching shell while the session keeps running. The assertion matches
// executed output ("ok-42" from "$((6*7))"), not the echoed input, so it proves
// the shell actually ran.
func TestMenuCreateAttachDetach(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmf")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("XDG_RUNTIME_DIR", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40)) // the v2 cell renderer needs a non-zero screen size

	defer func() { _ = pt.Close() }()

	c := pt.Command(bin)
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	send := func(s string) {
		_, werr := pt.Write([]byte(s))
		g.E(werr)
		time.Sleep(200 * time.Millisecond)
	}

	g.True(waitForText(buf, "new session", 10*time.Second))

	send("ns\r") // filter to [new session] and select it
	g.True(waitForText(buf, "New session name", 10*time.Second))

	send("\r")                          // accept the default name -> spawn + attach
	time.Sleep(1500 * time.Millisecond) // let the shell come up

	send("echo ok-$((6*7))\r")
	g.Desc("outer buffer: %q", buf.String()).True(waitForText(buf, "ok-42", 15*time.Second))

	_, err = pt.Write([]byte{0x1c}) // Ctrl-\ detaches; tm exits to the launching shell
	g.E(err)

	g.E(c.Wait()) // detaching leaves tm (the session keeps running in the background)
}
