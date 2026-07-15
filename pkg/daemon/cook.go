package daemon

import (
	"bytes"
	"unicode/utf8"
)

const (
	esc = 0x1b
	bel = 0x07

	// maxLineWidth bounds the in-progress line's cells. Output with no newline
	// (a huge single line, binary data) would otherwise grow the buffer without
	// limit, since the line is only flushed on a newline. Reaching the cap
	// soft-wraps the line — flushes it and continues on a fresh one — so memory
	// stays bounded. It is far wider than any real terminal, so carriage-return
	// redraws (prompts, progress bars) never reach it and their overwrites are
	// unaffected.
	maxLineWidth = 1 << 13 // 8192 columns

	// maxHeldBytes bounds the partial sequence held across chunks. A sequence
	// with no terminator (an unterminated OSC/DCS in a malformed or binary
	// stream) would otherwise grow held without limit. Past this — far longer
	// than any sequence the cooker keeps, all of which are short — the held bytes
	// are dropped and scanning resyncs on the next chunk.
	maxHeldBytes = 1 << 16 // 64 KiB
)

// sgrReset ends the styling emitted for a rendered line, so each recorded line
// carries its own complete style and colors never bleed between lines in a pager.
var sgrReset = []byte("\x1b[0m")

// blankCell is a single space with no style, used to pad a line out to a column
// the cursor jumped past. Copied by value into the cell slice, so the shared
// value is never mutated.
var blankCell = cell{n: 1, text: [utf8.UTFMax]byte{' '}}

// cooker turns the raw PTY byte stream into the visible history that gets
// recorded to the log: printable text, tabs, newlines, and SGR (color and text
// attribute) escapes. Everything else — cursor motion, screen clears,
// reply-soliciting queries, bells, other control bytes — is dropped, and
// carriage-return and backspace overwrites are applied, so a redrawn line (a
// shell prompt, a progress bar, zsh's end-of-line "%" marker) is recorded as its
// final on-screen form rather than as literal ^M and stray characters. The log
// then reads cleanly in a pager (`[history]` runs `less -R` on it) instead of
// showing the raw control soup a terminal acts on but a pager prints verbatim.
//
// It models one logical line as a grid of cells addressed by column, so
// within-line overwrites resolve to what each column ends up showing. It is not
// a full terminal: absolute cursor addressing (a full-screen TUI that paints the
// whole screen) is not reconstructed — such output flattens rather than
// reproducing the screen. That is the deliberate trade for a history *log* of a
// shell session.
//
// Cells own their bytes and the returned line buffer is reused, so cook does not
// copy or allocate per chunk in steady state: the read buffer is scanned in
// place. All the fields below outlive a single call and are reused across them.
type cooker struct {
	cells  []cell // the in-progress (not yet newline-terminated) line, by column
	col    int    // the cursor's column within the line
	style  []byte // SGR sequence(s) active for new cells (cooker-owned; nil = default)
	held   []byte // a sequence or rune split across calls, kept for the next chunk
	joined []byte // scratch to splice held + next chunk when held is non-empty
	out    []byte // completed lines returned by cook; valid until the next cook call
}

// cell is one column of the current line: the character's own bytes (copied in,
// so nothing aliases the read buffer) and the SGR style active when it was
// written (a cooker-owned, immutable snapshot shared by reference).
type cell struct {
	style []byte
	text  [utf8.UTFMax]byte
	n     uint8
}

// isSpace reports whether the cell is a lone unstyled-or-styled space, which
// renderInto trims from a line's end.
func (c cell) isSpace() bool { return c.n == 1 && c.text[0] == ' ' }

// cook consumes a chunk of raw PTY output and returns the finished lines to
// append to the log — every line this chunk terminated with a newline, rendered
// to its visible form. The in-progress line stays buffered in the cooker (see
// tail), so nothing is written until a newline settles it and a later overwrite
// can still revise it. The returned slice is owned by the cooker and stays valid
// only until the next cook or reset call.
func (c *cooker) cook(p []byte) []byte {
	p = c.spliceHeld(p)
	c.out = c.out[:0]

	for i := 0; i < len(p); {
		b := p[i]

		switch {
		case b == esc:
			n, sgr, complete := scanEscape(p[i:])
			if !complete {
				c.hold(p[i:])

				return c.out
			}

			if sgr {
				c.setStyle(p[i : i+n])
			}

			i += n
		case b == '\n':
			c.flushLine()

			i++
		case b == '\r':
			c.col = 0
			i++
		case b == '\b':
			if c.col > 0 {
				c.col--
			}

			i++
		case b == '\t':
			c.put(p[i : i+1])

			i++
		case b >= 0x20 && b != 0x7f:
			r, size := decodeRune(p[i:])
			if size == 0 {
				c.hold(p[i:]) // truncated UTF-8; await the rest

				return c.out
			}

			c.put(r)

			i += size
		default:
			i++ // drop other C0 controls: bell, form feed, DEL, ...
		}
	}

	return c.out
}

