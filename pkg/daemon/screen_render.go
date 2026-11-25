package daemon

import (
	"bytes"
	"strconv"
)

// sgrReset ends the styling emitted for a rendered row, so each one carries its
// own complete style and colors never bleed between lines in a pager.
var sgrReset = []byte("\x1b[0m")

// renderRow appends cells' visible form to dst — text, with each run's style
// emitted where it changes and a closing reset if the row ends styled — and
// returns it with the number of columns rendered. Trailing unstyled blanks (a
// terminal's padding) are left out. With keepSpaces, trailing spaces that were
// printed stay and only the cells nothing was written to are dropped: a row
// continuing onto the next is joined to it by its spaces, and a painted screen
// must reproduce a prompt's trailing space, which a program can rely on.
func renderRow(dst []byte, cells []cell, keepSpaces bool) ([]byte, int) {
	end := len(cells)
	for end > 0 && cells[end-1].isPad() && (!keepSpaces || cells[end-1].text != " ") {
		end--
	}

	var (
		cur  []byte
		link string
	)

	for i := range end {
		c := &cells[i]
		if c.width == 0 {
			continue // a wide character's right half: its left half drew it
		}

		if c.link != link {
			if link != "" {
				dst = append(dst, linkClose...)
			}

			if c.link != "" {
				dst = openLink(dst, c.link)
			}

			link = c.link
		}

		if !bytes.Equal(c.style, cur) {
			if len(cur) > 0 {
				dst = append(dst, sgrReset...)
			}

			dst = append(dst, c.style...)
			cur = c.style
		}

		if c.text == "" {
			dst = append(dst, ' ')
		} else {
			dst = append(dst, c.text...)
		}
	}

	if len(cur) > 0 {
		dst = append(dst, sgrReset...)
	}

	if link != "" {
		dst = append(dst, linkClose...)
	}

	return dst, end
}

// linkClose ends an OSC 8 hyperlink. Each rendered row closes its own, so a
// link never runs on past its row in a pager or a replay.
var linkClose = []byte("\x1b]8;;\x1b\\")

// openLink appends the OSC 8 sequence opening link ("params;uri").
func openLink(dst []byte, link string) []byte {
	dst = append(dst, "\x1b]8;"...)
	dst = append(dst, link...)

	return append(dst, "\x1b\\"...)
}

// snapshot renders the main screen as history text — what would be recorded if
// it all scrolled off now — down to its last row with text. [history] shows it
// after the log, so the pager ends on what the session's screen shows.
func (s *screen) snapshot() []byte {
	var b []byte

	last := lastTextRow(s.main)
	for y := 0; y <= last; y++ {
		r := &s.main[y]
		b, _ = renderRow(b, r.cells, r.wrapped)

		if !r.wrapped || y == last {
			b = append(b, '\n')
		}
	}

	return b
}

// paint returns the escape stream that reproduces the screen on a client
// terminal of the same size that has just been soft-reset (see softReset in
// daemon.go), with whatever it showed before scrolled up out of the way.
//
// The main screen is printed row by row, top to bottom — each row cleared past
// its text, so nothing the client showed before survives — which leaves the
// client showing exactly these rows. When the session is inside a full-screen
// app, the client's cursor is first put where the main screen's cursor was
// saved, so the alt-screen switch saves the right position for the app's exit;
// the mode restore then re-enters the alt screen, whose rows are painted in
// place. Last come the scroll region, the saved cursor, the cursor itself and
// the active style, so the program's next output — a relative cursor move, a
// colored word — lands as it would have on the terminal it was writing to.
func (s *screen) paint() []byte {
	b := s.paintRows(nil, s.main, false)

	if s.onAlt {
		b = cup(b, s.altSaved.x, s.altSaved.y)
	}

	b = append(b, s.modes.restore()...)

	if s.onAlt {
		b = s.paintRows(b, s.alt, true)
	}

	if s.top != 0 || s.bot != s.rows-1 {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(s.top+1), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(s.bot+1), 10)
		b = append(b, 'r')
	}

	if s.saved.set {
		b = cup(b, s.saved.x, s.saved.y)
		b = append(b, s.saved.style...)
		b = append(b, "\x1b7\x1b[0m"...)
	}

	if s.insert {
		b = append(b, "\x1b[4h"...)
	}

	y := s.y
	if s.origin {
		b = append(b, "\x1b[?6h"...)
		y -= s.top
	}

	b = cup(b, s.x, y)

	if s.decGfx {
		b = append(b, "\x1b(0"...)
	}

	if s.link != "" {
		b = openLink(b, s.link) // the program is mid-link: its next text belongs to it
	}

	return append(b, s.style...)
}

// paintRows prints buffer g's rows, each cleared past its text. Main-screen rows
// are separated by CRLF so they scroll whatever the client showed before up out
// of view; alt-screen rows are addressed in place, since the alt screen has
// nothing to scroll into.
func (s *screen) paintRows(b []byte, g []row, alt bool) []byte {
	for y := range g {
		switch {
		case alt:
			b = cup(b, 0, y)
		case y > 0:
			b = append(b, "\r\n"...)
		}

		var n int

		b, n = renderRow(b, g[y].cells, true)
		if n < s.cols {
			b = append(b, "\x1b[K"...) // erase the rest; not at the edge, where it would eat the last column
		}
	}

	return b
}

// cup appends a cursor-position sequence for 0-based (x, y).
func cup(b []byte, x, y int) []byte {
	b = append(b, "\x1b["...)
	b = strconv.AppendInt(b, int64(y+1), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(x+1), 10)

	return append(b, 'H')
}
