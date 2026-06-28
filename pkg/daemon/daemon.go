// Package daemon implements a per-session background process: it owns the
// session's shell in a pseudo-terminal, records scrollback, and serves attach
// connections over the session socket. One session has at most one attached
// client at a time; attaching again displaces the previous client.
package daemon

import (
	"net"
	"os"
	"strings"
	"sync"

	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/pty"
	"github.com/ysmood/tm/pkg/store"
)

const (
	defaultCols = 80
	defaultRows = 24
	readBufSize = 32 * 1024
)

// softReset is a VT soft terminal reset (DECSTR), sent before replaying history
// so the terminal recovers from attaching into the middle of a full-screen app.
var softReset = []byte("\x1b[!p")

// Daemon owns one session's PTY and serves attach connections.
type Daemon struct {
	paths config.Paths
	sess  store.Session
	pty   *pty.PTY
	sb    *Scrollback
	ln    net.Listener

	mu     sync.Mutex
	client *proto.Conn // currently attached client, or nil

	done     chan struct{}
	once     sync.Once
	exitCode int
}

// Start opens the PTY running the session's shell and begins serving on the
// session socket. The PTY and accept loops run in background goroutines; use
// Wait to block until the shell exits.
func Start(p config.Paths, sess store.Session) (*Daemon, error) {
	if err := p.EnsureDirs(); err != nil {
		return nil, err
	}

	sb, err := NewScrollback(DefaultRingBytes, p.LogFile(sess.ID))
	if err != nil {
		return nil, err
	}

	shell := sess.Shell
	if shell == "" {
		shell = defaultShell()
	}

	tp, err := pty.Start(shell, nil, sessionEnv(sess), sess.Cwd, defaultCols, defaultRows)
	if err != nil {
		_ = sb.Close()

		return nil, err
	}

	ln, err := proto.Listen(proto.SockAddr(p, sess.ID))
	if err != nil {
		_ = tp.Close()
		_ = sb.Close()

		return nil, err
	}

	d := &Daemon{
		paths: p,
		sess:  sess,
		pty:   tp,
		sb:    sb,
		ln:    ln,
		done:  make(chan struct{}),
	}
	go d.acceptLoop()
	go d.ptyLoop()

	return d, nil
}

// Addr is the socket address clients dial to attach.
func (d *Daemon) Addr() string { return proto.SockAddr(d.paths, d.sess.ID) }

// Wait blocks until the shell exits and cleanup has run.
func (d *Daemon) Wait() error {
	<-d.done

	return nil
}

// ExitCode returns the shell's exit code (valid after Wait).
func (d *Daemon) ExitCode() int { return d.exitCode }

// Close forces the session to shut down, terminating the shell.
func (d *Daemon) Close() error {
	d.shutdown(-1)

	return nil
}

// ptyLoop pumps PTY output to scrollback and the attached client until the
// shell exits.
func (d *Daemon) ptyLoop() {
	buf := make([]byte, readBufSize)

	for {
		n, err := d.pty.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			d.sb.Write(chunk)
			d.mu.Lock()
			if d.client != nil {
				werr := d.client.Write(proto.MsgOutput, chunk)
				if werr != nil {
					_ = d.client.Close()
					d.client = nil
				}
			}
			d.mu.Unlock()
		}

		if err != nil {
			break
		}
	}

	_ = d.pty.Wait()
	d.shutdown(d.pty.ExitCode())
}

func (d *Daemon) acceptLoop() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return // listener closed during shutdown
		}

		go d.handleConn(conn)
	}
}

func (d *Daemon) handleConn(nc net.Conn) {
	c := proto.NewConn(nc)
	defer func() { _ = c.Close() }()

	mt, payload, err := c.Read()
	if err != nil {
		return
	}

	// A MsgSwitch connection is a control request from a tm running inside this
	// session: forward it to the attached relay and close, without attaching (so
	// the current client is not displaced).
	if mt == proto.MsgSwitch {
		d.forwardSwitch(payload)

		return
	}

	if mt != proto.MsgAttach {
		return
	}

	att, err := proto.DecodeAttach(payload)
	if err != nil {
		return
	}

	if !d.register(c, att) {
		return
	}

	d.serveInput(c)

	d.mu.Lock()
	if d.client == c {
		d.client = nil
	}
	d.mu.Unlock()
}

