// Package daemon implements a per-session background process: it owns the
// session's shell in a pseudo-terminal, records scrollback, and serves attach
// connections over the session socket. One session has at most one attached
// client at a time; attaching again displaces the previous client.
package daemon

import (
	"bytes"
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

// clearedHint is replayed in place of history when a cleared session is
// attached with nothing recorded since the wipe: the shell prints its prompt
// only when asked, so the replay would otherwise be blank — no prompt, no
// cursor — as if the attach hung. The dim line (the grey of the tm notices)
// explains the emptiness and how to get a prompt. It is served at attach time,
// not written to the log, so it never becomes part of the recorded history.
var clearedHint = []byte(
	"\x1b[38;5;245m[tm history cleared here - might need to press enter for a prompt]\x1b[0m\r\n")

// Daemon owns one session's PTY and serves attach connections.
type Daemon struct {
	paths config.Paths
	sess  store.Session
	pty   *pty.PTY
	sb    *Scrollback
	ln    net.Listener

	mu     sync.Mutex
	client *proto.Conn // currently attached client, or nil
	// cleared records that the session's history was wiped (MsgClear), so an
	// attach that finds nothing to replay can say why instead of showing a blank
	// screen (see clearedHint). Never reset: it only matters while the scrollback
	// is still empty — any later output makes the replay non-empty again.
	cleared bool

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

	// The session's log lives in the session's own directory, which normally
	// exists already (the store wrote the metadata there); create it anyway so a
	// daemon started against a hand-made session still records its scrollback.
	if err := p.EnsureSessionDir(sess.ID); err != nil {
		return nil, err
	}

	sb, err := NewScrollback(p.LogFile(sess.ID))
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

// Close forces the session to shut down, killing the shell outright. The kill
// (see killShell) is needed on top of shutdown's PTY close: closing the PTY
// only delivers SIGHUP to the foreground process group, which anything can trap
// or ignore — and since shutdown deletes the session's record, processes
// surviving it would live on as orphans nothing tracks. The natural-exit path
// (ptyLoop reaching shutdown) never signals, so background jobs meant to
// outlive their shell (nohup and the like) still can.
func (d *Daemon) Close() error {
	killShell(d.pty)
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

	// A MsgKill connection ends the session without attaching: shutdown terminates
	// the shell, tells any attached client the session is over, and removes the
	// session's files. It runs synchronously here, so the deferred close of this
	// connection tells the killer that teardown is done.
	if mt == proto.MsgKill {
		_ = d.Close()

		return
	}

	// A MsgClear connection wipes the session's recorded history — its log file —
	// without attaching, so a later attach replays none of it. The session and any
	// attached client run on undisturbed; the deferred close of this connection
	// tells the requester the wipe is done.
	if mt == proto.MsgClear {
		_ = d.sb.Clear()

		d.mu.Lock()
		d.cleared = true
		d.mu.Unlock()

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
// the last window of recorded output under the lock, so live output can't
// interleave ahead of it. It returns false if the client went away during the
// replay.
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

	if !att.Replay {
		return true // resuming a session whose screen is still up: nothing to redraw
	}

	// The log is already cooked to visible text and color (see cooker), so the
	// window carries no probes to answer and no clears to re-run; it only needs
	// its bare newlines turned back into CRLF for a terminal in raw mode.
	hist := d.sb.History(rows)

	// A replay was asked for, but the wipe left nothing to show: explain the blank
	// screen instead of replaying it.
	if len(hist) == 0 && d.cleared {
		hist = clearedHint
	} else {
		hist = crlf(hist)
	}

	if !replay(c, hist) {
		d.client = nil

		return false
	}

	return true
}

// replay sends the soft reset followed by the recorded window to c. A window is
// one screen of lines — bounded well under proto.MaxPayload by the tail the
// scrollback reads (see TailBytes) — so it goes out as a single frame, with no
// chunking or streaming to interrupt. It returns false if a write fails.
func replay(c *proto.Conn, hist []byte) bool {
	if len(hist) == 0 {
		return true
	}

	if err := c.Write(proto.MsgOutput, softReset); err != nil {
		return false
	}

	return c.Write(proto.MsgOutput, hist) == nil
}

// crlf turns the cooked log's bare newlines back into CRLF, so a replay lands
// each line at column 0 on a terminal in raw mode (where a lone LF only moves
// down). The cooked stream has no carriage returns of its own, so this can't
// double up an existing one.
func crlf(p []byte) []byte {
	return bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))
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
// environment with TERM ensured, config.EnvSession set to the session id and
// config.EnvNamespace set to the session's namespace, so a tm launched inside the
// session knows which session and namespace it is in. Any EnvSession/EnvNamespace
// inherited from an outer session is dropped and replaced, so a nested session
// reports its own identity and namespace rather than a stale inherited one.
func sessionEnv(sess store.Session) []string {
	out := make([]string, 0, len(os.Environ())+3)
	hasTerm := false

	for _, e := range os.Environ() {
		if strings.HasPrefix(e, config.EnvSession+"=") || strings.HasPrefix(e, config.EnvNamespace+"=") {
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

	out = append(out, config.EnvSession+"="+sess.ID)
	if sess.Namespace != "" {
		out = append(out, config.EnvNamespace+"="+sess.Namespace)
	}

	return out
}
