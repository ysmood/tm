// Package attach implements the relay: a minimal raw-passthrough client that
// connects to a session daemon's socket, proxies the local terminal's I/O to
// the session, and returns when the user presses the menu key, the session
// exits, or local input ends. It is run as the hidden `tm __attach` subcommand.
package attach

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/proto"
	"golang.org/x/term"
)

// DefaultMenuKey is Ctrl-\ (0x1c): pressing it stops proxying and returns to tm
// so it can open the interactive menu (switch sessions, start a new one, or leave
// tm for the shell). The session keeps running in the background while the menu is
// up. Run reports the press to its caller, which reopens the menu and re-attaches.
const DefaultMenuKey = 0x1c

// Leaving a session returns the local terminal to a sane baseline. A session's
// PTY has its own terminal state, independent of the outer terminal's: a
// full-screen program run inside it (vim, less, htop, a git pager, any TUI)
// switches on the alternate screen buffer, mouse reporting, a scroll region,
// bracketed paste, etc. Those modes are forwarded to the outer terminal live, and
// leaving abandons the session mid-state, so without undoing them the outer
// terminal is left, e.g., stuck in the alternate screen buffer or with mouse
// reporting on, where the wheel emits escape codes instead of scrolling.
// Multiplexers (tmux, screen) reset the terminal on detach for exactly this reason.
//
// The reset is split in two so it never destroys scrollback the user wants to keep:
//
//   - altScreenExit leaves the alternate screen buffer. It is sent ONLY when the
//     session actually left the terminal in the alt screen (tracked by watching the
//     forwarded output). Sending it otherwise makes many terminals "restore" a
//     stale, usually empty primary screen — wiping the session's output and its
//     scrollback. That is exactly what made detaching/exiting from a plain shell
//     clear the history.
//   - terminalModesReset undoes the input/display modes a full-screen program may
//     have switched on. None of these touch scrollback, so it is always safe to send.
//
// It deliberately stops short of a full RIS reset (ESC c), which would also wipe
// the terminal's scrollback — the very thing the user wants back.
const (
	altScreenExit = "\x1b[?1049l" // leave the alternate screen buffer (restore main + scrollback)

	terminalModesReset = "\x1b[?1000l\x1b[?1002l\x1b[?1003l" + // disable mouse click / button / any-motion tracking
		"\x1b[?1005l\x1b[?1006l\x1b[?1015l" + // disable UTF-8 / SGR / urxvt mouse encodings
		"\x1b[?1004l" + // disable focus reporting
		"\x1b[?2004l" + // disable bracketed paste
		"\x1b[?7h" + // re-enable auto-wrap (DECAWM)
		"\x1b[?1l\x1b>" + // normal cursor keys (DECCKM) and keypad (DECKPNM)
		// Reset the scroll region to the full screen (DECSTBM). DECSTBM homes the
		// cursor as a side effect, so wrap it in save/restore-cursor (DECSC/DECRC):
		// otherwise, when the shell exits and app.Run reopens the inline menu, the
		// menu would render from the homed cursor at the top and erase the screen
		// below it — wiping the just-exited session's output and the scrollback view.
		"\x1b7\x1b[r\x1b8" +
		"\x1b(B" + // select US-ASCII for the G0 charset
		"\x1b[?25h" + // show the cursor
		"\x1b[m" // reset SGR colors / attributes
)

// TerminalModesReset is the scrollback-preserving reset: it undoes a session's
// terminal modes without leaving the alternate screen, so detaching or exiting at
// a shell prompt keeps the terminal's scrollback intact.
var TerminalModesReset = []byte(terminalModesReset)

// TerminalRestore is the full reset for when the session left the terminal in the
// alternate screen: leave the alt screen (restoring the main screen + scrollback),
// then reset the modes. Used where the screen is redrawn next (a session switch)
// or where a full-screen app was up when leaving.
var TerminalRestore = []byte(altScreenExit + terminalModesReset)

// SwitchReset is TerminalRestore plus a carriage return, written before re-attaching
// to a switch target. The target's history replay is a raw byte stream with no
// absolute positioning, so it must start from column 0 to line up with how it was
// recorded. The reset leaves the cursor wherever the leaving session's prompt sat —
// mid-line — so the replay would otherwise begin off-column, and a recorded partial
// last line (e.g. zsh's prompt, whose PROMPT_SP EOL marker "%" is baked into the
// scrollback) no longer lines up under what overwrites it, leaving a stray "%" on
// screen. The CR only moves to column 0 of the current row — not home — so the
// leaving session's output scrolls up into the scrollback as the target replays,
// rather than being overwritten. The plain restores omit it, so leaving to the
// inline menu (exit) or the launching shell (detach) renders exactly in place.
var SwitchReset = []byte(altScreenExit + terminalModesReset + "\r")

