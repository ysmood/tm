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

// TestFirstLaunchAttachErasesPicker drives the real binary from a shell prompt
// (not inside a session): selecting a session attaches the relay, and the picker
// must be erased on the way — like fzf — leaving the shell prompt above and the
// session's history below, with no menu rows stranded between them.
func TestFirstLaunchAttachErasesPicker(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmfl")
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

	// Give aaa a distinctive history line, then leave it.
	sp, err := gopty.New()
	g.E(err)
	g.E(sp.Resize(120, 40))

	scmd := sp.Command(bin, "__attach", "--id", "aaa")
	scmd.Env = os.Environ()
	g.E(scmd.Start())

	sbuf := &safeBuilder{}
	go func() { _, _ = io.Copy(sbuf, sp) }()

	time.Sleep(600 * time.Millisecond)

	_, err = sp.Write([]byte("echo AAA-HIST-MARK\r"))
	g.E(err)
	g.True(waitForText(sbuf, "AAA-HIST-MARK", 10*time.Second))

	_, err = sp.Write([]byte{0x1c})
	g.E(err)
	g.E(scmd.Wait())
	_ = sp.Close()

	// Launch the menu from a shell prompt and attach to aaa with all history.
	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))
	g.Cleanup(func() { _ = pt.Close() })

	c := pt.Command("/bin/sh", "-c", "echo OUTER-SHELL-PROMPT; exec "+bin)
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	g.True(waitForText(buf, "aaa", 10*time.Second))

	_, err = pt.Write([]byte("aaa\r"))
	g.E(err)
	g.True(waitForText(buf, "All history", 10*time.Second))

	_, err = pt.Write([]byte("\r"))
	g.E(err)
	g.True(waitForText(buf, "AAA-HIST-MARK", 10*time.Second))

	time.Sleep(500 * time.Millisecond)

	v := newVT(40, 120)
	v.feed([]byte(buf.String()))
	screen := v.visible()

	g.Desc("the shell prompt above the menu must remain: %q", screen).
		True(strings.Contains(screen, "OUTER-SHELL-PROMPT"))
	g.Desc("the session history must show after attach: %q", screen).
		True(strings.Contains(screen, "AAA-HIST-MARK"))
	g.Desc("the picker must be erased — no command rows left behind: %q", screen).
		False(strings.Contains(screen, "[new session]"))
	g.Desc("the picker must be erased — no header left behind: %q", screen).
		False(strings.Contains(screen, "namespace: default"))

	detachViaMenu(g, pt, buf) // Ctrl-\ menu -> [detach session] -> leave tm
	_ = c.Wait()
}