// hold keeps a partial sequence or rune for the next chunk to complete. Past
// maxHeldBytes it is instead dropped: nothing the cooker keeps is that long, so
// this is an unterminated sequence in a malformed stream, and holding it would
// grow the buffer without bound. Dropping resyncs scanning on the next chunk.
func (c *cooker) hold(rest []byte) {
	if len(rest) > maxHeldBytes {
		c.held = c.held[:0]

		return
	}

	c.held = append(c.held[:0], rest...)
}

// flushLine renders the in-progress line, terminates it with a newline, and
// clears it for the next one. The active style carries over, so a color that
// spans the break stays in effect.
func (c *cooker) flushLine() {
	c.out = append(c.renderInto(c.out), '\n')
	c.cells = c.cells[:0]
	c.col = 0
}

// spliceHeld prepends any bytes held from the previous chunk — a sequence or
// rune that straddled the boundary — so p scans as a whole. In the common case
// nothing is held and p is returned untouched (no copy); otherwise the splice
// reuses two buffers, so it costs no allocation in steady state.
func (c *cooker) spliceHeld(p []byte) []byte {
	if len(c.held) == 0 {
		return p
	}

	c.joined = append(append(c.joined[:0], c.held...), p...)
	c.held = c.held[:0]

	return c.joined
}

// tail returns the in-progress line's visible bytes as a fresh slice, so a
// replay can show the live prompt that has not yet been committed with a newline
// without disturbing cook's reused output buffer.
func (c *cooker) tail() []byte {
	return c.renderInto(nil)
}

// reset discards all buffered state, so a cleared session starts from a clean
// slate with no half-built line or partial sequence carried across the wipe.
func (c *cooker) reset() {
	*c = cooker{}
}

// put writes one character's bytes at the cursor column with the active style,
// padding with blank cells if the cursor sits past the line's end, then advances
// the cursor. The bytes are copied into the cell, so nothing aliases the caller's
// read buffer.
func (c *cooker) put(text []byte) {
	if c.col >= maxLineWidth {
		c.flushLine() // no newline in sight; soft-wrap so the buffer stays bounded
	}

	for len(c.cells) < c.col {
		c.cells = append(c.cells, blankCell)
	}

	cl := cell{style: c.style, n: uint8(len(text))}
	copy(cl.text[:], text)

	if c.col < len(c.cells) {
		c.cells[c.col] = cl
	} else {
		c.cells = append(c.cells, cl)
	}

	c.col++
}

// renderInto appends the current line's visible bytes to dst and returns it: the
// text, with each cell's style emitted when it changes (reset first, so a run's
// style never carries over from the previous one), and a closing reset if the
// line ended styled. Trailing spaces are dropped — a terminal pads a redrawn line
// out to its width, but a log needs no padding.
func (c *cooker) renderInto(dst []byte) []byte {
	end := len(c.cells)
	for end > 0 && c.cells[end-1].isSpace() {
		end--
	}

	var cur []byte

	for i := range end {
		cl := &c.cells[i]
		if !bytes.Equal(cl.style, cur) {
			if len(cur) > 0 {
				dst = append(dst, sgrReset...)
			}

			dst = append(dst, cl.style...)
			cur = cl.style
		}

		dst = append(dst, cl.text[:cl.n]...)
	}

	if len(cur) > 0 {
		dst = append(dst, sgrReset...)
	}

	return dst
}

// setStyle updates the active style from an SGR sequence. A reset (a 0 or empty
// parameter) clears the accumulated style; other attributes accumulate, in the
// order they arrived, so re-emitting them reproduces the same final appearance.
// Each update allocates a fresh slice, so the style already stored in written
// cells is never mutated underneath them.
func (c *cooker) setStyle(seq []byte) {
	params := seq[2 : len(seq)-1] // between the "\x1b[" and the final 'm'

	switch {
	case sgrPureReset(params):
		c.style = nil
	case sgrHasReset(params):
		c.style = bytes.Clone(seq) // a reset mixed with new attributes
	default:
		style := make([]byte, 0, len(c.style)+len(seq))
		style = append(style, c.style...)
		style = append(style, seq...)
		c.style = style
	}
}