// RestoreFor returns the reset to write when leaving a session: the full restore
// when the session left the terminal in the alternate screen, otherwise the
// scrollback-preserving modes-only reset.
func RestoreFor(inAltScreen bool) []byte {
	if inAltScreen {
		return TerminalRestore
	}

	return TerminalModesReset
}

// Options configures an attach session.
type Options struct {
	Hist    proto.HistMode
	Lines   uint32
	MenuKey byte
	// Name is the display name of the session first attached to, used for the
	// status notice printed if that session's shell exits. It is empty when the
	// name is unknown (e.g. the bare __attach relay), which suppresses the notice.
	Name string
}

// Outcome reports why the relay stopped, so the caller (app.Run) can decide what
// to do next.
type Outcome int

const (
	// OutcomeInputEnded means local input closed (the terminal went away). There is
	// nothing to return to, so the caller leaves tm for the launching shell.
	OutcomeInputEnded Outcome = iota
	// OutcomeMenu means the user pressed the menu key (Ctrl-\). The session keeps
	// running; the caller opens the menu over it and re-attaches.
	OutcomeMenu
	// OutcomeSessionExited means the session's shell exited (or its daemon dropped
	// the relay), so the session is gone. The caller returns to the top-level menu
	// rather than leaving tm, so the user can pick or start another session.
	OutcomeSessionExited
)

// Run connects to the session id under p, switches the terminal to raw mode, and
// proxies I/O until the user presses the menu key, the session exits, or local
// input ends. The returned Outcome tells the caller (app.Run) what to do next:
// OutcomeMenu re-opens the menu over the still-running session, OutcomeSessionExited
// returns to the top-level menu, and OutcomeInputEnded leaves tm. When the session
// asks the relay to switch (a tm running inside it picked another session), Run
// leaves the current session running and re-attaches to the target — so switching
// moves this one terminal between sessions instead of nesting relays.
// The returned bool reports whether the session left the terminal in the
// alternate screen, so a caller that resets the terminal later (app.Run, when the
// menu key returned OutcomeMenu and the user then detaches) knows whether the
// reset must leave the alt screen or preserve the scrollback.
// A non-nil Paused (returned only with OutcomeMenu) means the menu key landed
// mid-history-replay: the attachment is suspended, not detached, so the caller
// must either Resume it (esc back into the session) or Abort it (anything else)
// once the menu closes.
func Run(p config.Paths, id string, opt Options) (Outcome, bool, *Paused, error) {
	in, closeIn := openInput()
	defer closeIn()

	return runRelay(opt, in, os.Stdout, int(in.Fd()), true,
		func(sid string) string { return proto.SockAddr(p, sid) }, id, nil)
}

// relay holds the state shared across a relay's session iterations: a single
// input reader forwards keystrokes to whichever session is current, so switching
// sessions never leaves a second reader competing for the terminal's input.
type relay struct {
	menuKey byte

	ready     chan struct{} // closed once the first session connection is set
	readyOnce sync.Once

	// menu records that input ended because the user pressed the menu key (rather
	// than the local input closing), so the caller knows to open the menu. It is
	// set just before the input-ended channel closes, which the reader of that
	// channel synchronizes with, so a plain Load sees the right value.
	menu atomic.Bool

	// alt records whether the current session has the outer terminal in the
	// alternate screen buffer, tracked from the forwarded output. It decides whether
	// leaving the session must send altScreenExit (which would drop scrollback if the
	// session is on the main screen). Stored atomically so the goroutine that returns
	// the relay can read it while pumpOutput is still updating it.
	alt atomic.Bool
	// altCarry holds the tail of the last forwarded chunk so an alt-screen toggle
	// split across two chunks is still recognized. Only touched by pumpOutput, which
	// runs one chunk at a time, so it needs no lock.
	altCarry []byte

	// replaying is true between an attach and the daemon's MsgReplayDone: the
	// output arriving is recorded history, not live. It decides what the menu key
	// does — mid-replay it pauses the replay (see session) instead of detaching.
	replaying atomic.Bool
	// pauseReq asks pumpOutput to park at its next check so the menu can open over
	// a paused replay. Set only by session, after the menu key arrived mid-replay.
	pauseReq atomic.Bool
	// pending is the unwritten tail of a frame pumpOutput was forwarding when it
	// parked; the resumed pump flushes it first, continuing the byte stream exactly
	// where it stopped. Handed between pump goroutines via their result channel, so
	// it needs no lock.
	pending []byte

	mu   sync.Mutex
	conn *proto.Conn // the session connection input is currently forwarded to
}

