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
	"time"

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

// softReset is sent before replaying history so the terminal recovers from
// attaching into the middle of a full-screen app: a VT soft terminal reset
// (DECSTR), plus clearing the keyboard protocols DECSTR does not cover (kitty
// flags, xterm modifyOtherKeys) — a stale protocol left on the terminal would
// re-encode keys, taking the relay's Ctrl-\ menu key with it. The state
// restore that follows the history re-arms them when the session's app has
// them on (see termModes).
var softReset = []byte("\x1b[!p\x1b[=0;1u\x1b[>4m")

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
	sb    *scrollback
	ln    net.Listener

	mu     sync.Mutex
	client *proto.Conn // currently attached client, or nil
	// cleared records that the session's history was wiped (MsgClear), so an
	// attach that finds nothing to replay can say why instead of showing a blank
	// screen (see clearedHint). Never reset: it only matters while the scrollback
	// is still empty — any later output makes the replay non-empty again.
	cleared bool
	// cols/rows mirror the PTY's current size, so an attach can tell whether its
	// resize actually changed anything: an unchanged size delivers no SIGWINCH,
	// which is what decides whether a full-screen app needs the repaint nudge
	// (see nudgeRepaint), and the nudge can tell whether a real resize landed
	// mid-nudge and should win.
	cols, rows int

	done chan struct{}
	once sync.Once
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

	sb, err := newScrollback(p.LogFile(sess.ID))
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
		cols:  defaultCols,
		rows:  defaultRows,
		done:  make(chan struct{}),
	}
	go d.acceptLoop()
	go d.ptyLoop()

	return d, nil
}

// Wait blocks until the shell exits and cleanup has run.
func (d *Daemon) Wait() error {
	<-d.done

	return nil
}

// Close forces the session to shut down, killing the shell outright. The kill
// (see killShell) is needed on top of shutdown's PTY close: closing the PTY
// only delivers SIGHUP to the foreground process group, which anything can trap
// or ignore — and since shutdown deletes the session's record, processes
// surviving it would live on as orphans nothing tracks. The natural-exit path
// (ptyLoop reaching shutdown) never signals, so background jobs meant to
// outlive their shell (nohup and the like) still can.
func (d *Daemon) Close() error {
	killShell(d.pty)
	d.shutdown()

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
	d.shutdown()
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
// interleave ahead of it — including the repaint a resize provokes from a
// running full-screen app, which is why the resize also happens under the
// lock. It returns false if the client went away during the replay.
func (d *Daemon) register(c *proto.Conn, att proto.Attach) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	resized := d.resizeTo(int(att.Cols), int(att.Rows))

	if d.client != nil {
		_ = d.client.Close()
	}

	d.client = c

	var alt bool

	if att.Replay {
		hist, restore, replayAlt := d.replayWindow(int(att.Rows))
		if !replay(c, hist, restore) {
			d.client = nil

			return false
		}

		alt = replayAlt
	} else {
		// Resuming a session whose screen is still up: nothing to replay. The
		// menu may just have closed over it, though, so the repaint decision
		// below still applies.
		_, alt = d.sb.TermState()
	}

	// The session is inside a full-screen app, whose alt-screen content is not
	// recorded — only the app itself can rebuild it, whether a replay just
	// re-entered its screen or a closing menu left a hole in it. A size change
	// above already made it repaint; an attach at the same size delivered no
	// SIGWINCH, so nudge one out of it. The mutex order guarantees the repaint
	// reaches the client after the replay.
	if alt && !resized {
		go d.nudgeRepaint(d.cols, d.rows)
	}

	return true
}

// resizeTo applies an attach's terminal size to the PTY when it is valid and
// actually different, reporting whether a resize happened. The caller holds
// d.mu.
func (d *Daemon) resizeTo(cols, rows int) bool {
	if cols <= 0 || rows <= 0 || (cols == d.cols && rows == d.rows) {
		return false
	}

	_ = d.pty.Resize(cols, rows)
	d.cols, d.rows = cols, rows

	return true
}

