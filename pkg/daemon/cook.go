package daemon

import (
	"bytes"
	"unicode/utf8"
)

const (
	esc = 0x1b
	bel = 0x07
)

// space is the byte a blank cell holds; also what renderLine trims from a line's
// end.
var space = []byte{' '}

// sgrReset ends the styling emitted for a rendered line, so each recorded line
// carries its own complete style and colors never bleed between lines in a pager.
var sgrReset = []byte("\x1b[0m")

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
type cooker struct {
	cells   []cell // the in-progress (not yet newline-terminated) line, by column
	col     int    // the cursor's column within the line
	style   []byte // SGR sequence(s) active for newly written cells (nil = default)
	pending []byte // a sequence split across cook calls, held for the next chunk
}

// cell is one column of the current line: the character's bytes and the SGR
// style that was active when it was written.
type cell struct {
	style []byte
	text  []byte
}

// cook consumes a chunk of raw PTY output and returns the finished lines to
// append to the log — every line this chunk terminated with a newline, rendered
// to its visible form. The in-progress line stays buffered in the cooker (see
// tail), so nothing is written until a newline settles it and a later overwrite
// can still revise it.
func (c *cooker) cook(p []byte) []byte {
	// Work on a copy detached from the caller's read buffer (which is reused for
	// the next read): cells alias these bytes and must outlive this call. A held
	// partial sequence from last time is prepended so it is scanned as a whole.
	p = append(c.pending, p...)
	c.pending = nil

	var out []byte

	for i := 0; i < len(p); {
		b := p[i]

		switch {
		case b == esc:
			n, sgr, complete := scanEscape(p[i:])
			if !complete {
				c.pending = append([]byte(nil), p[i:]...)

				return out
			}

			if sgr {
				c.setStyle(p[i : i+n])
			}

			i += n
		case b == '\n':
			out = append(out, c.renderLine()...)
			out = append(out, '\n')
			c.cells = c.cells[:0]
			c.col = 0
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
				c.pending = append([]byte(nil), p[i:]...) // truncated UTF-8; await the rest

				return out
			}

			c.put(r)

			i += size
		default:
			i++ // drop other C0 controls: bell, form feed, DEL, ...
		}
	}

	return out
}

// tail returns the in-progress line's visible bytes, so a replay can show the
// live prompt that has not yet been committed with a newline.
func (c *cooker) tail() []byte {
	return c.renderLine()
}

// reset discards all buffered state, so a cleared session starts from a clean
// slate with no half-built line or partial sequence carried across the wipe.
func (c *cooker) reset() {
	*c = cooker{}
}

// put writes one character's bytes at the cursor column with the active style,
// padding with blank cells if the cursor sits past the line's end, then advances
// the cursor.
func (c *cooker) put(text []byte) {
	for len(c.cells) < c.col {
		c.cells = append(c.cells, cell{text: space})
	}

	if c.col < len(c.cells) {
		c.cells[c.col] = cell{style: c.style, text: text}
	} else {
		c.cells = append(c.cells, cell{style: c.style, text: text})
	}

	c.col++
}

// renderLine renders the current line's cells to their visible bytes: the text,
// with each cell's style emitted when it changes (reset first, so a run's style
// never carries over from the previous one), and a closing reset if the line
// ended styled. Trailing spaces are dropped — a terminal pads a redrawn line out
// to its width, but a log needs no padding.
func (c *cooker) renderLine() []byte {
	end := len(c.cells)
	for end > 0 && bytes.Equal(c.cells[end-1].text, space) {
		end--
	}

	var out, cur []byte

	for _, cl := range c.cells[:end] {
		if !bytes.Equal(cl.style, cur) {
			if len(cur) > 0 {
				out = append(out, sgrReset...)
			}

			out = append(out, cl.style...)
			cur = cl.style
		}

		out = append(out, cl.text...)
	}

	if len(cur) > 0 {
		out = append(out, sgrReset...)
	}

	return out
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
		c.style = append([]byte(nil), seq...) // a reset mixed with new attributes
	default:
		c.style = append(append([]byte(nil), c.style...), seq...)
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