// relayFor returns the relay driving this invocation: when resuming, the
// suspended one — its pending output, alt tracking and replay phase carry over —
// with the pump unparked and the menu flag down for the fresh menu-key round;
// otherwise a new relay.
func relayFor(opt Options, resume *Paused) *relay {
	if resume == nil {
		return &relay{menuKey: opt.MenuKey, ready: make(chan struct{})}
	}

	r := resume.r
	r.menu.Store(false)
	r.pauseReq.Store(false)

	return r
}

func (r *relay) setConn(c *proto.Conn) {
	r.mu.Lock()
	r.conn = c
	r.mu.Unlock()

	r.readyOnce.Do(func() { close(r.ready) })
}

func (r *relay) curConn() *proto.Conn {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.conn
}

// runRelay drives the attach loop with explicit I/O endpoints so the relay can be
// tested without a real terminal. addrOf resolves a session id to its socket
// address. It starts one input reader for the whole relay lifetime, then attaches
// to id, re-attaching to the target whenever a session asks it to switch, and
// returns when the user detaches or a session exits. A non-nil resume continues
// that suspended attachment (its connection, pending output and replay state)
// instead of dialing a new one.
func runRelay(
	opt Options, in io.Reader, out io.Writer, inFd int, raw bool,
	addrOf func(string) string, id string, resume *Paused,
) (outcome Outcome, leftAlt bool, paused *Paused, err error) {
	if opt.MenuKey == 0 {
		opt.MenuKey = DefaultMenuKey
	}

	r := relayFor(opt, resume)

	// curName tracks the session the relay is currently on (updated as it switches
	// internally), so the exit notice below names the right session.
	curName := opt.Name

	if raw && term.IsTerminal(inFd) {
		restore, merr := rawTerminal(inFd)
		if merr != nil {
			return OutcomeInputEnded, false, nil, merr
		}

		defer func() { restore(out, outcome, curName, r.alt.Load()) }()
	}

	ended := make(chan struct{})

	var once sync.Once

	onEnd := func() { once.Do(func() { close(ended) }) }

	// On any exit, signal end so the input reader unblocks even if it is still
	// waiting for the first connection (e.g. the initial dial failed).
	defer onEnd()

	go r.inputLoop(in, ended, onEnd)

	// cur is the suspended attachment to continue instead of dialing; only the
	// first session iteration can be a resumption.
	cur := resume

	for {
		next, oc, p, serr := r.session(addrOf(id), opt, out, ended, cur)
		cur = nil

		if serr != nil {
			return OutcomeInputEnded, r.alt.Load(), nil, serr
		}

		if p != nil {
			// The menu key mid-replay: the attachment is suspended, not detached. Hand
			// the caller everything Resume needs to continue it (or Abort to drop it).
			p.r, p.id, p.addrOf = r, id, addrOf
			p.opt = opt
			p.opt.Name = curName

			return OutcomeMenu, r.alt.Load(), p, nil
		}

		if next == nil {
			// No switch: the menu key (OutcomeMenu), the session exiting
			// (OutcomeSessionExited), or local input ending (OutcomeInputEnded).
			// app.Run reads the outcome to decide whether to re-open the menu over the
			// session, fall back to the top-level menu, or leave tm. leftAlt lets it
			// preserve scrollback when the session was not on the alt screen.
			return oc, r.alt.Load(), nil, nil
		}

		announceSwitch(out, next)

		id, opt.Hist, opt.Lines, curName = next.ID, next.Hist, next.Lines, next.Name
	}
}

