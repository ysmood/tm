//go:build unix

package app_test

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// vt is a minimal terminal model: enough to track the main-screen scrollback and
// visible grid so a test can assert what the user can actually see. The tests feed
// it everything tm wrote to a PTY (see term.screen) and then read the resulting
// screen, which is the only way to tell "drawn below the prompt" from "drew over
// it" — both are the same bytes until a terminal interprets them.
type vt struct {
	rows, cols     int
	cur            [][]rune
	scroll         []string
	cr, cc         int
	savedR, savedC int // DECSC/DECRC saved cursor (ESC 7 / ESC 8)
}

func newVT(rows, cols int) *vt {
	v := &vt{rows: rows, cols: cols}
	v.cur = v.blank()

	return v
}

func (v *vt) blank() [][]rune {
	g := make([][]rune, v.rows)
	for i := range g {
		g[i] = make([]rune, v.cols)
		for j := range g[i] {
			g[i][j] = ' '
		}
	}

	return g
}

func (v *vt) line(r []rune) string { return strings.TrimRight(string(r), " ") }

func (v *vt) scrollUp() {
	v.scroll = append(v.scroll, v.line(v.cur[0]))
	copy(v.cur, v.cur[1:])

	last := make([]rune, v.cols)
	for j := range last {
		last[j] = ' '
	}

	v.cur[v.rows-1] = last
}

func (v *vt) newline() {
	v.cr++
	if v.cr >= v.rows {
		v.cr = v.rows - 1
		v.scrollUp()
	}
}

// reverseIndex is ESC M: cursor up one row, scrolling the screen down when it is
// already at the top. Bubble Tea uses it (with insertLines) to open a line above
// the inline picker for tea.Println output.
func (v *vt) reverseIndex() {
	if v.cr > 0 {
		v.cr--

		return
	}

	v.insertLines(1)
}

// insertLines is CSI L: push the rows from the cursor down by n, blanking the n
// rows it opens up. Rows shifted past the bottom are lost.
func (v *vt) insertLines(n int) {
	blank := v.blank()

	for i := v.rows - 1; i >= v.cr+n; i-- {
		v.cur[i] = v.cur[i-n]
	}

	for i := v.cr; i < min(v.cr+n, v.rows); i++ {
		v.cur[i] = blank[i]
	}
}

// deleteLines is CSI M: remove n rows at the cursor, pulling the rows below up
// and blanking the bottom. The prompt guard uses it (with insertLines) to move
// rename notices from below the prompt to above it.
func (v *vt) deleteLines(n int) {
	blank := v.blank()

	for i := v.cr; i < v.rows; i++ {
		if i+n < v.rows {
			v.cur[i] = v.cur[i+n]
		} else {
			v.cur[i] = blank[i]
		}
	}
}

func (v *vt) put(ch rune) {
	if v.cc >= v.cols {
		v.cc = 0
		v.newline()
	}

	v.cur[v.cr][v.cc] = ch
	v.cc++
}

func (v *vt) feed(p []byte) {
	for i := 0; i < len(p); {
		switch b := p[i]; {
		case b == 0x1b && i+1 < len(p) && p[i+1] == '7': // DECSC: save cursor
			v.savedR, v.savedC = v.cr, v.cc
			i += 2
		case b == 0x1b && i+1 < len(p) && p[i+1] == '8': // DECRC: restore cursor
			v.cr, v.cc = v.savedR, v.savedC
			i += 2
		case b == 0x1b && i+1 < len(p) && p[i+1] == 'M': // RI: reverse index
			v.reverseIndex()

			i += 2
		case b == 0x1b:
			i += v.esc(p[i:])
		case b == '\n':
			v.newline()

			i++
		case b == '\r':
			v.cc = 0
			i++
		case b == '\b':
			if v.cc > 0 {
				v.cc--
			}

			i++
		case b < 0x20:
			i++
		default:
			r, size := utf8.DecodeRune(p[i:])
			v.put(r) // each rune is one column (no wide chars in this UI)

			i += size
		}
	}
}

// esc handles one CSI sequence (cursor motion and erases are all this needs).
func (v *vt) esc(p []byte) int {
	if len(p) < 2 || p[1] != '[' {
		return 2
	}

	i := 2
	for i < len(p) && (p[i] == '?' || p[i] == '>' || p[i] == '=' || p[i] == '<') {
		i++
	}

	ps := i

	for i < len(p) && ((p[i] >= '0' && p[i] <= '9') || p[i] == ';') {
		i++
	}

	params := string(p[ps:i])

	for i < len(p) && p[i] >= 0x20 && p[i] <= 0x2f {
		i++
	}

	if i >= len(p) {
		return len(p)
	}

	final := p[i]
	i++

	v.apply(params, final)

	return i
}

func (v *vt) apply(params string, final byte) {
	n := func(idx, def int) int {
		fields := strings.Split(params, ";")
		if idx < len(fields) && fields[idx] != "" {
			x := 0
			_, _ = fmt.Sscanf(fields[idx], "%d", &x)

			if x > 0 {
				return x
			}
		}

		return def
	}

	switch final {
	case 'A':
		v.cr = max(0, v.cr-n(0, 1))
	case 'B':
		v.cr = min(v.rows-1, v.cr+n(0, 1))
	case 'H', 'f':
		v.cr, v.cc = max(0, n(0, 1)-1), max(0, n(1, 1)-1)
	case 'J':
		v.eraseDisplay(n(0, 0))
	case 'L':
		// IL and DL move the cursor to the first column on VT/xterm-family
		// terminals (unlike RI, which keeps it — see reverseIndex). Modeled so a
		// cursor position that only looks right without the reset can't pass.
		v.insertLines(n(0, 1))
		v.cc = 0
	case 'M':
		v.deleteLines(n(0, 1))
		v.cc = 0
	case 'K':
		for j := v.cc; j < v.cols; j++ {
			v.cur[v.cr][j] = ' '
		}
	case 'r':
		// DECSTBM (set scroll region) homes the cursor to the top margin as a side
		// effect — the very behavior that made a bare \e[r wipe the screen when the
		// menu reopened. Model it so tests can catch a regression.
		v.cr, v.cc = max(0, n(0, 1)-1), 0
	}
}

func (v *vt) eraseDisplay(mode int) {
	switch mode {
	case 0: // cursor to end of screen
		for j := v.cc; j < v.cols; j++ {
			v.cur[v.cr][j] = ' '
		}

		for r := v.cr + 1; r < v.rows; r++ {
			for j := range v.cur[r] {
				v.cur[r][j] = ' '
			}
		}
	case 2: // whole screen
		v.cur = v.blank()
	case 3: // scrollback
		v.scroll = nil
	}
}

// visible is the scrollback plus the current screen: everything the user could
// scroll back to see.
func (v *vt) visible() string {
	var b strings.Builder

	for _, l := range v.scroll {
		b.WriteString(l)
		b.WriteString("\n")
	}

	b.WriteString(v.screen())

	return b.String()
}

// screen returns only the on-screen rows (no scrollback), i.e. what is actually
// visible right now — used to catch content being erased off the current screen.
func (v *vt) screen() string {
	var b strings.Builder

	for _, row := range v.cur {
		b.WriteString(v.line(row))
		b.WriteString("\n")
	}

	return b.String()
}

// cursorLine is the line the cursor is sitting on, so a test can check the
// resumed session put it back on the shell's prompt.
func (v *vt) cursorLine() string { return v.line(v.cur[v.cr]) }
