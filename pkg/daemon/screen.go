package daemon

import (
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

const (
	// maxHeldBytes bounds the partial sequence held across chunks. A sequence
	// with no terminator (an unterminated OSC/DCS in a malformed or binary
	// stream) would otherwise grow held without limit. Past this — far longer
	// than any sequence the screen acts on — the held bytes are dropped and
	// scanning resyncs on the next chunk.
	maxHeldBytes = 1 << 16 // 64 KiB

	// maxCols and maxRows bound the screen's size. The PTY takes whatever size
	// the client reports, but the screen allocates a cell per position, so a
	// bogus size must not allocate without limit; real terminals sit far below.
	maxCols = 4096
	maxRows = 1024

	tabWidth = 8
)

// screen is the session's shadow terminal: a model of what the session's
// terminal shows, fed the same raw PTY output the attached client's real
// terminal gets. It is never displayed live — the client renders the raw
// stream itself — but it decides two things a stream of bytes can't:
//
//   - What is history. A line is recorded (returned by write, for the log) only
//     when it scrolls off the top of the main screen, or when the screen is
//     cleared — exactly what a terminal's own scrollback would hold. A program
//     redrawing in place (a prompt, a progress bar, an inline TUI moving the
//     cursor up to repaint) changes the screen but records nothing.
//   - What an attach shows. paint reproduces the screen — both buffers, cursor,
//     scroll region and style — so re-attaching lands exactly where the session
//     is, ready for the program's next relative cursor move.
//
// It covers what shells and TUIs use: printing with autowrap and wide
// characters, cursor motion, erase/insert/delete, scroll regions, the alternate
// screen, SGR styling, cursor save/restore and the DEC line-drawing set. Queries
// are not answered (the client's terminal does that), and anything else is
// ignored.
type screen struct {
	cols, rows int

	main, alt []row
	grid      []row // the buffer on display: main or alt
	onAlt     bool

	x, y     int
	wrapNext bool // a character was just written in the last column; the next one wraps first
	top, bot int  // scroll region, inclusive rows
	wrapOff  bool // DECAWM reset: the last column is overwritten instead of wrapping
	insert   bool // IRM: printing shifts the rest of the row right
	origin   bool // DECOM: row addressing is relative to the scroll region
	decGfx   bool // G0 is the DEC special graphics (line-drawing) set

	sgr   sgrState
	style []byte // sgr rendered, taken by new cells (shared, immutable; nil = default)
	bg    []byte // the background part of style, taken by erased cells

	saved    savedCursor // DECSC / SCOSC / ?1048
	altSaved savedCursor // saved by ?1049 on entering the alt screen
	last     cell        // the last character printed, for REP

	held    []byte // a sequence or rune split across chunks, kept for the next one
	joined  []byte // scratch to splice held + the next chunk
	params  []int  // scratch for CSI parameters
	scratch []row  // scratch for rotating rows

	// modes tracks the terminal modes the program has set up (mouse reporting,
	// keyboard protocols, ...) — the parts of terminal state that aren't on
	// screen — so paint can hand them back to a new client.
	modes termModes

	out []byte // history produced by the current call; reused across calls
}

// row is one screen line.
type row struct {
	cells []cell
	// wrapped means the line continues on the next row: the text reached the
	// last column and autowrap carried it over, so the two rows are one logical
	// line — recorded as one line, not two.
	wrapped bool
}

// cell is one column of a row.
type cell struct {
	text  string // the grapheme shown; "" is a blank
	style []byte // rendered SGR (shared, immutable; nil = default)
	width uint8  // 1, or 2 for a wide character's left half; 0 is its right half
}

// blank is an unstyled empty cell.
var blank = cell{width: 1}

// isPad reports whether c is an unstyled blank or space: padding a row needn't
// carry. (A printed space and an erased cell look the same.)
func (c *cell) isPad() bool {
	return (c.text == "" || c.text == " ") && c.width == 1 && c.style == nil
}

// savedCursor is the state DECSC saves and DECRC restores.
type savedCursor struct {
	set                      bool
	x, y                     int
	sgr                      sgrState
	style, bg                []byte
	wrapNext, origin, decGfx bool
}

// asciiText interns the one-character strings of printable ASCII, so printing
// ASCII allocates nothing.
var asciiText = func() (t [128]string) {
	for i := 0x20; i < 0x7f; i++ {
		t[i] = string(rune(i))
	}

	return t
}()

// decGraphics maps 0x5f..0x7e to the DEC special graphics set, which TUIs
// switch G0 to ("\x1b(0") for box drawing.
var decGraphics = [...]string{
	" ", "◆", "▒", "␉", "␌", "␍", "␊", "°", "±", "␤", "␋", "┘", "┐", "┌", "└", "┼",
	"⎺", "⎻", "─", "⎼", "⎽", "├", "┤", "┴", "┬", "│", "≤", "≥", "π", "≠", "£", "·",
}

func newScreen(cols, rows int) *screen {
	s := &screen{}
	s.cols, s.rows = clampSize(cols, rows)
	s.main = newRows(s.cols, s.rows)
	s.alt = newRows(s.cols, s.rows)
	s.grid = s.main
	s.bot = s.rows - 1

	return s
}

func clampSize(cols, rows int) (int, int) {
	return min(max(cols, 1), maxCols), min(max(rows, 1), maxRows)
}

func newRows(cols, rows int) []row {
	g := make([]row, rows)
	for i := range g {
		g[i] = row{cells: blankCells(cols)}
	}

	return g
}

func blankCells(n int) []cell {
	c := make([]cell, n)
	for i := range c {
		c[i] = blank
	}

	return c
}

// write feeds a chunk of raw PTY output to the screen and returns the history it
// produced: the lines that left the main screen, rendered (see push). The
// returned slice is owned by the screen and valid until the next call.
func (s *screen) write(p []byte) []byte {
	p = s.spliceHeld(p)
	s.out = s.out[:0]

	for i := 0; i < len(p); {
		b := p[i]

		switch {
		case b == esc:
			n, complete := scanEscape(p[i:])
			if !complete {
				s.hold(p[i:])

				return s.out
			}

			s.escape(p[i : i+n])
			i += n
		case b < 0x20 || b == 0x7f:
			s.control(b)

			i++
		default:
			j := i + 1
			for j < len(p) && p[j] >= 0x20 && p[j] != 0x7f {
				j++
			}

			run := p[i:j]

			if j == len(p) {
				// A rune cut off by the chunk's end waits for its remaining bytes.
				if k := incompleteRune(run); k > 0 {
					s.print(run[:len(run)-k])
					s.hold(run[len(run)-k:])

					return s.out
				}
			}

			s.print(run)

			i = j
		}
	}

	return s.out
}

// spliceHeld prepends any bytes held from the previous chunk, so a sequence or
// rune straddling the boundary scans as a whole.
func (s *screen) spliceHeld(p []byte) []byte {
	if len(s.held) == 0 {
		return p
	}

	s.joined = append(append(s.joined[:0], s.held...), p...)
	s.held = s.held[:0]

	return s.joined
}

// hold keeps a partial sequence or rune for the next chunk to complete. Past
// maxHeldBytes it is dropped instead: nothing worth acting on is that long, so
// this is an unterminated sequence in a malformed stream.
func (s *screen) hold(rest []byte) {
	if len(rest) > maxHeldBytes {
		s.held = s.held[:0]

		return
	}

	s.held = append(s.held[:0], rest...)
}

// incompleteRune reports how many bytes at the end of b are a UTF-8 rune
// missing its continuation bytes (0 if b ends on a whole rune).
func incompleteRune(b []byte) int {
	for i := 1; i <= utf8.UTFMax-1 && i <= len(b); i++ {
		c := b[len(b)-i]
		if c < utf8.RuneSelf {
			return 0
		}

		if utf8.RuneStart(c) {
			if utf8.FullRune(b[len(b)-i:]) {
				return 0
			}

			return i
		}
	}

	return 0
}

// print writes a run of printable bytes, one grapheme cluster per cell (two
// for a wide one). ASCII takes a fast path; anything else is segmented into
// grapheme clusters, so combining marks and emoji sequences stay whole.
func (s *screen) print(run []byte) {
	state := -1

	for len(run) > 0 {
		b := run[0]
		if b < utf8.RuneSelf && (len(run) == 1 || run[1] < utf8.RuneSelf) {
			text := asciiText[b]
			if s.decGfx && b >= 0x5f && b <= 0x7e {
				text = decGraphics[b-0x5f]
			}

			s.put(text, 1)

			run = run[1:]
			state = -1

			continue
		}

		var cluster []byte

		var width int

		cluster, run, width, state = uniseg.FirstGraphemeCluster(run, state)
		s.put(string(cluster), min(width, 2))
	}
}

// put writes one grapheme of the given width at the cursor, wrapping first if
// the previous character filled the row. A zero-width grapheme — a combining
// mark that arrived in a later chunk than its base — joins the previous cell.
func (s *screen) put(text string, width int) {
	if width == 0 {
		s.combine(text)

		return
	}

	if s.cols < 2 {
		width = 1
	}

	if s.wrapNext && !s.wrapOff {
		s.wrap()
	}

	if width == 2 && s.x == s.cols-1 {
		if s.wrapOff {
			return // a wide character can't fit in the last column
		}

		s.eraseCells(s.y, s.x, s.cols)
		s.wrap()
	}

	r := &s.grid[s.y]
	if s.insert {
		s.insertCells(r, s.x, width)
	}

	s.splitWide(r, s.x)
	s.splitWide(r, s.x+width)

	c := cell{text: text, style: s.style, width: uint8(width)}
	r.cells[s.x] = c

	if width == 2 {
		r.cells[s.x+1] = cell{style: s.style}
	}

	s.last = c

	if s.x+width >= s.cols {
		s.x = s.cols - 1
		s.wrapNext = true
	} else {
		s.x += width
		s.wrapNext = false
	}
}

// wrap carries the cursor to the start of the next row, marking the row it
// leaves as continuing there.
func (s *screen) wrap() {
	s.grid[s.y].wrapped = true
	s.x = 0
	s.wrapNext = false
	s.index()
}

// combine appends a zero-width grapheme to the character left of the cursor.
func (s *screen) combine(text string) {
	x := s.x - 1
	if s.wrapNext {
		x = s.x
	}

	r := &s.grid[s.y]
	if x > 0 && r.cells[x].width == 0 {
		x--
	}

	if x >= 0 {
		r.cells[x].text += text
	}
}

// control acts on a C0 control byte.
func (s *screen) control(b byte) {
	switch b {
	case '\b':
		if s.x > 0 {
			s.x--
		}

		s.wrapNext = false
	case '\t':
		s.x = min((s.x/tabWidth+1)*tabWidth, s.cols-1)
		s.wrapNext = false
	case '\n', '\v', '\f':
		s.wrapNext = false
		s.index()
	case '\r':
		s.x = 0
		s.wrapNext = false
	}
}

// index moves the cursor down a row, scrolling the region up when it is on the
// region's bottom row (LF, IND).
func (s *screen) index() {
	switch {
	case s.y == s.bot:
		s.scrollUp(s.top, s.bot, 1)
	case s.y < s.rows-1:
		s.y++
	}
}

// reverseIndex moves the cursor up a row, scrolling the region down when it is
// on the region's top row (RI).
func (s *screen) reverseIndex() {
	switch {
	case s.y == s.top:
		s.scrollDown(s.top, s.bot, 1)
	case s.y > 0:
		s.y--
	}
}

// escape acts on one complete escape sequence. Every sequence also feeds the
// mode tracker, which keeps the off-screen terminal state paint restores.
func (s *screen) escape(seq []byte) {
	s.modes.track(seq)

	switch seq[1] {
	case '[':
		s.csi(seq)
	case '7':
		s.saveCursor(&s.saved)
	case '8':
		s.restoreCursor(&s.saved)
	case 'D':
		s.wrapNext = false
		s.index()
	case 'E':
		s.x = 0
		s.wrapNext = false
		s.index()
	case 'M':
		s.wrapNext = false
		s.reverseIndex()
	case 'c':
		s.fullReset()
	case '(':
		if len(seq) == 3 {
			s.decGfx = seq[2] == '0'
		}
	}
}

func (s *screen) saveCursor(sc *savedCursor) {
	*sc = savedCursor{
		set: true, x: s.x, y: s.y, sgr: s.sgr, style: s.style, bg: s.bg,
		wrapNext: s.wrapNext, origin: s.origin, decGfx: s.decGfx,
	}
}

// restoreCursor restores a saved cursor, or homes it with default attributes
// when nothing was saved — what a terminal does.
func (s *screen) restoreCursor(sc *savedCursor) {
	saved := *sc
	if !saved.set {
		saved = savedCursor{}
	}

	s.x, s.y = min(saved.x, s.cols-1), min(saved.y, s.rows-1)
	s.sgr, s.style, s.bg = saved.sgr, saved.style, saved.bg
	s.wrapNext, s.origin, s.decGfx = saved.wrapNext, saved.origin, saved.decGfx
}

// setAlt switches between the main and alternate screen buffers.
func (s *screen) setAlt(on bool) {
	if on == s.onAlt {
		return
	}

	s.onAlt = on
	s.grid = s.main

	if on {
		s.grid = s.alt
	}
}

// fullReset is RIS: the terminal back to its power-on state. What the main
// screen showed is kept as history first, the way a clear keeps it (see
// eraseDisplay).
func (s *screen) fullReset() {
	s.setAlt(false)
	s.pushScreen()

	*s = screen{
		cols: s.cols, rows: s.rows, main: s.main, alt: s.alt,
		held: s.held, joined: s.joined, params: s.params, scratch: s.scratch, out: s.out,
	}
	s.clearRows(s.main)
	s.clearRows(s.alt)
	s.grid = s.main
	s.bot = s.rows - 1
}

// softReset is DECSTR: modes and attributes back to their defaults, the screen
// and cursor position untouched.
func (s *screen) softReset() {
	s.sgr, s.style, s.bg = sgrState{}, nil, nil
	s.insert, s.origin, s.wrapOff, s.decGfx, s.wrapNext = false, false, false, false, false
	s.top, s.bot = 0, s.rows-1
	s.saved = savedCursor{}
}

// setStyle folds an SGR parameter list into the style new cells take, and
// keeps the background part for erased cells (terminals erase with the current
// background). Each change renders fresh slices, so styles already stored in
// cells are never mutated underneath them.
func (s *screen) setStyle(params []byte) {
	before := s.sgr
	s.sgr.apply(string(params))

	if s.sgr == before {
		return
	}

	s.style = s.sgr.render()

	if s.sgr[slotBg] != before[slotBg] {
		var bg sgrState

		bg[slotBg] = s.sgr[slotBg]
		s.bg = bg.render()
	}
}