// rawTerminal puts the relay's terminal into raw mode and returns the teardown
// to run once the relay stops. The teardown hands the terminal back as it was
// found — but only when the relay is truly done with this screen: leaving tm,
// or stopping because the session ended (then app.Run draws the top-level menu
// over a clean screen). When it stops just so app.Run can open its inline menu
// (OutcomeMenu, a paused replay included), the reset is skipped: the menu draws
// beneath the session's screen, like running `tm` inside it, so wiping it would
// drop the scrollback. RestoreFor sends the alt-screen exit only when the
// session actually left the terminal in the alt screen (alt), so exiting at a
// plain shell prompt keeps the scrollback; app.Run resets the terminal itself
// if it then switches sessions or detaches. A session whose shell exited is
// noted before app.Run draws the top-level menu, so the name of the session
// that just ended stays in the scrollback.
func rawTerminal(inFd int) (func(out io.Writer, outcome Outcome, curName string, alt bool), error) {
	old, err := term.MakeRaw(inFd)
	if err != nil {
		return nil, err
	}

	return func(out io.Writer, outcome Outcome, curName string, alt bool) {
		if outcome != OutcomeMenu {
			_, _ = out.Write(RestoreFor(alt))
		}

		if outcome == OutcomeSessionExited && curName != "" {
			_, _ = out.Write(ExitedSessionNotice(curName))
		}

		_ = term.Restore(inFd, old)
	}, nil
}

// announceSwitch prepares the terminal for switching to another session. The
// session being left may have left the outer terminal in the alternate screen
// buffer, with mouse reporting on, or the cursor hidden — e.g. a full-screen
// app, or tm's own menu (which runs in the alternate screen), was running in
// it. Unlike a detach, nothing else resets the terminal here: the menu's
// teardown went to the old session's PTY, which the relay has already left, so
// it never reaches this terminal. Reset to baseline and return to column 0
// before re-attaching, so the next session's history replay lands from a known
// column with a visible cursor and clean modes, instead of inheriting the
// previous session's leftover modes or mid-line cursor position (which leaves a
// stray zsh "%" on screen). The switch is then noted before the target's
// history replays over it — skipped when the target carries no name (e.g. an
// older sender), so the notice never renders an empty name.
func announceSwitch(out io.Writer, next *proto.SwitchTarget) {
	_, _ = out.Write(SwitchReset)

	if next.Name != "" {
		_, _ = out.Write(SwitchedNotice(next.Name))
	}
}

// Paused is a session attachment suspended mid-history-replay so the menu could
// open promptly. The connection stays up but the relay reads no more frames, so
// the daemon's replay stalls on socket backpressure — no further history is
// loaded or rendered until Resume, or ever if the caller Aborts instead (the
// daemon aborts the rest of the replay when the connection closes).
type Paused struct {
	r      *relay
	nc     net.Conn
	c      *proto.Conn
	opt    Options
	id     string
	addrOf func(string) string
}

// Resume reopens the terminal and continues the paused attachment: the replay
// picks up exactly where it stopped, and the relay then behaves as if never
// interrupted (the menu key, switching, exiting all work as usual).
func (p *Paused) Resume() (Outcome, bool, *Paused, error) {
	in, closeIn := openInput()
	defer closeIn()

	return runRelay(p.opt, in, os.Stdout, int(in.Fd()), true, p.addrOf, p.id, p)
}

// Abort abandons the paused attachment instead of resuming it: closing the
// connection makes the daemon's next replay write fail, so it drops the relay
// and never loads the rest of the history. The session keeps running.
func (p *Paused) Abort() { _ = p.nc.Close() }

