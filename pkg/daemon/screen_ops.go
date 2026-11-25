package daemon

import "slices"

// setX moves the cursor to column x on its row, clamped to the screen.
func (s *screen) setX(x int) {
	s.x = min(max(x, 0), s.cols-1)
	s.wrapNext = false
}

// moveTo moves the cursor to (x, y) in screen coordinates, clamped to the
// screen — or to the scroll region in origin mode.
func (s *screen) moveTo(x, y int) {
	lo, hi := 0, s.rows-1
	if s.origin {
		lo, hi = s.top, s.bot
	}

	s.y = min(max(y, lo), hi)
	s.setX(x)
}

// originRow converts a row as addressed by CUP/VPA to a screen row: in origin
// mode rows count from the top of the scroll region.
func (s *screen) originRow(y int) int {
	if s.origin {
		return y + s.top
	}

	return y
}

// cursorUp moves up n rows, stopping at the region's top when it starts inside
// the region and at the screen's top otherwise.
func (s *screen) cursorUp(n int) {
	lo := 0
	if s.y >= s.top {
		lo = s.top
	}

	s.y = max(s.y-n, lo)
	s.wrapNext = false
}

// cursorDown moves down n rows, stopping at the region's bottom when it starts
// inside the region and at the screen's bottom otherwise.
func (s *screen) cursorDown(n int) {
	hi := s.rows - 1
	if s.y <= s.bot {
		hi = s.bot
	}

	s.y = min(s.y+n, hi)
	s.wrapNext = false
}

func (s *screen) tabForward(n int) {
	s.setX((s.x/tabWidth + n) * tabWidth)
}

func (s *screen) tabBackward(n int) {
	s.setX(((s.x+tabWidth-1)/tabWidth - n) * tabWidth)
}

// setRegion sets the scroll region (DECSTBM) and homes the cursor. An invalid
// region is ignored, as terminals do.
func (s *screen) setRegion(top, bot int) {
	bot = min(bot, s.rows-1)
	if top < 0 || top >= bot {
		return
	}

	s.top, s.bot = top, bot
	s.moveTo(0, s.originRow(0))
}

// repeat prints the last printed character n more times (REP).
func (s *screen) repeat(n int) {
	if s.last.width == 0 {
		return
	}

	for range min(n, s.cols*s.rows) {
		s.put(s.last.text, int(s.last.width))
	}
}

// eraseDisplay is ED. Clearing the whole main screen keeps what it showed as
// history first — tmux's scroll-on-clear — so `clear` or Ctrl-L never loses
// output that hadn't scrolled off yet. A clear from the home position (ED 0 at
// 0,0) is a whole-screen clear too. ED 3 asks a terminal to drop its scrollback,
// which a session's history deliberately outlives, so it is ignored.
func (s *screen) eraseDisplay(mode int) {
	switch mode {
	case 0:
		if s.x == 0 && s.y == 0 {
			s.pushScreen()
		}

		s.eraseCells(s.y, s.x, s.cols)

		for y := s.y + 1; y < s.rows; y++ {
			s.eraseRow(y)
		}
	case 1:
		for y := range s.y {
			s.eraseRow(y)
		}

		s.eraseCells(s.y, 0, s.x+1)
	case 2:
		s.pushScreen()

		for y := range s.rows {
			s.eraseRow(y)
		}
	}
}

// eraseLine is EL. Erasing through the row's end also ends its continuation
// onto the next row.
func (s *screen) eraseLine(mode int) {
	switch mode {
	case 0:
		s.eraseCells(s.y, s.x, s.cols)
		s.grid[s.y].wrapped = false
	case 1:
		s.eraseCells(s.y, 0, s.x+1)
	case 2:
		s.eraseRow(s.y)
	}
}

func (s *screen) eraseRow(y int) {
	s.eraseCells(y, 0, s.cols)
	s.grid[y].wrapped = false
}

// eraseCells blanks columns [from, to) of row y with the current background.
func (s *screen) eraseCells(y, from, to int) {
	from, to = max(from, 0), min(to, s.cols)
	if from >= to {
		return
	}

	r := &s.grid[y]
	s.splitWide(r, from)
	s.splitWide(r, to)

	b := cell{width: 1, style: s.bg}
	for x := from; x < to; x++ {
		r.cells[x] = b
	}

	s.wrapNext = false
}

