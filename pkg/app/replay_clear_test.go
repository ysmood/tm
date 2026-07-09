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
	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

// TestReattachReplaysHistoryPastClear drives the real binary: a session records
// content, then a `clear`, then more content; re-attaching with "all history"
// must still replay the pre-clear content (and must not replay the screen/
// scrollback clears that would wipe it). `clear` emits ED3+ED2, so before the
// replay sanitizer dropped them a single `clear` made "all history" come back
// blank — the screen cleared instead of showing the history.
func TestReattachReplaysHistoryPastClear(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmvfix")
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
		ID: "clr", Name: "clr", Namespace: store.DefaultNamespace,
		Shell: "/bin/sh", CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))
	g.E(app.SpawnWith(bin, p, sess))

	// First attach: produce content, clear, then more content.
	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))

	c := pt.Command(bin, "__attach", "--id", "clr")
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	time.Sleep(700 * time.Millisecond)

	_, err = pt.Write([]byte("echo BEFORE-CLEAR-MARK\r"))
	g.E(err)
	g.True(waitForText(buf, "BEFORE-CLEAR-MARK", 10*time.Second))

	_, err = pt.Write([]byte("clear\r"))
	g.E(err)
	time.Sleep(400 * time.Millisecond)

	_, err = pt.Write([]byte("echo AFTER-CLEAR-MARK\r"))
	g.E(err)
	g.True(waitForText(buf, "AFTER-CLEAR-MARK", 10*time.Second))

	_, err = pt.Write([]byte{0x1c}) // detach; session keeps running
	g.E(err)
	g.E(c.Wait())

	_ = pt.Close()

	// Re-attach with all history (hist=1 => HistAll).
	pt2, err := gopty.New()
	g.E(err)
	g.E(pt2.Resize(120, 40))
	g.Cleanup(func() { _ = pt2.Close() })

	c2 := pt2.Command(bin, "__attach", "--id", "clr", "--hist", "1")
	c2.Env = os.Environ()
	g.E(c2.Start())

	buf2 := &safeBuilder{}
	go func() { _, _ = io.Copy(buf2, pt2) }()

	g.True(waitForText(buf2, "AFTER-CLEAR-MARK", 10*time.Second))
	time.Sleep(500 * time.Millisecond)

	replay := buf2.String()

	// The pre-clear content the user wants to scroll back to is present...
	g.Desc("replay must contain pre-clear history: %q", replay).
		True(strings.Contains(replay, "BEFORE-CLEAR-MARK"))
	// ...and the screen/scrollback clears that would have wiped it are gone.
	g.Desc("replay must not re-run ED3 (clear scrollback): %q", replay).
		False(strings.Contains(replay, "\x1b[3J"))
	g.Desc("replay must not re-run ED2 (clear screen): %q", replay).
		False(strings.Contains(replay, "\x1b[2J"))

	_, err = pt2.Write([]byte{0x1c})
	g.E(err)

	_ = c2.Wait()
}
