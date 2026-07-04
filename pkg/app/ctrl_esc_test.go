//go:build unix

package app_test

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

// TestCtrlBackslashEscPreservesPrompt drives the real binary through the relay
// menu (runMenu): attach to a session, set a distinctive shell prompt, open the
// menu with Ctrl-\ and immediately cancel it with esc. Resuming the session must
// leave the shell's prompt line untouched (like fzf on esc), not swallow it, and
// must put the cursor back where the prompt left it. The fix saves and restores
// the cursor with DECSC/DECRC and draws the picker below the prompt.
func TestCtrlBackslashEscPreservesPrompt(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(90 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmctrlesc")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)

	p, err := config.New()
	g.E(err)
	g.E(p.EnsureDirs())
	st := store.New(p)

	sess := store.Session{
		ID: "aaa", Name: "aaa", Namespace: store.DefaultNamespace,
		Shell: "/bin/sh", CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))
	g.E(app.SpawnWith(bin, p, sess))

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))
	g.Cleanup(func() { _ = pt.Close() })

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

	step := func(label, want string) {
		if !waitForText(buf, want, 8*time.Second) {
			t.Fatalf("step %s: never saw %q; buf: %q", label, want, buf.String())
		}
	}

	// Pick session aaa from the relay menu, with all history -> attach.
	step("menu-shown", "new session")
	send("aaa\r")
	step("scrollback-shown", "All history")
	send("\r")
	time.Sleep(1000 * time.Millisecond)

	// Give the session shell a distinctive prompt, then settle on it. The prompt
	// text is assembled from pieces so the echoed command line never contains the
	// marker contiguously — only the live prompt does, so the assertion can't match
	// the command echo by accident. "ZZMARK> " is 8 cells, so the cursor sits at
	// column index 8.
	const promptEndCol = 8

	send(`A=ZZ; B=MARK; PS1="$A$B> "` + "\r")
	step("prompt-set", "ZZMARK>")

	// Open the menu (Ctrl-\) and immediately cancel it (esc) to resume aaa.
	send(string([]byte{0x1c}))
	step("menu-reopened", "[new session]")
	send(string([]byte{0x1b}))
	time.Sleep(1200 * time.Millisecond)

	v := newVT(40, 120)
	v.feed([]byte(buf.String()))
	screen := v.visible()

	// After esc the resumed shell is sitting at its prompt; that prompt must be the
	// last visible line, exactly as it was before Ctrl-\ (like fzf on esc). If the
	// menu teardown swallowed it, the last line is the menu/command echo instead.
	last := lastNonEmptyLine(screen)
	g.Desc("the shell prompt must survive Ctrl-\\ then esc, last line: %q\nfull screen: %q", last, screen).
		True(strings.Contains(last, "ZZMARK>"))

	// The cursor must be back on the prompt line, just past it, so the resumed shell
	// keeps typing in the right place.
	g.Desc("cursor row %d must be on the prompt line: %q", v.cr, v.line(v.cur[v.cr])).
		True(strings.Contains(v.line(v.cur[v.cr]), "ZZMARK>"))
	g.Desc("cursor must be restored just past the prompt, got column %d", v.cc).Eq(v.cc, promptEndCol)

	// Exit tm cleanly via the menu (Ctrl-\ -> [detach session] -> top-level menu ->
	// esc), then let the process go.
	detachViaMenu(g, pt, buf)

	waitExit(c, 8*time.Second)
}

// waitExit waits up to d for c to exit, killing it if it overstays, so a test's
// teardown can't hang on a process that didn't notice it was asked to leave.
func waitExit(c *gopty.Cmd, d time.Duration) {
	done := make(chan struct{})

	go func() { _ = c.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(d):
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	}
}

func lastNonEmptyLine(screen string) string {
	lines := strings.Split(screen, "\n")
	for _, v := range slices.Backward(lines) {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}
