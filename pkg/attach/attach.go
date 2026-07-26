// Package attach implements the relay: a minimal raw-passthrough client that
// connects to a session daemon's socket, proxies the local terminal's I/O to
// the session, and returns when the user presses the menu key, the session
// exits, or local input ends. It is run as the hidden `tm __attach` subcommand.
package attach

import (
	"bytes"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/muesli/cancelreader"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/proto"
	"golang.org/x/term"
)

// menuKey is Ctrl-\ (0x1c): pressing it stops proxying and returns to tm so it
// can open the interactive menu (switch sessions, start a new one, or leave tm for
// the shell). The session keeps running in the background while the menu is up.
// Run reports the press to its caller, which reopens the menu and re-attaches.
//
// It is fixed rather than configurable: the key is part of tm's interface, named
// in the menu's own help and in the README, and one relay's idea of it must match
// what every other part of tm tells the user to press.
const menuKey = 0x1c

// menuKeySeqs are every encoding the terminal may use for Ctrl-\. A terminal
// sends the raw 0x1c byte only in the legacy encoding: once a program in the
// session switches the terminal to the kitty keyboard protocol ("\x1b[=1u" —
// lazygit and other tcell v3 apps do, when the terminal supports it) or to
// xterm's modifyOtherKeys, Ctrl-\ arrives as an escape sequence instead. The
// relay must recognize those too, or the menu key goes dead exactly while such
// an app is on screen. 92 is '\', modifier 5 is Ctrl (4) + 1.
var menuKeySeqs = [][]byte{
	{menuKey},
	[]byte("\x1b[92;5u"),    // kitty protocol / modifyOtherKeys CSI-u format
	[]byte("\x1b[92;5:1u"),  // kitty with event-type reporting: press
	[]byte("\x1b[92;5:2u"),  // kitty with event-type reporting: repeat
	[]byte("\x1b[27;5;92~"), // xterm modifyOtherKeys=2 legacy format
}

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
		"\x1b[=0;1u" + // kitty keyboard protocol flags cleared (keys back to legacy bytes)
		"\x1b[>4m" + // xterm modifyOtherKeys off
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

// modesReset is the scrollback-preserving reset: it undoes a session's terminal
// modes without leaving the alternate screen, so detaching or exiting at a shell
// prompt keeps the terminal's scrollback intact.
var modesReset = []byte(terminalModesReset)

// fullRestore is the reset for when the session left the terminal in the alternate
// screen: leave the alt screen (restoring the main screen + scrollback), then reset
// the modes. Used where the screen is redrawn next (a session switch) or where a
// full-screen app was up when leaving.
//
// Both are reached through RestoreFor, which is what callers outside this package
// use — picking between them is this package's job, not theirs.
var fullRestore = []byte(altScreenExit + terminalModesReset)

// SwitchReset is fullRestore plus a carriage return, written before re-attaching
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
		return fullRestore
	}

	return modesReset
}