// forwardSwitch relays a switch request to the currently attached client (the
// relay), telling it to re-attach to another session. It is best-effort: with no
// client attached there is nothing to hand over.
func (d *Daemon) forwardSwitch(payload []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.client != nil {
		_ = d.client.Write(proto.MsgSwitchTo, payload)
	}
}

// register makes c the active client (displacing any previous one) and replays
// the requested history under the lock, so live output can't interleave ahead
// of it. It returns false if the client went away during the replay.
func (d *Daemon) register(c *proto.Conn, att proto.Attach) bool {
	rows := int(att.Rows)

	if att.Cols > 0 && att.Rows > 0 {
		_ = d.pty.Resize(int(att.Cols), int(att.Rows))
	} else {
		rows = defaultRows
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.client != nil {
		_ = d.client.Close()
	}

	d.client = c

	// Strip query sequences so replaying history can't make the attaching
	// terminal answer probes and inject the replies into the session.
	hist := sanitizeReplay(d.sb.History(att.Hist, int(att.Lines), rows))
	if len(hist) == 0 {
		return true
	}

	if !replay(c, hist) {
		d.client = nil

		return false
	}

	return true
}

// replay sends the soft reset followed by the recorded history to c, split into
// frames no larger than proto.MaxPayload. "All history" can be many megabytes,
// while a single frame is capped, so the history must be chunked — otherwise the
// oversized frame is rejected, the connection drops, and the attach silently
// bounces back to the menu. It returns false if any write fails.
func replay(c *proto.Conn, hist []byte) bool {
	if err := c.Write(proto.MsgOutput, softReset); err != nil {
		return false
	}

	for off := 0; off < len(hist); off += proto.MaxPayload {
		end := min(off+proto.MaxPayload, len(hist))
		if err := c.Write(proto.MsgOutput, hist[off:end]); err != nil {
			return false
		}
	}

	return true
}

// serveInput processes client frames until detach, a read error, or EOF.
func (d *Daemon) serveInput(c *proto.Conn) {
	for {
		mt, payload, err := c.Read()
		if err != nil {
			return
		}

		switch mt {
		case proto.MsgInput:
			_, _ = d.pty.Write(payload)
		case proto.MsgResize:
			if r, derr := proto.DecodeResize(payload); derr == nil {
				_ = d.pty.Resize(int(r.Cols), int(r.Rows))
			}
		case proto.MsgDetach:
			return
		}
	}
}

// shutdown runs exactly once: notifies the client, closes resources, removes the
// session's files, and unblocks Wait.
func (d *Daemon) shutdown(code int) {
	d.once.Do(func() {
		d.exitCode = code
		d.mu.Lock()
		if d.client != nil {
			_ = d.client.Write(proto.MsgExit, proto.EncodeExit(int32(code)))
			_ = d.client.Close()
			d.client = nil
		}
		d.mu.Unlock()

		_ = d.ln.Close()
		_ = d.pty.Close()
		_ = d.sb.Close()
		_ = store.New(d.paths).DeleteSession(d.sess.ID)
		close(d.done)
	})
}

// sessionEnv returns the environment for the session's shell: the daemon's
// environment with TERM ensured and config.EnvSession set to the session id, so a
// tm launched inside the session knows which session it is in. Any EnvSession
// inherited from an outer session is dropped, so a nested session reports itself.
func sessionEnv(sess store.Session) []string {
	out := make([]string, 0, len(os.Environ())+2)
	hasTerm := false

	for _, e := range os.Environ() {
		if strings.HasPrefix(e, config.EnvSession+"=") {
			continue // replaced below so nesting reports the innermost session
		}

		if strings.HasPrefix(e, "TERM=") {
			hasTerm = true
		}

		out = append(out, e)
	}

	if !hasTerm {
		out = append(out, "TERM=xterm-256color")
	}

	return append(out, config.EnvSession+"="+sess.ID)
}
