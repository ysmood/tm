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

// TestDetachRestoresTerminal proves that detaching from a session in which a
// full-screen program left the terminal in a non-default state (alternate screen
// buffer, mouse reporting, a scroll region) restores the outer terminal, so the
// user gets their scrollback back. Without the restore the terminal is left stuck
// in the alternate screen buffer (the wheel then finds no history) — this drives
// the real menu and asserts the captured stream ends with the terminal back on
// the main screen and those modes off.
func TestDetachRestoresTerminal(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmrest")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)
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
		time.Sleep(250 * time.Millisecond)
	}

	g.True(waitForText(buf, "new session", 10*time.Second))
	send("ns\r")
	g.True(waitForText(buf, "New session name", 10*time.Second))
	send("\r")
	time.Sleep(1500 * time.Millisecond)

	// Emulate a full-screen app inside the session: enter the alternate screen
	// buffer, turn on SGR mouse reporting, and set a scroll region — exactly the
	// state vim/less/htop leave the terminal in while running.
	send("printf '\\033[?1049h\\033[?1000h\\033[?1006h\\033[5;20r'\r")
	send("echo MARK-$((6*7))\r")
	g.Desc("session output: %q", buf.String()).True(waitForText(buf, "MARK-42", 15*time.Second))

	// Ctrl-\ opens the menu (resetting the terminal as it tears the relay down);
	// [detach session] then leaves tm. The reset we assert on is emitted by that
	// teardown, exactly as a direct detach used to emit it.
	detachViaMenu(g, pt, buf)
	g.E(c.Wait())

	out := buf.String()
	count := func(s string) int { return strings.Count(out, s) }

	// The terminal must end on the main screen, not the alternate buffer: every
	// alt-screen enter is balanced by (or outnumbered by) a leave.
	g.Desc("alt-screen left enabled: %d enters vs %d leaves\nstream: %q",
		count("\x1b[?1049h"), count("\x1b[?1049l"), out).
		True(count("\x1b[?1049l") >= count("\x1b[?1049h"))

	// Mouse reporting and the scroll region the session set must be turned off.
	g.Desc("SGR mouse reporting not disabled: %q", out).True(strings.Contains(out, "\x1b[?1006l"))
	g.Desc("normal mouse reporting not disabled: %q", out).True(strings.Contains(out, "\x1b[?1000l"))
	g.Desc("scroll region not reset: %q", out).True(strings.Contains(out, "\x1b[r"))
}
