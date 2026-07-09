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

// TestExitReopensMenuInline proves that when a session's shell exits and tm
// reopens the top-level menu, the menu is drawn inline below the just-exited
// session's output — not from the top of the screen. The terminal reset that runs
// on exit resets the scroll region (DECSTBM), which homes the cursor; if that home
// is not undone, the reopened inline menu renders from the top and erases the
// screen below it, wiping the session output the user just had. The reset wraps the
// scroll-region reset in save/restore-cursor so this cannot happen.
func TestExitReopensMenuInline(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(60 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmemr")
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
		time.Sleep(300 * time.Millisecond)
	}

	g.True(waitForText(buf, "new session", 10*time.Second))
	send("ns\r")
	g.True(waitForText(buf, "New session name", 10*time.Second))
	send("\r")
	time.Sleep(1500 * time.Millisecond)

	send("echo INSESSION-42\r")
	g.True(waitForText(buf, "INSESSION-42", 10*time.Second))

	mark := len(buf.String())

	send("exit\r")
	g.True(waitForTextFrom(buf, mark, "new session", 10*time.Second))
	time.Sleep(500 * time.Millisecond)

	// Render the whole stream and look at the CURRENT screen (no scrollback): the
	// session's output must still be there, with the reopened menu below it. If the
	// menu had rendered from a homed cursor it would have erased the screen and this
	// marker would be gone.
	v := newVT(40, 120)
	v.feed([]byte(buf.String()))
	screen := v.screen()

	g.Desc("session output erased when the menu reopened:\n%s", screen).
		True(strings.Contains(screen, "INSESSION-42"))

	menuLine := strings.Index(screen, "[new session]")
	sessLine := strings.Index(screen, "INSESSION-42")
	g.Desc("menu did not reopen below the session output:\n%s", screen).
		True(menuLine > sessLine)

	send("\x1b") // leave tm
	waitExit(c, 8*time.Second)
}
