package daemon

// csiParams is a CSI sequence's numeric parameters; a missing or empty one is 0.
type csiParams []int

// get returns parameter i, or def when it is missing or 0 (the "default" every
// CSI count and position treats 0 as).
func (p csiParams) get(i, def int) int {
	if i < len(p) && p[i] > 0 {
		return p[i]
	}

	return def
}

// n returns parameter i as a count: at least 1.
func (p csiParams) n(i int) int { return p.get(i, 1) }

// csiHandlers dispatches plain (no private marker, no intermediate) CSI
// sequences by final byte.
var csiHandlers = map[byte]func(s *screen, p csiParams){
	'A': func(s *screen, p csiParams) { s.cursorUp(p.n(0)) },
	'B': func(s *screen, p csiParams) { s.cursorDown(p.n(0)) },
	'e': func(s *screen, p csiParams) { s.cursorDown(p.n(0)) },
	'C': func(s *screen, p csiParams) { s.setX(s.x + p.n(0)) },
	'a': func(s *screen, p csiParams) { s.setX(s.x + p.n(0)) },
	'D': func(s *screen, p csiParams) { s.setX(s.x - p.n(0)) },
	'E': func(s *screen, p csiParams) { s.cursorDown(p.n(0)); s.setX(0) },
	'F': func(s *screen, p csiParams) { s.cursorUp(p.n(0)); s.setX(0) },
	'G': func(s *screen, p csiParams) { s.setX(p.n(0) - 1) },
	'`': func(s *screen, p csiParams) { s.setX(p.n(0) - 1) },
	'd': func(s *screen, p csiParams) { s.moveTo(s.x, s.originRow(p.n(0)-1)) },
	'H': func(s *screen, p csiParams) { s.moveTo(p.n(1)-1, s.originRow(p.n(0)-1)) },
	'f': func(s *screen, p csiParams) { s.moveTo(p.n(1)-1, s.originRow(p.n(0)-1)) },
	'I': func(s *screen, p csiParams) { s.tabForward(p.n(0)) },
	'Z': func(s *screen, p csiParams) { s.tabBackward(p.n(0)) },
	'J': func(s *screen, p csiParams) { s.eraseDisplay(p.get(0, 0)) },
	'K': func(s *screen, p csiParams) { s.eraseLine(p.get(0, 0)) },
	'X': func(s *screen, p csiParams) { s.eraseCells(s.y, s.x, s.x+p.n(0)) },
	'@': func(s *screen, p csiParams) { s.insertCells(&s.grid[s.y], s.x, p.n(0)) },
	'P': func(s *screen, p csiParams) { s.deleteCells(p.n(0)) },
	'L': func(s *screen, p csiParams) { s.insertLines(p.n(0)) },
	'M': func(s *screen, p csiParams) { s.deleteLines(p.n(0)) },
	'S': func(s *screen, p csiParams) { s.scrollUp(s.top, s.bot, p.n(0)) },
	'T': func(s *screen, p csiParams) {
		if len(p) <= 1 { // with more it is mouse highlight tracking
			s.scrollDown(s.top, s.bot, p.n(0))
		}
	},
	'b': func(s *screen, p csiParams) { s.repeat(p.n(0)) },
	'r': func(s *screen, p csiParams) { s.setRegion(p.get(0, 1)-1, p.get(1, s.rows)-1) },
	's': func(s *screen, p csiParams) {
		if len(p) == 0 {
			s.saveCursor(&s.saved)
		}
	},
	'u': func(s *screen, p csiParams) {
		if len(p) == 0 {
			s.restoreCursor(&s.saved)
		}
	},
	'h': func(s *screen, p csiParams) { s.ansiMode(p, true) },
	'l': func(s *screen, p csiParams) { s.ansiMode(p, false) },
}

// csi acts on one complete CSI sequence.
func (s *screen) csi(seq []byte) {
	body := seq[2 : len(seq)-1]
	final := seq[len(seq)-1]

	var private byte
	if len(body) > 0 && body[0] >= 0x3c && body[0] <= 0x3f {
		private, body = body[0], body[1:]
	}

	i := len(body)
	for i > 0 && body[i-1] >= 0x20 && body[i-1] <= 0x2f {
		i--
	}

	inter, params := body[i:], body[:i]

	switch {
	case private == '?':
		s.privateCSI(final, s.parse(params))
	case private != 0:
		// Kitty keyboard, modifyOtherKeys, device queries: terminal state the
		// mode tracker keeps, nothing on screen.
	case len(inter) > 0:
		if string(inter) == "!" && final == 'p' {
			s.softReset()
		}
	case final == 'm':
		s.setStyle(params)
	default:
		if h := csiHandlers[final]; h != nil {
			h(s, s.parse(params))
		}
	}
}

// privateCSI acts on a "\x1b[?" sequence: DEC private modes and the selective
// erases (treated as plain erases).
func (s *screen) privateCSI(final byte, p csiParams) {
	switch final {
	case 'h', 'l':
		for _, mode := range p {
			s.decMode(mode, final == 'h')
		}
	case 'J':
		s.eraseDisplay(p.get(0, 0))
	case 'K':
		s.eraseLine(p.get(0, 0))
	}
}

// parse splits CSI parameter bytes into numbers, reusing the screen's scratch.
// A sub-parameter list ("4:3") counts as its leading number.
func (s *screen) parse(b []byte) csiParams {
	s.params = s.params[:0]

	if len(b) == 0 {
		return s.params
	}

	n, sub := 0, false

	for _, c := range b {
		switch {
		case c == ';':
			s.params = append(s.params, n)
			n, sub = 0, false
		case c == ':':
			sub = true
		case c >= '0' && c <= '9' && !sub && n < 1<<16:
			n = n*10 + int(c-'0')
		}
	}

	return append(s.params, n)
}

// decMode sets or resets one DEC private mode the screen acts on.
func (s *screen) decMode(mode int, on bool) {
	switch mode {
	case 6:
		s.origin = on
		s.moveTo(0, s.originRow(0))
	case 7:
		s.wrapOff = !on
	case 47:
		s.setAlt(on)
	case 1047:
		if !on && s.onAlt {
			s.clearRows(s.alt)
		}

		s.setAlt(on)
	case 1048:
		if on {
			s.saveCursor(&s.saved)
		} else {
			s.restoreCursor(&s.saved)
		}
	case 1049:
		if on && !s.onAlt {
			s.saveCursor(&s.altSaved)
			s.clearRows(s.alt)
			s.setAlt(true)
		} else if !on && s.onAlt {
			s.setAlt(false)
			s.restoreCursor(&s.altSaved)
		}
	}
}

// ansiMode sets or resets ANSI modes; only insert mode (IRM, 4) changes what
// lands on screen.
func (s *screen) ansiMode(p csiParams, on bool) {
	for _, mode := range p {
		if mode == 4 {
			s.insert = on
		}
	}
}
