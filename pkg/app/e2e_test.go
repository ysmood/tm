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

	defer func() { _ = pt.Close() }()

	c := pt.Command(bin, "__attach", "--id", "rly", "--hist", "1") // hist=1 -> HistAll

	c.Env = append(os.Environ(), "CI=1") // skip termenv's init query (see relayEnv)
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
// a session, attach, run a command in its shell, then detach back to the menu
// and quit. The assertion matches executed output ("ok-42" from "$((6*7))"),
// not the echoed input, so it proves the shell actually ran.
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

	_, err = pt.Write([]byte{0x1c}) // Ctrl-\ detaches back to the menu
	g.E(err)
	time.Sleep(1500 * time.Millisecond)

	_, err = pt.Write([]byte{0x03}) // Ctrl-C quits the menu
	g.E(err)
	g.E(c.Wait())
}
