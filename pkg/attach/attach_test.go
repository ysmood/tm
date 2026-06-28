//go:build unix

package attach

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/proto"
)

// safeBuf is a goroutine-safe accumulator for the relay's output.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.String()
}

// echoServer accepts one connection, consumes the Attach, then echoes each
// Input back as Output until the client detaches. It records whether it saw a
// Detach (so the test can confirm the detach key never leaked as input).
func echoServer(g got.G, addr string) (detached func() bool) {
	ln, err := proto.Listen(addr)
	g.E(err)
	g.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex

	sawDetach := false

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}

		c := proto.NewConn(conn)
		if mt, _, rerr := c.Read(); rerr != nil || mt != proto.MsgAttach {
			return
		}

		for {
			mt, payload, rerr := c.Read()
			if rerr != nil {
				return
			}

			switch mt {
			case proto.MsgInput:
				_ = c.Write(proto.MsgOutput, payload)
			case proto.MsgDetach:
				mu.Lock()
				sawDetach = true
				mu.Unlock()

				return
			}
		}
	}()

	return func() bool {
		mu.Lock()
		defer mu.Unlock()

		return sawDetach
	}
}

func TestRelayForwardsAndDetaches(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(10 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tma")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	addr := filepath.Join(rt, "s.sock")

	detached := echoServer(g, addr)

	inR, inW := io.Pipe()
	out := &safeBuf{}

	done := make(chan error, 1)

	go func() {
		done <- runRelay(Options{Hist: proto.HistNone}, inR, out, 0, false,
			func(string) string { return addr }, "x")
	}()

	// Input is forwarded and echoed back as output.
	_, err = inW.Write([]byte("ping"))
	g.E(err)
	g.True(waitFor(func() bool { return strings.Contains(out.String(), "ping") }, 5*time.Second))

	// The detach key ends the relay and is sent as Detach, not as input.
	_, err = inW.Write([]byte{DefaultDetachKey})
	g.E(err)

	select {
	case rerr := <-done:
		g.E(rerr)
	case <-time.After(5 * time.Second):
		g.Logf("relay did not return after detach key")
		g.FailNow()
	}

	g.True(detached())
	g.False(bytes.Contains([]byte(out.String()), []byte{DefaultDetachKey}))
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}

		time.Sleep(10 * time.Millisecond)
	}

	return false
}