// splitWide blanks a wide character straddling the boundary before column x
// (left half at x-1, right half at x), so an edit starting or ending at x never
// leaves half of one behind.
func (s *screen) splitWide(r *row, x int) {
	if x <= 0 || x >= len(r.cells) || r.cells[x].width != 0 {
		return
	}

	r.cells[x-1] = blank
	r.cells[x] = blank
}

// insertCells shifts row r right by n from column x, blanking the gap (ICH,
// and printing in insert mode). Cells pushed past the edge are lost.
func (s *screen) insertCells(r *row, x, n int) {
	n = min(n, s.cols-x)
	if n <= 0 {
		return
	}

	s.splitWide(r, x)
	copy(r.cells[x+n:], r.cells[x:s.cols-n])

	b := cell{width: 1, style: s.bg}
	for i := x; i < x+n; i++ {
		r.cells[i] = b
	}

	if last := &r.cells[s.cols-1]; last.width == 2 {
		*last = blank // its right half was pushed off the edge
	}

	s.wrapNext = false
}

// deleteCells removes n cells at the cursor, shifting the rest of the row left
// and blanking the end (DCH).
func (s *screen) deleteCells(n int) {
	r := &s.grid[s.y]
	n = min(n, s.cols-s.x)

	s.splitWide(r, s.x)
	s.splitWide(r, s.x+n)
	copy(r.cells[s.x:], r.cells[s.x+n:])

	b := cell{width: 1, style: s.bg}
	for i := s.cols - n; i < s.cols; i++ {
		r.cells[i] = b
	}

	s.wrapNext = false
}

// insertLines inserts n blank rows at the cursor's row, pushing the rows below
// it down within the scroll region (IL). Rows pushed past the region's bottom
// are discarded, not recorded: they never left the screen's top.
func (s *screen) insertLines(n int) {
	if s.y < s.top || s.y > s.bot {
		return
	}

	s.scrollDown(s.y, s.bot, n)
	s.x = 0
}

// deleteLines removes n rows at the cursor's row, pulling the rows below it up
// within the scroll region (DL).
func (s *screen) deleteLines(n int) {
	if s.y < s.top || s.y > s.bot {
		return
	}

	n = min(n, s.bot-s.y+1)
	s.rotate(s.grid[s.y:s.bot+1], n)
	s.clearRows(s.grid[s.bot-n+1 : s.bot+1])
	s.x = 0
	s.wrapNext = false
}

// scrollUp scrolls rows top..bot up by n. Rows leaving the top of the main
// screen become history; rows leaving anywhere else (a region lower down, the
// alt screen) are just gone, as in any terminal.
func (s *screen) scrollUp(top, bot, n int) {
	n = min(n, bot-top+1)
	if n <= 0 {
		return
	}

	if top == 0 && !s.onAlt {
		for y := range n {
			s.push(&s.grid[y])
		}
	}

	s.rotate(s.grid[top:bot+1], n)
	s.clearRows(s.grid[bot-n+1 : bot+1])
}

// scrollDown scrolls rows top..bot down by n, blanking the rows opened at top.
func (s *screen) scrollDown(top, bot, n int) {
	n = min(n, bot-top+1)
	if n <= 0 {
		return
	}

	g := s.grid[top : bot+1]
	s.rotate(g, len(g)-n)
	s.clearRows(g[:n])
	s.wrapNext = false
}

// rotate rotates g left by n rows, reusing the rows' cell storage.
func (s *screen) rotate(g []row, n int) {
	if n <= 0 || n >= len(g) {
		return
	}

	s.scratch = append(s.scratch[:0], g[:n]...)
	copy(g, g[n:])
	copy(g[len(g)-n:], s.scratch)
}

// clearRows blanks rows with the current background.
func (s *screen) clearRows(g []row) {
	b := cell{width: 1, style: s.bg}

	for i := range g {
		for x := range g[i].cells {
			g[i].cells[x] = b
		}

		g[i].wrapped = false
	}
}

