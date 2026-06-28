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

// Run connects to the session at addr, switches the terminal to raw mode, and
// proxies I/O until the user detaches or the session exits.
func Run(addr string, opt Options) error {
	in, closeIn := openInput()
	defer closeIn()

	return runIO(addr, opt, in, os.Stdout, int(in.Fd()), true)
}

// runIO is Run with explicit I/O endpoints so the relay can be tested without a
// real terminal. When raw is true and inFd is a terminal, it switches to raw mode.
func runIO(addr string, opt Options, in io.Reader, out io.Writer, inFd int, raw bool) error {
	if opt.DetachKey == 0 {
		opt.DetachKey = DefaultDetachKey
	}

	nc, err := proto.Dial(addr)
	if err != nil {
		return err
	}

	defer func() { _ = nc.Close() }()

	c := proto.NewConn(nc)

	cols, rows := terminalSize()

	att := proto.Attach{Hist: opt.Hist, Lines: opt.Lines, Cols: uint16(cols), Rows: uint16(rows)}
	if err := c.Write(proto.MsgAttach, att.Encode()); err != nil {
		return err
	}

	if raw && term.IsTerminal(inFd) {
		old, err := term.MakeRaw(inFd)
		if err != nil {
			return err
		}

		defer func() { _ = term.Restore(inFd, old) }()
	}

	stopResize := watchResize(c)
	defer stopResize()

	done := make(chan struct{})

	var once sync.Once

	finish := func() { once.Do(func() { close(done) }) }

	go func() { defer finish(); pumpOutput(c, out) }()
	go func() { defer finish(); pumpInput(c, in, opt.DetachKey) }()

	<-done

	return nil
}

// pumpOutput forwards daemon output to out until the connection closes or the
// session exits.
func pumpOutput(c *proto.Conn, out io.Writer) {
	for {
		mt, payload, err := c.Read()
		if err != nil {
			return
		}

		switch mt {
		case proto.MsgOutput:
			_, _ = out.Write(payload)
		case proto.MsgExit:
			return
		}
	}
}

// pumpInput forwards local input to the daemon until a read error, swallowing
// the detach key (and sending MsgDetach) instead of forwarding it.
func pumpInput(c *proto.Conn, in io.Reader, detachKey byte) {
	buf := make([]byte, 4096)

	for {
		n, err := in.Read(buf)
		if n > 0 {
			data := buf[:n]
			if i := bytes.IndexByte(data, detachKey); i >= 0 {
				if i > 0 {
					_ = c.Write(proto.MsgInput, data[:i])
				}

				_ = c.Write(proto.MsgDetach, nil)

				return
			}

			if werr := c.Write(proto.MsgInput, data); werr != nil {
				return
			}
		}

		if err != nil {
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
