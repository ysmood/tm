//go:build unix

package app_test

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
)

// TestSpawnDetachedDaemon builds the real tm binary, spawns a detached session
// daemon through it, then attaches over the socket to prove the re-exec +
// readiness handshake + lifecycle cleanup all work end to end.
func TestSpawnDetachedDaemon(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(90 * time.Second) // includes a one-off `go build`

	// Short runtime dir for sockets; isolated home for metadata/logs.
	rt, err := os.MkdirTemp("/tmp", "tmd")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)

	p, err := config.New()
	g.E(err)
	g.E(p.EnsureDirs())

	bin := filepath.Join(t.TempDir(), "tm")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/ysmood/tm").CombinedOutput()
	g.Desc("%s", string(out)).E(err)

	st := store.New(p)
	sess := store.Session{
		ID:        "d1",
		Name:      "d1",
		Namespace: store.DefaultNamespace,
		Shell:     "/bin/sh",
		CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))

	g.E(app.SpawnWith(bin, p, sess))

	// The daemon recorded its own PID into the session record.
	updated, err := st.GetSession("d1")
	g.E(err)
	g.Gt(updated.PID, 0)

	// Attach and run a command through the persistent shell.
	nc, err := proto.Dial(proto.SockAddr(p, "d1"))
	g.E(err)

	defer func() { _ = nc.Close() }()

	c := proto.NewConn(nc)
	g.E(c.Write(proto.MsgAttach, proto.Attach{Hist: proto.HistNone, Cols: 80, Rows: 24}.Encode()))
	g.E(c.Write(proto.MsgInput, []byte("echo spawned-ok\n")))
	g.True(readContains(nc, c, "spawned-ok", 10*time.Second))

	// Exiting the shell ends the daemon, which removes the session's files.
	g.E(c.Write(proto.MsgInput, []byte("exit\n")))
	g.True(waitGone(st, "d1", 10*time.Second))
}

func readContains(nc net.Conn, c *proto.Conn, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	var acc strings.Builder

	for {
		_ = nc.SetReadDeadline(deadline)

		mt, payload, err := c.Read()
		if err != nil {
			return false
		}

		if mt == proto.MsgOutput {
			acc.Write(payload)

			if strings.Contains(acc.String(), want) {
				return true
			}
		}
	}
}

func waitGone(st *store.Store, id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := st.GetSession(id); errors.Is(err, store.ErrNotFound) {
			return true
		}

		time.Sleep(50 * time.Millisecond)
	}

	return false
}
