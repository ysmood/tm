//go:build unix

package daemon_test

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/daemon"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
)

func setupDaemon(t *testing.T) (got.G, *store.Store, config.Paths) {
	g := got.T(t)
	g.PanicAfter(15 * time.Second)
	// Sockets need a short path (sun_path limit), so keep Runtime under /tmp;
	// metadata/logs can live in the deeper test temp dir.
	rt, err := os.MkdirTemp("/tmp", "tmd")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	p := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(p.EnsureDirs())

	return g, store.New(p), p
}

func makeSession(g got.G, st *store.Store, id string) store.Session {
	sess := store.Session{
		ID:        id,
		Name:      id,
		Namespace: store.DefaultNamespace,
		Shell:     "/bin/sh",
		PID:       1,
		CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))

	return sess
}

// dialAttach connects, sends an Attach, and returns the framed conn plus the raw
// net.Conn (for read deadlines).
func dialAttach(g got.G, addr string, att proto.Attach) (net.Conn, *proto.Conn) {
	nc, err := proto.Dial(addr)
	g.E(err)

	c := proto.NewConn(nc)
	g.E(c.Write(proto.MsgAttach, att.Encode()))

	return nc, c
}

// readUntil reports whether Output containing want arrives before an Exit/timeout.
func readUntil(nc net.Conn, c *proto.Conn, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	var acc strings.Builder

	for {
		_ = nc.SetReadDeadline(deadline)

		mt, payload, err := c.Read()
		if err != nil {
			return false
		}

		switch mt {
		case proto.MsgOutput:
			acc.Write(payload)

			if strings.Contains(acc.String(), want) {
				return true
			}
		case proto.MsgExit:
			return strings.Contains(acc.String(), want)
		}
	}
}

func TestAttachInputOutputAndExit(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "echo1")

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	nc, c := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistNone, Cols: 80, Rows: 24})
	defer nc.Close()

	g.E(c.Write(proto.MsgInput, []byte("echo hello-tm\n")))
	found := readUntil(nc, c, "hello-tm", 10*time.Second)
	g.True(found)

	// Exiting the shell ends the session and removes its metadata.
	g.E(c.Write(proto.MsgInput, []byte("exit\n")))
	g.E(d.Wait())

	_, gerr := st.GetSession(sess.ID)
	g.Is(gerr, store.ErrNotFound)
}

func TestDetachThenReattach(t *testing.T) {
	g, st, p := setupDaemon(t)
	sess := makeSession(g, st, "persist1")

	d, err := daemon.Start(p, sess)
	g.E(err)

	defer d.Close()

	// First attach: produce a marker, then detach.
	nc1, c1 := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistNone, Cols: 80, Rows: 24})
	g.E(c1.Write(proto.MsgInput, []byte("echo first-attach\n")))
	found := readUntil(nc1, c1, "first-attach", 10*time.Second)
	g.True(found)
	g.E(c1.Write(proto.MsgDetach, nil))
	nc1.Close()

	// Session still alive: reattach with full history and see the earlier marker.
	nc2, c2 := dialAttach(g, d.Addr(), proto.Attach{Hist: proto.HistAll, Cols: 80, Rows: 24})
	defer nc2.Close()

	found = readUntil(nc2, c2, "first-attach", 10*time.Second)
	g.True(found)
}
