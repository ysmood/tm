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

// TestImmediateQuitPreservesScrollback drives the real binary from a shell prompt
// (not inside a session) and quits the menu straight away with esc, without ever
// attaching. Because the menu renders inline and no relay runs, tm must leave the
// outer terminal exactly as it found it: it must not touch the alternate screen
// buffer or erase the screen/scrollback, so the shell's history survives.
//
// Regression: tm used to write a blanket terminal reset on every exit path,
// including this one. That reset began with "leave alternate screen" (\e[?1049l)
// even though nothing had entered it, which drops the scrollback on terminals
// (xterm.js / the VS Code terminal among them) that pair rmcup with a buffer wipe.
func TestImmediateQuitPreservesScrollback(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(60 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmquit")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))
	g.Cleanup(func() { _ = pt.Close() })

	c := pt.Command("/bin/sh", "-c", "echo OUTER-SCROLLBACK-MARK; exec "+bin)
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	g.True(waitForText(buf, "new session", 10*time.Second))
	mark := len(buf.String())

	_, err = pt.Write([]byte("\x1b")) // esc -> quit immediately, no session attached
	g.E(err)
	g.E(c.Wait())

	out := buf.String()[mark:]

	// Never having shown a session, tm must not enter or leave the alternate
	// screen, and must not erase the whole screen or the scrollback.
	g.Desc("must not enter the alternate screen: %q", out).
		False(strings.Contains(out, "\x1b[?1049h"))
	g.Desc("must not leave the alternate screen (a bare rmcup wipes scrollback): %q", out).
		False(strings.Contains(out, "\x1b[?1049l"))
	g.Desc("must not erase the scrollback: %q", out).
		False(strings.Contains(out, "\x1b[3J"))
	g.Desc("must not erase the whole screen: %q", out).
		False(strings.Contains(out, "\x1b[2J"))

	// The shell prompt printed before tm started must still be on screen: the
	// inline picker sat below it and is erased on quit, leaving the prompt intact.
	v := newVT(40, 120)
	v.feed([]byte(buf.String()))
	g.Desc("the pre-menu scrollback must survive the quit: %q", v.visible()).
		True(strings.Contains(v.visible(), "OUTER-SCROLLBACK-MARK"))
}
