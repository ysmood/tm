// Package attach implements the relay: a minimal raw-passthrough client that
// connects to a session daemon's socket, proxies the local terminal's I/O to
// the session, and returns when the user presses the menu key, the session
// exits, or local input ends. It is run as the hidden `tm __attach` subcommand.
package attach

import (
	"bytes"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/proto"
	"golang.org/x/term"
)

// DefaultMenuKey is Ctrl-\ (0x1c): pressing it stops proxying and returns to tm
// so it can open the interactive menu (switch sessions, start a new one, or leave
// tm for the shell). The session keeps running in the background while the menu is
// up. Run reports the press to its caller, which reopens the menu and re-attaches.
const DefaultMenuKey = 0x1c

// TerminalRestore returns the local terminal to a sane baseline when leaving a
// session. A session's PTY has its own terminal state, independent of the outer
// terminal's: a full-screen program run inside it (vim, less, htop, a git pager,
// any TUI) switches on the alternate screen buffer, mouse reporting, a scroll
// region, bracketed paste, etc. Those modes are forwarded to the outer terminal
// live, and detaching abandons the session mid-state, so without undoing them
// the outer terminal is left, e.g., stuck in the alternate screen buffer — where
// the scroll wheel finds no scrollback — or with mouse reporting on, where the
// wheel emits escape codes instead of scrolling. Multiplexers (tmux, screen)
// reset the terminal on detach for exactly this reason.
//
// It deliberately stops short of a full RIS reset (ESC c), which would also wipe
// the terminal's scrollback — the very thing the user wants back.
var TerminalRestore = []byte(
	"\x1b[?1049l" + // leave the alternate screen buffer (restore main + scrollback)
		"\x1b[?1000l\x1b[?1002l\x1b[?1003l" + // disable mouse click / button / any-motion tracking
		"\x1b[?1005l\x1b[?1006l\x1b[?1015l" + // disable UTF-8 / SGR / urxvt mouse encodings
		"\x1b[?1004l" + // disable focus reporting
		"\x1b[?2004l" + // disable bracketed paste
		"\x1b[?7h" + // re-enable auto-wrap (DECAWM)
		"\x1b[?1l\x1b>" + // normal cursor keys (DECCKM) and keypad (DECKPNM)
		"\x1b[r" + // reset the scroll region to the full screen (DECSTBM)
		"\x1b(B" + // select US-ASCII for the G0 charset
		"\x1b[?25h" + // show the cursor
		"\x1b[m") // reset SGR colors / attributes

// Options configures an attach session.
type Options struct {
	Hist    proto.HistMode
	Lines   uint32
	MenuKey byte
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
func Run(p config.Paths, id string, opt Options) (Outcome, error) {
	in, closeIn := openInput()
	defer closeIn()

	return runRelay(opt, in, os.Stdout, int(in.Fd()), true,
		func(sid string) string { return proto.SockAddr(p, sid) }, id)
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
) (outcome Outcome, err error) {
	if opt.MenuKey == 0 {
		opt.MenuKey = DefaultMenuKey
	}

	if raw && term.IsTerminal(inFd) {
		old, merr := term.MakeRaw(inFd)
		if merr != nil {
			return OutcomeInputEnded, merr
		}

		// Hand the terminal back as we found it — but only when we are truly done
		// with this screen: leaving tm, or stopping because the session ended (then
		// app.Run draws the top-level menu over a clean screen). When we stop just so
		// app.Run can open its inline menu (OutcomeMenu), skip the reset: the menu
		// draws beneath the session's screen, like running `tm` inside it, so wiping
		// it (a bare \e[?1049l drops the scrollback on some terminals) is exactly what
		// we must not do. app.Run resets the terminal itself if it then switches
		// sessions or detaches.
		defer func() {
			if outcome != OutcomeMenu {
				_, _ = out.Write(TerminalRestore)
			}

			_ = term.Restore(inFd, old)
		}()
	}

	r := &relay{menuKey: opt.MenuKey, ready: make(chan struct{})}

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
			return OutcomeInputEnded, serr
		}

		if next == nil {
			// No switch: the menu key (OutcomeMenu), the session exiting
			// (OutcomeSessionExited), or local input ending (OutcomeInputEnded).
			// app.Run reads the outcome to decide whether to re-open the menu over the
			// session, fall back to the top-level menu, or leave tm.
			return oc, nil
		}

		// Switching to another session. The session we are leaving may have left
		// the outer terminal in the alternate screen buffer, with mouse reporting
		// on, or the cursor hidden — e.g. a full-screen app, or tm's own menu
		// (which runs in the alternate screen), was running in it. Unlike a detach,
		// nothing else resets the terminal here: the menu's teardown went to the
		// old session's PTY, which we have already left, so it never reaches this
		// terminal. Reset to baseline before re-attaching, so the next session's
		// output (and history replay) lands on a clean screen with a visible cursor
		// instead of inheriting the previous session's leftover modes.
		_, _ = out.Write(TerminalRestore)

		id, opt.Hist, opt.Lines = next.ID, next.Hist, next.Lines
	}
}

// session runs one attachment: it dials addr, attaches, and proxies output until
// input ends (the menu key or the local input closing), the session exits, or the
// session asks the relay to switch. It returns a non-nil SwitchTarget only for a
// switch; otherwise the Outcome says why it stopped (the menu key, the session
// exiting, or local input ending).
func (r *relay) session(
	addr string, opt Options, out io.Writer, ended <-chan struct{},
) (*proto.SwitchTarget, Outcome, error) {
	nc, err := proto.Dial(addr)
	if err != nil {
		return nil, OutcomeInputEnded, err
	}

	defer func() { _ = nc.Close() }()

	c := proto.NewConn(nc)

	cols, rows := terminalSize()

	att := proto.Attach{Hist: opt.Hist, Lines: opt.Lines, Cols: uint16(cols), Rows: uint16(rows)}
	if err := c.Write(proto.MsgAttach, att.Encode()); err != nil {
		return nil, OutcomeInputEnded, err
	}

	r.setConn(c)

	stopResize := watchResize(c)
	defer stopResize()

	target := make(chan *proto.SwitchTarget, 1)
	go func() { target <- pumpOutput(c, out) }()

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
		// non-nil: switch to another session (the Outcome is unused by the caller).
		// nil: the session's shell exited or its daemon dropped the relay — the
		// session is gone, so app.Run falls back to the top-level menu.
		if t != nil {
			return t, OutcomeInputEnded, nil
		}

		return nil, OutcomeSessionExited, nil
	}
}

// pumpOutput forwards daemon output to out until the connection closes or the
// session exits. It returns a non-nil SwitchTarget if the daemon asked the relay
// to switch to another session, or nil on connection close or session exit.
func pumpOutput(c *proto.Conn, out io.Writer) *proto.SwitchTarget {
	for {
		mt, payload, err := c.Read()
		if err != nil {
			return nil
		}

		switch mt {
		case proto.MsgOutput:
			_, _ = out.Write(payload)
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