// pushScreen records the main screen's rows, down to its last non-blank one,
// as history — what a clear keeps. The last of them always ends its line: what
// it continued onto is being erased.
func (s *screen) pushScreen() {
	if s.onAlt {
		return
	}

	last := lastTextRow(s.main)
	for y := 0; y <= last; y++ {
		s.push(&s.main[y])
	}

	if last >= 0 && s.main[last].wrapped {
		s.out = append(s.out, '\n')
	}
}

// push records a row leaving the main screen as history. A wrapped row runs on
// into the next one pushed, so a logical line is recorded as one line.
func (s *screen) push(r *row) {
	s.out, _ = renderRow(s.out, r.cells, r.wrapped)
	if !r.wrapped {
		s.out = append(s.out, '\n')
	}
}

// lastTextRow returns the index of the last row with any text, or -1.
func lastTextRow(g []row) int {
	for y, v := range slices.Backward(g) {
		for _, c := range v.cells {
			if c.text != "" {
				return y
			}
		}
	}

	return -1
}

// clearContent blanks both buffers, keeping the cursor, modes and style: a
// history wipe (see scrollback.Clear) erases what the screen shows too, since
// an attach would otherwise replay it.
func (s *screen) clearContent() {
	s.clearRows(s.main)
	s.clearRows(s.alt)
}

// resize changes the screen's size and returns the history it produced. Rows
// are not reflowed. Shrinking drops blank rows from the bottom first, then
// scrolls rows off the top — into history on the main screen — so the cursor
// stays on screen; growing adds blank rows at the bottom. Rows are truncated or
// padded to the new width. The scroll region resets to the whole screen, as
// terminals do.
func (s *screen) resize(cols, rows int) []byte {
	s.out = s.out[:0]

	cols, rows = clampSize(cols, rows)
	if cols == s.cols && rows == s.rows {
		return s.out
	}

	// The buffer not on display has no live cursor: the main screen's is the one
	// ?1049 saved; the alt screen's content is stale until it is shown again.
	mainY, altY := s.y, 0
	if s.onAlt {
		mainY, altY = s.altSaved.y, s.y
	}

	var cut int

	s.main, cut = s.resizeRows(s.main, cols, rows, mainY, true)
	if !s.onAlt {
		s.y -= cut
	} else {
		s.altSaved.y = max(s.altSaved.y-cut, 0)
	}

	s.alt, cut = s.resizeRows(s.alt, cols, rows, altY, false)
	if s.onAlt {
		s.y -= cut
	}

	s.cols, s.rows = cols, rows
	s.grid = s.main

	if s.onAlt {
		s.grid = s.alt
	}

	s.top, s.bot = 0, rows-1
	s.x, s.y = min(s.x, cols-1), min(max(s.y, 0), rows-1)
	s.wrapNext = false
	s.saved.x, s.saved.y = min(s.saved.x, cols-1), min(s.saved.y, rows-1)

	return s.out
}

// resizeRows fits buffer g to cols×rows, returning it and how many rows it
// dropped from the top. cy is the buffer's cursor row; main says whether rows
// dropped from the top are history.
func (s *screen) resizeRows(g []row, cols, rows, cy int, main bool) ([]row, int) {
	cut := 0

	for len(g) > rows && len(g)-1 > cy && lastTextRow(g[len(g)-1:]) < 0 {
		g = g[:len(g)-1]
	}

	if extra := len(g) - rows; extra > 0 {
		if main {
			for y := range extra {
				s.push(&g[y])
			}
		}

		g, cut = g[extra:], extra
	}

	for i := range g {
		g[i].cells = fitCells(g[i].cells, cols)
	}

	for len(g) < rows {
		g = append(g, row{cells: blankCells(cols)})
	}

	return g, cut
}

// fitCells truncates or pads a row's cells to n, blanking a wide character cut
// in half by the new edge.
func fitCells(c []cell, n int) []cell {
	if len(c) > n {
		c = c[:n]
		if c[n-1].width == 2 {
			c[n-1] = blank
		}

		return c
	}

	for len(c) < n {
		c = append(c, blank)
	}

	return c
}
