//go:build unix

package app_test

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
)

// TestExitFromShellPreservesScrollback drives the real binary, attaches to a
// session on the main screen, exits its shell, then leaves tm. Exiting a session
// (the relay's own teardown, a different code path from a menu detach) must not
// emit the alt-screen leave (\e[?1049l) or erase anything when the session never
// entered the alternate screen, so the session's output stays in the scrollback.
func TestExitFromShellPreservesScrollback(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(60 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmeks")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("XDG_RUNTIME_DIR", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))

	defer func() { _ = pt.Close() }()

	c := pt.Command(bin)
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}

	go func() { _, _ = io.Copy(buf, pt) }()

	send := func(s string) {
		_, werr := pt.Write([]byte(s))
		g.E(werr)
		time.Sleep(300 * time.Millisecond)
	}

	g.True(waitForText(buf, "new session", 10*time.Second))
	send("ns\r")
	g.True(waitForText(buf, "New session name", 10*time.Second))
	send("\r") // accept the default name -> spawn + attach
	time.Sleep(1500 * time.Millisecond)

	send("echo IN-SHELL-$((6*7))\r")
	g.Desc("session output: %q", buf.String()).True(waitForText(buf, "IN-SHELL-42", 15*time.Second))

	mark := len(buf.String())

	// Exit the shell: the session ends and tm returns to the top-level menu.
	send("exit\r")
	g.True(waitForTextFrom(buf, mark, "new session", 10*time.Second))

	out := buf.String()[mark:]

	g.Desc("exit must not emit rmcup (it wipes scrollback): %q", out).
		False(strings.Contains(out, "\x1b[?1049l"))
	g.Desc("exit must not erase the scrollback: %q", out).
		False(strings.Contains(out, "\x1b[3J"))
	g.Desc("exit must not erase the whole screen: %q", out).
		False(strings.Contains(out, "\x1b[2J"))

	// esc at the top-level menu leaves tm.
	send("\x1b")
	waitExit(c, 8*time.Second)
}