// sgrPureReset reports whether an SGR parameter list only resets — every
// parameter is empty or "0" (e.g. "\x1b[0m", "\x1b[m", "\x1b[0;0m").
func sgrPureReset(params []byte) bool {
	for f := range bytes.SplitSeq(params, []byte{';'}) {
		if !isZeroParam(f) {
			return false
		}
	}

	return true
}

// sgrHasReset reports whether any parameter resets, so the style accumulated
// before it should be dropped (e.g. "\x1b[0;1;31m" resets, then sets bold red).
func sgrHasReset(params []byte) bool {
	for f := range bytes.SplitSeq(params, []byte{';'}) {
		if isZeroParam(f) {
			return true
		}
	}

	return false
}

func isZeroParam(f []byte) bool {
	return len(f) == 0 || string(f) == "0"
}

// decodeRune returns the bytes of the rune at the start of p and its size. A
// size of 0 means p ends mid-rune, so the caller should wait for more input. An
// invalid byte is returned as-is (size 1) so a bad stream cannot stall.
func decodeRune(p []byte) ([]byte, int) {
	if p[0] < 0x80 {
		return p[:1], 1 // ASCII fast path
	}

	if !utf8.FullRune(p) {
		return nil, 0 // a multi-byte rune truncated at the buffer's end
	}

	_, size := utf8.DecodeRune(p)

	return p[:size], size
}

// scanEscape parses one ESC-introduced sequence at the start of p (p[0]==esc),
// reporting its length, whether it is an SGR set to keep, and whether it is
// complete (an incomplete tail reports complete=false so the caller can wait).
func scanEscape(p []byte) (n int, sgr, complete bool) {
	if len(p) < 2 {
		return 0, false, false
	}

	switch p[1] {
	case '[':
		return scanCSI(p)
	case ']':
		return scanString(p) // OSC
	case 'P', 'X', '^', '_':
		return scanString(p) // DCS / SOS / PM / APC — all string-terminated
	default:
		// A two- or three-byte escape (ESC 7, ESC =, ESC ( B, ...): consume its
		// intermediate bytes and one final byte. None are SGR, so all are dropped.
		i := 1
		for i < len(p) && p[i] >= 0x20 && p[i] <= 0x2f {
			i++
		}

		if i >= len(p) {
			return 0, false, false
		}

		return i + 1, false, true
	}
}

// scanCSI parses a CSI sequence (ESC [), reporting whether it is an SGR set —
// final 'm' with no private marker or intermediate bytes — which is the only CSI
// the log keeps.
func scanCSI(p []byte) (n int, sgr, complete bool) {
	i := 2 // past ESC [

	private := false
	if i < len(p) && p[i] >= 0x3c && p[i] <= 0x3f {
		private = true
		i++
	}

	for i < len(p) && p[i] >= 0x30 && p[i] <= 0x3f {
		i++ // parameter bytes
	}

	intermediate := false

	for i < len(p) && p[i] >= 0x20 && p[i] <= 0x2f {
		i++
		intermediate = true
	}

	if i >= len(p) {
		return 0, false, false // final byte not yet arrived
	}

	final := p[i]
	i++

	if final < 0x40 || final > 0x7e {
		return i, false, true // malformed; drop it
	}

	return i, final == 'm' && !private && !intermediate, true
}

// scanString parses a string-terminated sequence (OSC after ESC ], and DCS / SOS
// / PM / APC after ESC P/X/^/_), which ends at BEL or ST (ESC \). None are SGR,
// so all are dropped; only their length and completeness matter.
func scanString(p []byte) (n int, sgr, complete bool) {
	for i := 2; i < len(p); i++ {
		switch p[i] {
		case bel:
			return i + 1, false, true
		case esc:
			if i+1 >= len(p) {
				return 0, false, false // possibly a truncated ST
			}

			if p[i+1] == '\\' {
				return i + 2, false, true
			}

			return i, false, true // a bare ESC ends it; rescan from there
		}
	}

	return 0, false, false // no terminator yet
}