// Options configures an attach session.
type Options struct {
	// Replay asks the daemon to redraw the session's last window of output before
	// the live stream. It is what an attach or a switch wants; resuming a session
	// the terminal is still showing leaves it off.
	Replay bool
	// InAltScreen seeds the relay's alternate-screen tracking with what the
	// caller already knows: a resume (Replay off) redraws nothing, so a session
	// sitting quietly inside a full-screen app would otherwise read as
	// main-screen and a later detach would skip the alt-screen exit, leaving
	// the terminal stuck in the app's dead screen. A replaying attach ignores
	// it — the replayed window re-establishes the state itself.
	InAltScreen bool
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
func Run(p config.Paths, id string, opt Options) (Outcome, bool, error) {
	in, closeIn := openInput()
	defer closeIn()

	rd, cancel := cancelInput(in)
	defer cancel()

	return runRelay(opt, rd, os.Stdout, int(in.Fd()), true,
		func(sid string) string { return proto.SockAddr(p, sid) }, id)
}

// cancelInput wraps the relay's input so its pending read can be canceled once
// the relay stops. The input reader blocks in a read between keystrokes, and a
// relay can end without any input arriving — the session's shell exiting (or
// being killed) — which would leave that read pending. macOS cannot poll
// /dev/tty, so closing the file does not unblock it either; the leftover reader
// would then steal keystrokes from the menu that opens next and forward them to
// the dead session. cancelreader falls back to select(2) there, which cancels
// cleanly and never holds a consuming read while idle. If wrapping fails, the
// plain file is used and cancellation is a no-op.
func cancelInput(f *os.File) (io.Reader, func()) {
	cr, err := cancelreader.NewReader(f)
	if err != nil {
		return f, func() {}
	}

	return cr, func() { _ = cr.Cancel() }
}

// relay holds the state shared across a relay's session iterations: a single
// input reader forwards keystrokes to whichever session is current, so switching
// sessions never leaves a second reader competing for the terminal's input.
type relay struct {
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
	// altScan splices altCarry onto the current chunk so a toggle straddling the
	// boundary is scanned as one run. It is its own buffer rather than an append
	// onto altCarry, so the scanned bytes never share altCarry's array with the
	// carry it is about to be refilled from, and it is reused across chunks so the
	// stitch costs no allocation in steady state.
	altScan []byte

	mu   sync.Mutex
	conn *proto.Conn // the session connection input is currently forwarded to
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
// returns when the user detaches or a session exits.
func runRelay(
	opt Options, in io.Reader, out io.Writer, inFd int, raw bool,
	addrOf func(string) string, id string,
) (outcome Outcome, leftAlt bool, err error) {
	r := &relay{ready: make(chan struct{})}
	r.alt.Store(opt.InAltScreen) // a replaying attach resets this in dialSession

	// curName tracks the session the relay is currently on (updated as it switches
	// internally), so the exit notice below names the right session.
	curName := opt.Name

	if raw && term.IsTerminal(inFd) {
		restore, merr := rawTerminal(inFd)
		if merr != nil {
			return OutcomeInputEnded, false, merr
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

	for {
		next, oc, serr := r.session(addrOf(id), opt, out, ended)
		if serr != nil {
			return OutcomeInputEnded, r.alt.Load(), serr
		}

		if next == nil {
			// No switch: the menu key (OutcomeMenu), the session exiting
			// (OutcomeSessionExited), or local input ending (OutcomeInputEnded).
			// app.Run reads the outcome to decide whether to re-open the menu over the
			// session, fall back to the top-level menu, or leave tm. leftAlt lets it
			// preserve scrollback when the session was not on the alt screen.
			return oc, r.alt.Load(), nil
		}

		announceSwitch(out, next)

		// The target's window is redrawn: switching moves the terminal to a session
		// whose screen is not the one currently up.
		id, opt.Replay, curName = next.ID, true, next.Name
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
			_, _ = out.Write(exitedSessionNotice(curName))
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
// window replays over it — skipped when the target carries no name (e.g. an
// older sender), so the notice never renders an empty name.
func announceSwitch(out io.Writer, next *proto.SwitchTarget) {
	_, _ = out.Write(SwitchReset)

	if next.Name != "" {
		_, _ = out.Write(SwitchedNotice(next.Name))
	}
}

// session runs one attachment: it dials addr, attaches, and proxies output until
// input ends (the menu key or the local input closing), the session exits, or the
// session asks the relay to switch. It returns a non-nil SwitchTarget only for a
// switch; otherwise the Outcome says why it stopped.
func (r *relay) session(
	addr string, opt Options, out io.Writer, ended <-chan struct{},
) (*proto.SwitchTarget, Outcome, error) {
	nc, c, err := r.dialSession(addr, opt)
	if err != nil {
		return nil, OutcomeInputEnded, err
	}

	defer func() { _ = nc.Close() }()

	r.setConn(c)

	stopResize := watchResize(c)
	defer stopResize()

	target := make(chan *proto.SwitchTarget, 1)

	go func() { target <- r.pumpOutput(c, out) }()

	select {
	case <-ended:
		// Input ended: either the user pressed the menu key (r.menu set) or the
		// local input closed. Either way tell the daemon to drop us — the session
		// keeps running — and stop this attachment. The Outcome tells the caller
		// whether to open the menu or leave tm.
		_ = c.Write(proto.MsgDetach, nil)

		if r.menu.Load() {
			return nil, OutcomeMenu, nil
		}

		return nil, OutcomeInputEnded, nil
	case t := <-target:
		// A target means switch to another session (the Outcome is unused by the
		// caller). Otherwise the session's shell exited or its daemon dropped the
		// relay — the session is gone, so app.Run falls back to the top-level menu.
		if t != nil {
			return t, OutcomeInputEnded, nil
		}

		return nil, OutcomeSessionExited, nil
	}
}

// dialSession dials addr and sends the attach request, returning the framed
// connection. On a replaying attach it also resets the alternate-screen
// tracking, so the attachment decides it from its own output (the replayed
// window re-establishes it if the session is mid-full-screen-app). A resume
// (no replay) keeps the tracked state instead: the session's screen never
// went away and replays nothing, so resetting would forget it is on the alt
// screen and a later detach would skip the alt-screen exit — leaving the
// terminal stuck inside the dead app's screen.
func (r *relay) dialSession(addr string, opt Options) (net.Conn, *proto.Conn, error) {
	nc, err := proto.Dial(addr)
	if err != nil {
		return nil, nil, err
	}

	c := proto.NewConn(nc)

	cols, rows := terminalSize()

	att := proto.Attach{Replay: opt.Replay, Cols: uint16(cols), Rows: uint16(rows)}
	if aerr := c.Write(proto.MsgAttach, att.Encode()); aerr != nil {
		_ = nc.Close()

		return nil, nil, aerr
	}

	if opt.Replay {
		r.resetAlt()
	}

	return nc, c, nil
}

// pumpOutput forwards daemon output to out until the connection closes, the
// session exits, or the daemon asks the relay to switch — which it reports by
// returning the target (nil otherwise). While forwarding it tracks the
// alternate-screen state, so leaving the session knows whether sending
// altScreenExit is needed.
func (r *relay) pumpOutput(c *proto.Conn, out io.Writer) *proto.SwitchTarget {
	for {
		mt, payload, err := c.Read()
		if err != nil {
			return nil // the connection closed or the session exited
		}

		switch mt {
		case proto.MsgOutput:
			_, _ = out.Write(payload)
			r.trackAlt(payload)
		case proto.MsgSwitchTo:
			if t, derr := proto.DecodeSwitchTarget(payload); derr == nil {
				return &t
			}

			return nil
		case proto.MsgExit:
			return nil
		}
	}
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
// previous chunk's tail onto this one (via altScan) so a toggle split across the
// boundary is still seen. resetAlt clears the tracking at the start of each session.
func (r *relay) trackAlt(payload []byte) {
	data := payload

	if len(r.altCarry) > 0 {
		r.altScan = append(append(r.altScan[:0], r.altCarry...), payload...)
		data = r.altScan
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
// the menu key it forwards any bytes before the key, flags it, signals end, and
// stops; it also stops (signalling end, with no flag) when input closes. Running
// it once — rather than per session — keeps a
// single reader on the terminal so switching never leaves a second reader
// stealing keystrokes. It waits for the first connection before reading, so
// early keystrokes aren't dropped before the relay has attached.
func (r *relay) inputLoop(in io.Reader, ended <-chan struct{}, onEnd func()) {
	select {
	case <-r.ready:
	case <-ended:
		return // relay ended before any session attached
	}

	buf := make([]byte, 4096)

	for {
		n, err := in.Read(buf)
		if n > 0 && r.forwardInput(buf[:n]) {
			onEnd()

			return
		}

		if err != nil {
			onEnd()

			return
		}
	}
}

// forwardInput handles one chunk of local input: it forwards it to the current
// session connection, stopping at the menu key in any of its encodings (see
// menuKeySeqs) — the bytes before the key are still forwarded, and the menu
// flag is set before the caller signals end, so the ended-channel reader sees
// it. It reports whether the input loop should stop.
func (r *relay) forwardInput(data []byte) (stop bool) {
	i := findMenuKey(data)
	if i < 0 {
		r.sendInput(data)

		return false
	}

	if i > 0 {
		r.sendInput(data[:i])
	}

	r.menu.Store(true)

	return true
}

// findMenuKey returns the index of the earliest menu-key encoding in data, or
// -1 if none is present. A key press arrives as one whole read from the
// terminal, so an encoding is never split across chunks.
func findMenuKey(data []byte) int {
	idx := -1

	for _, seq := range menuKeySeqs {
		if i := bytes.Index(data, seq); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}

	return idx
}

// sendInput forwards local input bytes to the current session connection,
// dropping them when no session is attached yet.
func (r *relay) sendInput(data []byte) {
	if c := r.curConn(); c != nil {
		_ = c.Write(proto.MsgInput, data)
	}
}

func terminalSize() (int, int) {
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 && h > 0 {
		return w, h
	}

	return 80, 24
}
