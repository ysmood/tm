// Package attach implements the relay: a minimal raw-passthrough client that
// connects to a session daemon's socket, proxies the local terminal's I/O to
// the session, and returns when the user presses the detach key or the session
// exits. It is run as the hidden `tm __attach` subcommand.
package attach

import (
	"bytes"
	"io"
	"os"
	"sync"

	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/proto"
	"golang.org/x/term"
)

// DefaultDetachKey is Ctrl-\ (0x1c): pressing it detaches from the session,
// which keeps running in the background, and returns to tm (which then exits to
// the launching shell).
const DefaultDetachKey = 0x1c

// Options configures an attach session.
type Options struct {
	Hist      proto.HistMode
	Lines     uint32
	DetachKey byte
}

// Run connects to the session id under p, switches the terminal to raw mode, and
// proxies I/O until the user detaches or the session exits. When the session asks
// the relay to switch (a tm running inside it picked another session), Run leaves
// the current session running and re-attaches to the target — so switching moves
// this one terminal between sessions instead of nesting relays.
func Run(p config.Paths, id string, opt Options) error {
	in, closeIn := openInput()
	defer closeIn()

	return runRelay(opt, in, os.Stdout, int(in.Fd()), true,
		func(sid string) string { return proto.SockAddr(p, sid) }, id)
}

// relay holds the state shared across a relay's session iterations: a single
// input reader forwards keystrokes to whichever session is current, so switching
// sessions never leaves a second reader competing for the terminal's input.
type relay struct {
	detachKey byte

	ready     chan struct{} // closed once the first session connection is set
	readyOnce sync.Once

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
func runRelay(opt Options, in io.Reader, out io.Writer, inFd int, raw bool, addrOf func(string) string, id string) error {
	if opt.DetachKey == 0 {
		opt.DetachKey = DefaultDetachKey
	}

	if raw && term.IsTerminal(inFd) {
		old, err := term.MakeRaw(inFd)
		if err != nil {
			return err
		}

		defer func() { _ = term.Restore(inFd, old) }()
	}

	r := &relay{detachKey: opt.DetachKey, ready: make(chan struct{})}

	detached := make(chan struct{})

	var once sync.Once

	onDetach := func() { once.Do(func() { close(detached) }) }

	// On any exit, signal detach so the input reader unblocks even if it is still
	// waiting for the first connection (e.g. the initial dial failed).
	defer onDetach()

	go r.inputLoop(in, detached, onDetach)

	for {
		next, err := r.session(addrOf(id), opt, out, detached)
		if err != nil {
			return err
		}

		if next == nil {
			return nil // detached or the session exited
		}

		id, opt.Hist, opt.Lines = next.ID, next.Hist, next.Lines
	}
}

// session runs one attachment: it dials addr, attaches, and proxies output until
// the user detaches, the session exits, or the session asks the relay to switch.
// It returns a non-nil SwitchTarget only for a switch; nil means stop the relay.
func (r *relay) session(addr string, opt Options, out io.Writer, detached <-chan struct{}) (*proto.SwitchTarget, error) {
	nc, err := proto.Dial(addr)
	if err != nil {
		return nil, err
	}

	defer func() { _ = nc.Close() }()

	c := proto.NewConn(nc)

	cols, rows := terminalSize()

	att := proto.Attach{Hist: opt.Hist, Lines: opt.Lines, Cols: uint16(cols), Rows: uint16(rows)}
	if err := c.Write(proto.MsgAttach, att.Encode()); err != nil {
		return nil, err
	}

	r.setConn(c)

	stopResize := watchResize(c)
	defer stopResize()

	target := make(chan *proto.SwitchTarget, 1)
	go func() { target <- pumpOutput(c, out) }()

	select {
	case <-detached:
		// The user pressed the detach key: tell the daemon to drop us (the session
		// keeps running) and stop the relay.
		_ = c.Write(proto.MsgDetach, nil)

		return nil, nil
	case t := <-target:
		// nil: the session exited (stop). non-nil: switch to another session.
		return t, nil
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
// the detach key it forwards any bytes before the key, signals detach, and stops;
// it also stops (signalling detach) when input ends. Running it once — rather than
// per session — keeps a single reader on the terminal so switching never leaves a
// second reader stealing keystrokes. It waits for the first connection before
// reading, so early keystrokes aren't dropped before the relay has attached.
func (r *relay) inputLoop(in io.Reader, detached <-chan struct{}, onDetach func()) {
	select {
	case <-r.ready:
	case <-detached:
		return // relay ended before any session attached
	}

	buf := make([]byte, 4096)

	for {
		n, err := in.Read(buf)
		if n > 0 {
			data := buf[:n]
			if i := bytes.IndexByte(data, r.detachKey); i >= 0 {
				if i > 0 {
					if c := r.curConn(); c != nil {
						_ = c.Write(proto.MsgInput, data[:i])
					}
				}

				onDetach()

				return
			}

			if c := r.curConn(); c != nil {
				_ = c.Write(proto.MsgInput, data)
			}
		}

		if err != nil {
			onDetach()

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