// replayWindow assembles what a replaying attach sends: the last window of
// recorded output and the terminal-state restore, plus whether the session is
// on the alt screen. The log is already cooked to visible text and color (see
// cooker), so the window carries no probes to answer and no clears to re-run;
// it only needs its bare newlines turned back into CRLF for a terminal in raw
// mode. When a wipe left nothing to show, the window explains the blank screen
// instead (see clearedHint). The caller holds d.mu.
func (d *Daemon) replayWindow(rows int) (hist, restore []byte, alt bool) {
	if rows <= 0 {
		rows = defaultRows
	}

	hist = d.sb.History(rows)
	restore, alt = d.sb.TermState()

	if len(hist) == 0 && d.cleared {
		hist = clearedHint
	} else {
		hist = crlf(hist)
	}

	return hist, restore, alt
}

// replay sends the soft reset, the recorded window, and the terminal-state
// restore to c. A window is one screen of lines — bounded well under
// proto.MaxPayload by the tail the scrollback reads (see tailBytes) — so it
// goes out as a single frame, with no chunking or streaming to interrupt. The
// restore comes after the history so the history lands on the main screen:
// when the session is inside a full-screen app, the restore re-enters the alt
// screen, and the main screen it leaves behind — what the terminal shows once
// the app exits — is then the replayed history rather than leftovers of
// whatever the terminal showed before the attach. It returns false if a write
// fails.
func replay(c *proto.Conn, hist, restore []byte) bool {
	if len(hist) == 0 && len(restore) == 0 {
		return true
	}

	if err := c.Write(proto.MsgOutput, softReset); err != nil {
		return false
	}

	if len(hist) > 0 && c.Write(proto.MsgOutput, hist) != nil {
		return false
	}

	return len(restore) == 0 || c.Write(proto.MsgOutput, restore) == nil
}

// repaintDelay separates the repaint nudge's two resizes. Back-to-back they
// could collapse into one event for a busy app — signals coalesce, so it would
// wake once, read the (already restored) size, see nothing changed, and skip
// the repaint. The gap gives it time to observe the shrunken size first.
const repaintDelay = 50 * time.Millisecond

// nudgeRepaint makes the full-screen app the session is inside repaint its
// screen, by shrinking the PTY one row and restoring it: two genuine size
// changes, each delivering a real resize event (SIGWINCH on Unix, a console
// resize on ConPTY), which every full-screen app answers with a full redraw.
// The app cannot be told to repaint any other way — there is no standard
// "repaint" signal, and apps checking that the size actually changed ignore
// same-size events. Each step yields to any real resize that lands mid-nudge
// (see setSize), so a client resizing during the nudge wins.
func (d *Daemon) nudgeRepaint(cols, rows int) {
	nudgeCols, nudgeRows := cols, rows-1
	if rows <= 1 {
		nudgeCols, nudgeRows = cols+1, rows
	}

	if !d.setSize(nudgeCols, nudgeRows, cols, rows) {
		return
	}

	time.Sleep(repaintDelay)

	d.setSize(cols, rows, nudgeCols, nudgeRows)
}

// clientResize applies a resize sent by the attached client and records it in
// the size mirror, so attach-time comparisons and a nudge in flight see the
// PTY's real size.
func (d *Daemon) clientResize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if cols == d.cols && rows == d.rows {
		return
	}

	_ = d.pty.Resize(cols, rows)
	d.cols, d.rows = cols, rows
}

// setSize resizes the PTY to cols×rows provided its size is still
// fromCols×fromRows, reporting whether it did. The guard keeps a stale nudge
// step from stomping a real resize that landed in between.
func (d *Daemon) setSize(cols, rows, fromCols, fromRows int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cols != fromCols || d.rows != fromRows {
		return false
	}

	_ = d.pty.Resize(cols, rows)
	d.cols, d.rows = cols, rows

	return true
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
				d.clientResize(int(r.Cols), int(r.Rows))
			}
		case proto.MsgDetach:
			return
		}
	}
}

// shutdown runs exactly once: tells the attached client the session is over,
// closes resources, removes the session's files, and unblocks Wait.
func (d *Daemon) shutdown() {
	d.once.Do(func() {
		d.mu.Lock()
		if d.client != nil {
			_ = d.client.Write(proto.MsgExit, nil)
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