// session runs one attachment: it dials addr, attaches, and proxies output until
// input ends (the menu key or the local input closing), the session exits, or the
// session asks the relay to switch. A non-nil cur skips the dial and continues
// that paused attachment instead. It returns a non-nil SwitchTarget only for a
// switch and a non-nil Paused only for the menu key mid-replay (the Outcome is
// OutcomeMenu then, with the attachment left suspended for Resume or Abort);
// otherwise the Outcome says why it stopped.
func (r *relay) session(
	addr string, opt Options, out io.Writer, ended <-chan struct{}, cur *Paused,
) (*proto.SwitchTarget, Outcome, *Paused, error) {
	var (
		nc  net.Conn
		c   *proto.Conn
		err error
	)

	if cur != nil {
		nc, c = cur.nc, cur.c
	} else if nc, c, err = r.dialSession(addr, opt); err != nil {
		return nil, OutcomeInputEnded, nil, err
	}

	// The connection is closed on every way out except a pause, which hands it —
	// still attached, replay suspended — to the caller inside a Paused.
	closeConn := true
	defer func() {
		if closeConn {
			_ = nc.Close()
		}
	}()

	r.setConn(c)

	stopResize := watchResize(c)
	defer stopResize()

	pump := make(chan pumpResult, 1)
	go func() { pump <- r.pumpOutput(c, out) }()

	select {
	case <-ended:
		// The menu key mid-replay pauses instead of detaching: park the pump at its
		// next check — kicking a Read-blocked pump with an immediate deadline — and
		// wait for it to stop writing, so the menu never races leftover history onto
		// the screen. The daemon, unread, stalls mid-replay on socket backpressure.
		if r.menu.Load() && r.replaying.Load() {
			r.pauseReq.Store(true)

			_ = nc.SetReadDeadline(time.Now())

			if res := <-pump; res.paused {
				_ = nc.SetReadDeadline(time.Time{}) // rearm reads for Resume

				closeConn = false

				return nil, OutcomeMenu, &Paused{nc: nc, c: c}, nil
			}
			// The pump stopped for another reason (exit, close, switch) just as the
			// pause landed: nothing to suspend, fall through to the plain detach.
		}

		// Input ended: either the user pressed the menu key (r.menu set) or the
		// local input closed. Either way tell the daemon to drop us — the session
		// keeps running — and stop this attachment. The Outcome tells the caller
		// whether to open the menu or leave tm.
		_ = c.Write(proto.MsgDetach, nil)

		if r.menu.Load() {
			return nil, OutcomeMenu, nil, nil
		}

		return nil, OutcomeInputEnded, nil, nil
	case res := <-pump:
		// target set: switch to another session (the Outcome is unused by the
		// caller). Otherwise the session's shell exited or its daemon dropped the
		// relay — the session is gone, so app.Run falls back to the top-level menu.
		if res.target != nil {
			return res.target, OutcomeInputEnded, nil, nil
		}

		return nil, OutcomeSessionExited, nil, nil
	}
}

// dialSession dials addr and sends the attach request, returning the framed
// connection. Used only for fresh attachments — a resume reuses the paused one —
// so it also resets the per-attachment relay state: the alternate-screen
// tracking starts afresh from this attachment's own output (the replay
// re-establishes it if the session is mid-full-screen-app), and the replay
// phase is on until the daemon's MsgReplayDone.
func (r *relay) dialSession(addr string, opt Options) (net.Conn, *proto.Conn, error) {
	nc, err := proto.Dial(addr)
	if err != nil {
		return nil, nil, err
	}

	c := proto.NewConn(nc)

	cols, rows := terminalSize()

	att := proto.Attach{Hist: opt.Hist, Lines: opt.Lines, Cols: uint16(cols), Rows: uint16(rows)}
	if aerr := c.Write(proto.MsgAttach, att.Encode()); aerr != nil {
		_ = nc.Close()

		return nil, nil, aerr
	}

	r.resetAlt()
	r.replaying.Store(true)

	return nc, c, nil
}

// pumpResult is why pumpOutput stopped: a switch request (target set), a pause
// mid-replay (paused), or the connection closing / the session exiting (neither).
type pumpResult struct {
	target *proto.SwitchTarget
	paused bool
}

// pumpOutput forwards daemon output to out until the connection closes, the
// session exits, the daemon asks the relay to switch, or a pause is requested
// (the menu key mid-replay; see session). While forwarding it tracks the
// alternate-screen state so leaving the session knows whether sending
// altScreenExit is needed. A resumed pump first flushes the tail the paused one
// left in r.pending, so the byte stream continues exactly where it stopped.
func (r *relay) pumpOutput(c *proto.Conn, out io.Writer) pumpResult {
	if !r.forward(out, nil) {
		return pumpResult{paused: true}
	}

	for {
		mt, payload, err := c.Read()
		if err != nil {
			// session kicks a Read-blocked pump with an immediate read deadline so a
			// pause takes effect even when no frame is arriving; any other error means
			// the connection closed or the session exited.
			if r.pauseReq.Load() && errors.Is(err, os.ErrDeadlineExceeded) {
				return pumpResult{paused: true}
			}

			return pumpResult{}
		}

		switch mt {
		case proto.MsgOutput:
			if !r.forward(out, payload) {
				return pumpResult{paused: true}
			}
		case proto.MsgReplayDone:
			// History ends here; everything after is live output. The menu key now
			// detaches (today's behavior) instead of pausing.
			r.replaying.Store(false)
		case proto.MsgSwitchTo:
			if t, derr := proto.DecodeSwitchTarget(payload); derr == nil {
				return pumpResult{target: &t}
			}

			return pumpResult{}
		case proto.MsgExit:
			return pumpResult{}
		}
	}
}

