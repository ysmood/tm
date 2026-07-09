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

// TestRenameSessionNoticePersists drives the real binary: renaming a session from
// the menu prints a notice, and that notice must survive both the picker's
// redraws (the menu stays open on the main list afterwards) and its teardown when
// tm exits — so it reads like the attach/detach notices, left behind in the
// scrollback rather than erased along with the menu.
func TestRenameSessionNoticePersists(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmrn")
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

	g.True(waitForText(buf, "[rename session]", 10*time.Second))

	// [rename session] -> the session chooser -> pick aaa -> the name prompt,
	// prefilled with "aaa".
	_, err = pt.Write([]byte("rs\r"))
	g.E(err)
	time.Sleep(400 * time.Millisecond)

	_, err = pt.Write([]byte("\r"))
	g.E(err)
	g.True(waitForText(buf, "Rename session:", 10*time.Second))

	_, err = pt.Write([]byte("-zzz\r"))
	g.E(err)
	g.True(waitForText(buf, "renamed session", 10*time.Second))

	// The rename lands in the store...
	renamed, err := st.GetSession("aaa")
	g.E(err)
	g.Eq(renamed.Name, "aaa-zzz")

	// ...and the notice is still on screen while the menu is back on the main list,
	// which now lists the session under its new name.
	v := newVT(40, 120)
	v.feed([]byte(buf.String()))
	g.Desc("the notice must survive the menu's redraws: %q", v.screen()).
		True(strings.Contains(v.screen(), "[tm renamed session aaa → aaa-zzz]"))
	g.Has(v.screen(), "[rename session]") // the menu is back, not torn down

	// esc leaves tm from the top-level menu: the picker is erased, but the notice
	// stays behind in the scrollback along with the exit notice.
	_, err = pt.Write([]byte{0x1b})
	g.E(err)
	g.True(waitForText(buf, "[tm exited]", 10*time.Second))
	g.E(c.Wait())

	v = newVT(40, 120)
	v.feed([]byte(buf.String()))
	out := v.visible()

	g.Desc("the notice must outlive the picker: %q", out).
		True(strings.Contains(out, "[tm renamed session aaa → aaa-zzz]"))
	g.Desc("the picker must be erased: %q", out).
		False(strings.Contains(out, "[rename session]"))
}
