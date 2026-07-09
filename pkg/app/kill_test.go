//go:build unix

package app

import (
	"os"
	"testing"
	"time"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/daemon"
	"github.com/ysmood/tm/pkg/store"
)

// KillSession on a live session asks its daemon to shut down: the shell
// terminates and the daemon removes the session's files itself; KillSession
// returns only once that teardown is done.
func TestControllerKillSessionLive(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(15 * time.Second)

	// Sockets need a short path (sun_path limit), so keep Runtime under /tmp.
	rt, err := os.MkdirTemp("/tmp", "tmk")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	p := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(p.EnsureDirs())
	st := store.New(p)

	sess := store.Session{
		ID: "k1", Name: "k1", Namespace: store.DefaultNamespace,
		Shell: "/bin/sh", PID: os.Getpid(), CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	ctrl := &controller{st: st}
	g.E(ctrl.KillSession("k1"))

	g.E(d.Wait()) // the daemon tore down in response to the kill

	_, gerr := st.GetSession("k1")
	g.Is(gerr, store.ErrNotFound)
}

// KillSession on a session whose daemon is unreachable (no socket to dial)
// removes it from the store directly, so [kill session] also clears dead
// entries instead of erroring on them.
func TestControllerKillSessionDead(t *testing.T) {
	g := got.T(t)

	p := config.Paths{Home: t.TempDir(), Runtime: t.TempDir()}
	g.E(p.EnsureDirs())
	st := store.New(p)
	g.E(st.SaveSession(store.Session{ID: "dead", Name: "dead", Namespace: store.DefaultNamespace}))

	ctrl := &controller{st: st}
	g.E(ctrl.KillSession("dead"))

	_, err := st.GetSession("dead")
	g.Is(err, store.ErrNotFound)
}