// forwardChunk is how many bytes forward writes to the terminal between pause
// checks. Terminal rendering is the slow side of the relay — a full 1 MiB
// history frame can take on the order of a second to draw — so slicing the
// writes keeps the menu key's pause latency imperceptible during a replay.
const forwardChunk = 16 * 1024

// forward writes payload to the terminal in forwardChunk slices — flushing any
// r.pending tail a paused pump left behind first — checking between slices
// whether a pause was requested. It returns false when it parked, leaving the
// unwritten remainder in r.pending for the resumed pump to pick up.
func (r *relay) forward(out io.Writer, payload []byte) bool {
	data := payload

	if len(r.pending) > 0 {
		r.pending = append(r.pending, payload...)
		data, r.pending = r.pending, nil
	}

	for len(data) > 0 {
		if r.pauseReq.Load() {
			r.pending = data

			return false
		}

		n := min(forwardChunk, len(data))
		_, _ = out.Write(data[:n])
		r.trackAlt(data[:n])
		data = data[n:]
	}

	return true
}

// altScreenToggles maps each alternate-screen enter/exit control sequence to the
// state it leaves the terminal in (true = on the alt screen). Both the modern xterm
// 1049 mode and the older 1047/47 modes are tracked, since a session can run any of
// them. The longest is 8 bytes ("\x1b[?1049h").
var altScreenToggles = []struct {
	seq []byte
	on  bool
}{
	{[]byte("\x1b[?1049h"), true}, {[]byte("\x1b[?1049l"), false},
	{[]byte("\x1b[?1047h"), true}, {[]byte("\x1b[?1047l"), false},
	{[]byte("\x1b[?47h"), true}, {[]byte("\x1b[?47l"), false},
}

const maxAltSeq = 8 // len("\x1b[?1049h"), the longest tracked toggle

// trackAlt updates r.alt from a forwarded output chunk by finding the last
// alternate-screen toggle in it; the most recent toggle wins. altCarry stitches the
// previous chunk's tail onto this one so a toggle split across the boundary is still
// seen. resetAlt clears both at the start of each session.
func (r *relay) trackAlt(payload []byte) {
	data := payload
	if len(r.altCarry) > 0 {
		data = append(r.altCarry, payload...)
	}

	lastIdx, lastOn := -1, false

	for _, t := range altScreenToggles {
		if i := bytes.LastIndex(data, t.seq); i > lastIdx {
			lastIdx, lastOn = i, t.on
		}
	}

	if lastIdx >= 0 {
		r.alt.Store(lastOn)
	}

	// Keep only enough trailing bytes to complete a toggle split across the boundary.
	if n := maxAltSeq - 1; len(data) > n {
		r.altCarry = append(r.altCarry[:0], data[len(data)-n:]...)
	} else {
		r.altCarry = append(r.altCarry[:0], data...)
	}
}

// resetAlt clears the alternate-screen tracking at the start of a session, so each
// attachment decides altScreenExit from its own output rather than a prior one's.
func (r *relay) resetAlt() {
	r.alt.Store(false)
	r.altCarry = r.altCarry[:0]
}

// inputLoop reads local input for the relay's whole lifetime and forwards it to
// the current session connection, which swaps as the relay switches sessions. On
// the menu key it forwards any bytes before the key, flags the menu, signals end,
// and stops; it also stops (signalling end, without the flag) when input closes.
// Running it once — rather than per session — keeps a single reader on the
// terminal so switching never leaves a second reader stealing keystrokes. It waits
// for the first connection before reading, so early keystrokes aren't dropped
// before the relay has attached.
func (r *relay) inputLoop(in io.Reader, ended <-chan struct{}, onEnd func()) {
	select {
	case <-r.ready:
	case <-ended:
		return // relay ended before any session attached
	}

	buf := make([]byte, 4096)

	for {
		n, err := in.Read(buf)
		if n > 0 {
			data := buf[:n]
			if i := bytes.IndexByte(data, r.menuKey); i >= 0 {
				if i > 0 {
					if c := r.curConn(); c != nil {
						_ = c.Write(proto.MsgInput, data[:i])
					}
				}

				r.menu.Store(true) // the menu key ended input, not a closed pipe
				onEnd()

				return
			}

			if c := r.curConn(); c != nil {
				_ = c.Write(proto.MsgInput, data)
			}
		}

		if err != nil {
			onEnd()

			return
		}
	}
}

func terminalSize() (int, int) {
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 && h > 0 {
		return w, h
	}

	return 80, 24
}
