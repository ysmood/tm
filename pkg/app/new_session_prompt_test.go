//go:build unix

package app_test

import (
	"io"
	"os"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
)

// TestNewSessionShowsInitialPrompt creates a new session through the real binary
// and asserts the shell's first prompt is visible immediately, without typing
// anything. The daemon reports ready as soon as the shell starts, which can be
// after the shell has already written its prompt into scrollback (common on
// Linux, rare on macOS); a new session must therefore replay history (HistAll) so
// that captured prompt is shown, instead of opening to a blank screen until the
// user presses Enter.
func TestNewSessionShowsInitialPrompt(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(60 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmpr")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)
	g.Setenv("SHELL", "/bin/sh")
	g.Setenv("PS1", "RDYPROMPT$ ")
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
	send("ns\r") // filter to [new session] and select it
	g.True(waitForText(buf, "New session name", 10*time.Second))

	mark := len(buf.String())
	send("\r") // accept the default name -> spawn + attach

	// The prompt must appear on its own, with no command typed.
	g.Desc("prompt after creating a new session: %q", buf.String()[mark:]).
		True(waitForTextFrom(buf, mark, "RDYPROMPT", 10*time.Second))

	detachViaMenu(g, pt, buf)
	waitExit(c, 10*time.Second)
}
