// Package proto defines the wire protocol between an attach client (the relay)
// and a session daemon: length-prefixed, typed frames over a stream connection.
package proto

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// MaxPayload caps a single frame's payload as a safety guard against corrupt
// or malicious length prefixes.
const MaxPayload = 1 << 20 // 1 MiB

// MsgType identifies a frame's kind.
type MsgType byte

const (
	// MsgAttach is the client's first frame: requested history + terminal size.
	MsgAttach MsgType = iota + 1
	// MsgInput carries raw keystrokes from client to daemon.
	MsgInput
	// MsgResize carries a new terminal size from client to daemon.
	MsgResize
	// MsgDetach asks the daemon to drop this client (the session keeps running).
	MsgDetach
	// MsgOutput carries raw terminal output from daemon to client.
	MsgOutput
	// MsgExit reports the shell's exit code; the session is over.
	MsgExit
	// MsgSwitch asks the daemon to hand its attached client (the relay) to another
	// session, so a tm running inside this session can move the terminal there
	// instead of nesting a new relay. The sender does not attach, so the current
	// client is not displaced. Payload is a SwitchTarget.
	MsgSwitch
	// MsgSwitchTo is the daemon forwarding a switch request to its attached client:
	// re-attach to the carried SwitchTarget. Payload is a SwitchTarget.
	MsgSwitchTo
	// MsgKill asks the daemon to end the session: terminate the shell, delete the
	// session's files, and exit. The sender does not attach; the daemon closes the
	// connection once teardown is done, so the sender can block on a read to know
	// the session is gone. No payload.
	MsgKill
	// MsgClear asks the daemon to wipe the session's recorded history: its log file
	// is truncated, so a later attach replays nothing of what came before (say, a
	// secret echoed to the terminal). The session keeps running. The sender does not
	// attach; the daemon closes the connection once the wipe is done, so the sender
	// can block on a read to know it happened. No payload.
	MsgClear
)

// Attach is the payload of a MsgAttach frame. Replay asks the daemon to send the
// last window of recorded output (one screen, sized by Rows) before the live
// stream: what an attach or a switch wants, so the session's screen is there to
// look at. Resuming a session the terminal is still showing (esc out of the menu
// opened over it) clears it, since that screen never went away.
type Attach struct {
	Replay bool
	Cols   uint16
	Rows   uint16
}

// Encode serializes the Attach payload.
func (a Attach) Encode() []byte {
	b := make([]byte, 5)
	if a.Replay {
		b[0] = 1
	}

	binary.BigEndian.PutUint16(b[1:], a.Cols)
	binary.BigEndian.PutUint16(b[3:], a.Rows)

	return b
}

// DecodeAttach parses an Attach payload.
func DecodeAttach(p []byte) (Attach, error) {
	if len(p) < 5 {
		return Attach{}, fmt.Errorf("proto: short attach payload: %d", len(p))
	}

	return Attach{
		Replay: p[0] != 0,
		Cols:   binary.BigEndian.Uint16(p[1:]),
		Rows:   binary.BigEndian.Uint16(p[3:]),
	}, nil
}

// Resize is the payload of a MsgResize frame.
type Resize struct {
	Cols uint16
	Rows uint16
}

// Encode serializes the Resize payload.
func (r Resize) Encode() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:], r.Cols)
	binary.BigEndian.PutUint16(b[2:], r.Rows)

	return b
}

// DecodeResize parses a Resize payload.
func DecodeResize(p []byte) (Resize, error) {
	if len(p) < 4 {
		return Resize{}, fmt.Errorf("proto: short resize payload: %d", len(p))
	}

	return Resize{
		Cols: binary.BigEndian.Uint16(p[0:]),
		Rows: binary.BigEndian.Uint16(p[2:]),
	}, nil
}

// SwitchTarget is the payload of MsgSwitch/MsgSwitchTo: the session to re-attach
// to and its display name (for the relay's status notice). The relay always
// replays the target's last window, so there is nothing to say about history.
type SwitchTarget struct {
	ID   string
	Name string
}

// Encode serializes the SwitchTarget payload: the id length, then the id bytes
// followed by the name bytes. The id is length-prefixed so the name (variable
// length, may be empty) can trail it.
func (s SwitchTarget) Encode() []byte {
	b := make([]byte, 2+len(s.ID)+len(s.Name))
	binary.BigEndian.PutUint16(b, uint16(len(s.ID)))
	n := copy(b[2:], s.ID)
	copy(b[2+n:], s.Name)

	return b
}

// DecodeSwitchTarget parses a SwitchTarget payload.
func DecodeSwitchTarget(p []byte) (SwitchTarget, error) {
	if len(p) < 2 {
		return SwitchTarget{}, fmt.Errorf("proto: short switch payload: %d", len(p))
	}

	idLen := int(binary.BigEndian.Uint16(p))
	if len(p) < 2+idLen {
		return SwitchTarget{}, fmt.Errorf("proto: short switch payload: %d", len(p))
	}

	return SwitchTarget{
		ID:   string(p[2 : 2+idLen]),
		Name: string(p[2+idLen:]),
	}, nil
}

// EncodeExit serializes an exit code for a MsgExit frame.
func EncodeExit(code int32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(code))

	return b
}

// DecodeExit parses a MsgExit payload.
func DecodeExit(p []byte) (int32, error) {
	if len(p) < 4 {
		return 0, fmt.Errorf("proto: short exit payload: %d", len(p))
	}

	return int32(binary.BigEndian.Uint32(p)), nil
}

// WriteFrame writes one length-prefixed frame to w.
func WriteFrame(w io.Writer, t MsgType, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("proto: payload too large: %d", len(payload))
	}

	var hdr [5]byte

	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))

	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}

	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}

	return nil
}

// ReadFrame reads one length-prefixed frame from r.
func ReadFrame(r io.Reader) (MsgType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}

	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("proto: payload too large: %d", n)
	}

	if n == 0 {
		return MsgType(hdr[0]), nil, nil
	}

	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}

	return MsgType(hdr[0]), payload, nil
}

// Conn is a framed connection. Writes are serialized so multiple goroutines may
// send frames concurrently; Read is expected to be driven by a single goroutine.
type Conn struct {
	rw io.ReadWriteCloser
	mu sync.Mutex
}

// NewConn wraps a stream connection.
func NewConn(rw io.ReadWriteCloser) *Conn { return &Conn{rw: rw} }

// Write sends one frame.
func (c *Conn) Write(t MsgType, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return WriteFrame(c.rw, t, payload)
}

// Read receives one frame.
func (c *Conn) Read() (MsgType, []byte, error) { return ReadFrame(c.rw) }

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.rw.Close() }
