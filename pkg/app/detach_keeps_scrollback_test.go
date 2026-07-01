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

// TestDetachFromShellPreservesScrollback drives the real binary, attaches to a
// session that never leaves the main screen, then detaches via the menu. Because
// nothing entered the alternate screen, detaching must not emit the alt-screen
// leave (\e[?1049l) — a bare rmcup wipes scrollback on many terminals — nor erase
// the screen/scrollback, while still resetting the harmless input/display modes.
//
// Regression: detaching (and exiting) used to write the full reset unconditionally,
// clearing the session's history from the outer terminal even from a plain shell.
func TestDetachFromShellPreservesScrollback(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(60 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmdks")
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
	detachViaMenu(g, pt, buf)
	g.E(c.Wait())

	out := buf.String()[mark:]

	// The session stayed on the main screen, so detaching must not leave the
	// alternate screen or erase anything.
	g.Desc("detach must not emit rmcup (it wipes scrollback): %q", out).
		False(strings.Contains(out, "\x1b[?1049l"))
	g.Desc("detach must not erase the scrollback: %q", out).
		False(strings.Contains(out, "\x1b[3J"))
	g.Desc("detach must not erase the whole screen: %q", out).
		False(strings.Contains(out, "\x1b[2J"))

	// The harmless mode reset is still sent (scroll region reset, cursor shown).
	g.Desc("detach should still reset terminal modes: %q", out).
		True(strings.Contains(out, "\x1b[r"))
	g.Desc("detach should show the cursor: %q", out).
		True(strings.Contains(out, "\x1b[?25h"))
}
