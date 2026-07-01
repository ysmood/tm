//go:build unix

package app_test

import (
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

// TestMenuReapsUnreachableSession reproduces the "can't re-enter a session, it
// bounces forever" bug and proves the fix. A session whose daemon died without a
// clean shutdown (SIGKILL here; equivalently a reboot that clears the socket dir)
// leaves a record with a stale PID. Attaching to it makes the relay fail to dial
// and exit, bouncing back to the menu. Before the fix the dead session stayed
// listed, so reselecting it bounced forever; now the failed attach reaps it and
// it drops out of the menu.
//
// To hold the menu on a doomed-but-still-listed session, the session is spawned
// outside the menu so it survives the startup prune, then killed while the menu
// is up (detaching now leaves tm entirely, so it can't keep the menu alive).
func TestMenuReapsUnreachableSession(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmrp")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)

	// Spawn a live session daemon before the menu starts, so the menu's startup
	// prune keeps it and lists it.
	p, err := config.New()
	g.E(err)
	g.E(p.EnsureDirs())
	st := store.New(p)
	sess := store.Session{
		ID: "dead", Name: "victim", Namespace: store.DefaultNamespace,
		Shell: "/bin/sh", CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))
	g.E(app.SpawnWith(bin, p, sess))

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

	// The live session is listed in the menu.
	g.Desc("menu: %q", buf.String()).True(waitForText(buf, "victim", 10*time.Second))

	// Kill the daemon hard while the menu is up: the record and socket file linger
	// but nothing serves, so attaching will fail to dial.
	live, err := st.GetSession("dead")
	g.E(err)
	g.E(syscall.Kill(live.PID, syscall.SIGKILL))
	time.Sleep(500 * time.Millisecond)

	// Select the now-dead session and try to attach -> relay fails to dial.
	send("\r") // pick the session -> scrollback chooser
	g.True(waitForText(buf, "All history", 5*time.Second))
	send("\r") // attach -> bounces back to the menu
	time.Sleep(1500 * time.Millisecond)

	// The fix: the dead session is reaped, so it is gone from the store and the
	// menu now reports it was removed rather than silently re-listing it.
	left, _ := st.ListSessions()
	g.Desc("dead session should have been reaped").Len(left, 0)
	g.True(waitForText(buf, "unreachable", 5*time.Second))

	_, _ = pt.Write([]byte{0x03}) // quit the menu
	_ = c.Wait()
}
