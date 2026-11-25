package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ysmood/got"
)

// feed writes chunks to a fresh cols×rows screen, returning it and the history
// the chunks produced.
func feed(cols, rows int, chunks ...string) (*screen, string) {
	s := newScreen(cols, rows)

	var hist strings.Builder
	for _, c := range chunks {
		hist.Write(s.write([]byte(c)))
	}

	return s, hist.String()
}

// shown renders the rows on display, each without its padding.
func (s *screen) shown() []string {
	out := make([]string, len(s.grid))
	for y := range s.grid {
		b, _ := renderRow(nil, s.grid[y].cells, false)
		out[y] = string(b)
	}

	return out
}

// Lines become history only when they scroll off the top; what is still on
// screen is not history yet.
func TestScreenRecordsWhatScrollsOff(t *testing.T) {
	g := got.T(t)

	s, hist := feed(10, 3, "a\r\nb\r\nc\r\nd\r\n")
	g.Eq(hist, "a\nb\n")
	g.Eq(s.shown(), []string{"c", "d", ""})
	g.Eq([]int{s.x, s.y}, []int{0, 2})
}

// An inline TUI (Ink: Claude Code) redraws its block by erasing its lines and
// moving the cursor up, many times a second. That changes the screen but must
// record nothing — the bloat the line-based recorder suffered — even when the
// block sits at the bottom of a full screen.
func TestScreenInlineRedrawRecordsNothing(t *testing.T) {
	g := got.T(t)

	frame := func(i int) string {
		return fmt.Sprintf("\x1b[2K\x1b[1A\x1b[2K\x1b[1A\x1b[2K\x1b[G\x1b[36m⠋ spin %d\x1b[39m\r\nstatus\r\n> input", i)
	}

	s := newScreen(20, 5)

	var hist strings.Builder

	for i := range 10 {
		hist.Write(s.write(fmt.Appendf(nil, "l%d\r\n", i)))
	}

	hist.Write(s.write([]byte("⠋ spin 0\r\nstatus\r\n> input")))
	before := hist.String()
	g.Eq(before, "l0\nl1\nl2\nl3\nl4\nl5\nl6\nl7\n")

	for i := 1; i <= 1000; i++ {
		hist.Write(s.write([]byte(frame(i))))
	}

	g.Eq(hist.String(), before)
	g.Eq(s.shown(), []string{"l8", "l9", "\x1b[36m⠋ spin 1000\x1b[0m", "status", "> input"})
}

// A carriage-return progress bar redraws one row in place: nothing recorded
// until the line is done and scrolls off.
func TestScreenProgressBar(t *testing.T) {
	g := got.T(t)

	var in strings.Builder
	for i := range 101 {
		fmt.Fprintf(&in, "\r[%-10s] %3d%%", strings.Repeat("#", i/10), i)
	}

	s, hist := feed(20, 2, in.String())
	g.Eq(hist, "")
	g.Eq(s.shown()[0], "[##########] 100%")

	_, hist = feed(20, 2, in.String(), "\r\n\r\n")
	g.Eq(hist, "[##########] 100%\n")
}

// A line longer than the screen is wide wraps onto several rows; recorded, they
// are one line again.
func TestScreenWrappedLineRecordsAsOne(t *testing.T) {
	g := got.T(t)

	s, hist := feed(5, 2, "abcdefghij\r\nk\r\n")
	g.Eq(hist, "abcdefghij\n")
	g.Eq(s.shown(), []string{"k", ""})

	// Text that exactly fills a row and is followed by a newline did not wrap.
	_, hist = feed(5, 2, "abcde\r\nf\r\ng\r\n")
	g.Eq(hist, "abcde\nf\n")
}

// Clearing the screen (Ctrl-L, `clear`) keeps what it showed as history, and
// `clear`'s erase-scrollback (ED 3) doesn't drop any of it.
func TestScreenClearKeepsHistory(t *testing.T) {
	g := got.T(t)

	s, hist := feed(10, 5, "one\r\ntwo\r\n$ ", "\x1b[3J\x1b[H\x1b[2J", "$ ")
	g.Eq(hist, "one\ntwo\n$\n")
	g.Eq(s.shown()[0], "$")

	// A clear from the home position (ED 0 at 0,0) is a whole-screen clear too;
	// a partial clear is not history.
	_, hist = feed(10, 5, "one\r\n", "\x1b[H\x1b[J")
	g.Eq(hist, "one\n")

	_, hist = feed(10, 5, "one\r\ntwo", "\x1b[1;2H\x1b[J")
	g.Eq(hist, "")

	// Clearing a blank screen records nothing.
	_, hist = feed(10, 5, "\x1b[2J\x1b[2J")
	g.Eq(hist, "")
}

// A full-screen app's alt screen is never history, and leaving it brings the
// main screen and cursor back as they were.
func TestScreenAltScreenNotRecorded(t *testing.T) {
	g := got.T(t)

	paint := strings.Repeat("PAINT\r\n", 50)
	s, hist := feed(10, 3, "$ vim\r\n", "\x1b[?1049h"+paint+"\x1b[2J", "\x1b[?1049l")
	g.Eq(hist, "")
	g.Eq(s.shown(), []string{"$ vim", "", ""})
	g.Eq([]int{s.x, s.y}, []int{0, 1})
	g.False(s.onAlt)
}

// Rows scrolling inside a region that doesn't start at the top never left the
// screen, so they are not history; a region anchored at the top feeds history.
func TestScreenScrollRegion(t *testing.T) {
	g := got.T(t)

	s, hist := feed(10, 5, "top\x1b[2;4r\x1b[4;1Ha\r\nb\r\nc\r\n")
	g.Eq(hist, "")
	g.Eq(s.shown(), []string{"top", "b", "c", "", ""})

	// Scrolling from a blank top records its blank rows too, as a terminal's
	// scrollback would.
	s, hist = feed(10, 5, "\x1b[1;3r\x1b[3;1Ha\r\nb\r\nc\r\n", "\x1b[5;1Hbottom")
	g.Eq(hist, "\n\na\n")
	g.Eq(s.shown(), []string{"b", "c", "", "", "bottom"})

	// IL/DL shift rows within the region; nothing they push out is history.
	s, hist = feed(10, 4, "1\r\n2\r\n3\r\n4", "\x1b[2;1H\x1b[2L")
	g.Eq(hist, "")
	g.Eq(s.shown(), []string{"1", "", "", "2"})

	s, _ = feed(10, 4, "1\r\n2\r\n3\r\n4", "\x1b[2;1H\x1b[M")
	g.Eq(s.shown(), []string{"1", "3", "4", ""})

	// RI at the top scrolls down.
	s, _ = feed(10, 3, "1\r\n2\r\n3", "\x1b[H\x1bM")
	g.Eq(s.shown(), []string{"", "1", "2"})
}

// A wide character takes two cells; overwriting either half blanks the other,
// and one that doesn't fit in the last column wraps whole.
func TestScreenWideCharacters(t *testing.T) {
	g := got.T(t)

	s, _ := feed(5, 2, "ＡＢ")
	g.Eq(s.shown()[0], "ＡＢ")
	g.Eq(s.x, 4)

	s, _ = feed(5, 2, "ＡＢ\ra")
	g.Eq(s.shown()[0], "a Ｂ")

	s, _ = feed(5, 2, "ＡＢ\x1b[2Gb")
	g.Eq(s.shown()[0], " bＢ")

	s, hist := feed(3, 2, "abＡ\r\n\r\n")
	g.Eq(hist, "abＡ\n")
	g.Eq(s.shown(), []string{"", ""})
}

// Combining marks and emoji sequences stay one grapheme per cell, even split
// across chunks — mid-rune or between a base and its mark.
func TestScreenGraphemes(t *testing.T) {
	g := got.T(t)

	s, _ := feed(10, 2, "éx")
	g.Eq(s.shown()[0], "éx")
	g.Eq(s.x, 2)

	s, _ = feed(10, 2, "e", "́x")
	g.Eq(s.shown()[0], "éx")

	s, _ = feed(10, 2, "\xef\xbc", "\xa1!")
	g.Eq(s.shown()[0], "Ａ!")

	s, _ = feed(10, 2, "👍🏽|")
	g.Eq(s.shown()[0], "👍🏽|")
	g.Eq(s.x, 3)
}

// The editing sequences shells and TUIs use, applied to one row.
func TestScreenEditing(t *testing.T) {
	g := got.T(t)

	row := func(in string) string {
		s, _ := feed(10, 2, in)

		return s.shown()[0]
	}

	g.Eq(row("abcdef\x1b[3G\x1b[2@"), "ab  cdef")   // ICH
	g.Eq(row("abcdef\x1b[3G\x1b[2P"), "abef")       // DCH
	g.Eq(row("abcdef\x1b[3G\x1b[2X"), "ab  ef")     // ECH
	g.Eq(row("abcdef\x1b[3G\x1b[K"), "ab")          // EL 0
	g.Eq(row("abcdef\x1b[3G\x1b[1K"), "   def")     // EL 1
	g.Eq(row("abcdef\x1b[2K"), "")                  // EL 2
	g.Eq(row("abcdef\x1b[3G\x1b[4hXY"), "abXYcdef") // insert mode
	g.Eq(row("ab\tc"), "ab      c")                 // tab stops
	g.Eq(row("abc\bX"), "abX")                      // backspace
	g.Eq(row("x\x1b[3b"), "xxxx")                   // REP
	g.Eq(row("\x1b(0lqqk\x1b(Bq"), "┌──┐q")         // DEC line drawing
	g.Eq(row("abc\x1b7\x1b[5GX\x1b8Y"), "abcYX")    // save/restore cursor
	g.Eq(row("\x1b[5Ca\x1b[3Db\x1b[Gc"), "c  b a")  // relative moves
}

// Autowrap off overwrites the last column instead of wrapping.
func TestScreenAutowrapOff(t *testing.T) {
	g := got.T(t)

	s, _ := feed(5, 2, "\x1b[?7labcdefg")
	g.Eq(s.shown(), []string{"abcdg", ""})
}

// Origin mode addresses rows from the top of the scroll region.
func TestScreenOriginMode(t *testing.T) {
	g := got.T(t)

	s, _ := feed(10, 5, "\x1b[2;4r\x1b[?6h\x1b[1;1Ha\x1b[9;1Hb")
	g.Eq(s.shown(), []string{"", "a", "", "b", ""})
}

// A full reset (RIS) keeps what the screen showed as history, like a clear.
func TestScreenFullReset(t *testing.T) {
	g := got.T(t)

	s, hist := feed(10, 3, "\x1b[31mabc\x1b[2;3r\x1bc", "x")
	g.Eq(hist, "\x1b[31mabc\x1b[0m\n")
	g.Eq(s.shown(), []string{"x", "", ""})
	g.Eq([]int{s.top, s.bot}, []int{0, 2})
}

// Shrinking drops blank rows from the bottom before scrolling rows off the top
// (into history), keeping the cursor's row on screen.
func TestScreenResize(t *testing.T) {
	g := got.T(t)

	s, _ := feed(10, 5, "a\r\nb")
	g.Eq(string(s.resize(10, 2)), "")
	g.Eq(s.shown(), []string{"a", "b"})

	s, _ = feed(10, 4, "a\r\nb\r\nc\r\nd")
	g.Eq(string(s.resize(10, 2)), "a\nb\n")
	g.Eq(s.shown(), []string{"c", "d"})
	g.Eq(s.y, 1)

	// A wide character that no longer fits re-wraps whole onto the next row,
	// and the cursor after it moves along.
	s, _ = feed(10, 2, "abcdefＡ")
	s.resize(7, 3)
	g.Eq(s.shown(), []string{"abcdef", "Ａ", ""})
	g.Eq([]int{s.x, s.y}, []int{2, 1})

	// The alt screen's rows are never history, however the size changes.
	s, _ = feed(10, 4, "\x1b[?1049ha\r\nb\r\nc\r\nd")
	g.Eq(string(s.resize(10, 2)), "")
}

// paint reproduces the screen: fed to a fresh terminal of the same size, it
// yields the same rows, styles, cursor, scroll region and buffer — and, from a
// fresh terminal's home position, scrolls nothing into its history.
func TestScreenPaintRoundTrip(t *testing.T) {
	g := got.T(t)

	check := func(in ...string) {
		g.Helper()

		s, _ := feed(12, 4, in...)
		copied, hist := feed(12, 4, string(s.paint()))

		g.Eq(hist, "")
		g.Eq(copied.shown(), s.shown())
		g.Eq([]int{copied.x, copied.y, copied.top, copied.bot}, []int{s.x, s.y, s.top, s.bot})
		g.Eq(copied.onAlt, s.onAlt)
		g.Eq(copied.style, s.style)
		g.Eq(copied.link, s.link)
	}

	check("plain\r\n\x1b[1;31mred\x1b[0m and ＡＢ\r\n$ ")
	check("exactly12col\r\nnext") // a row filling the width must not lose its last column
	check("\x1b[44m\x1b[2K bar \x1b[0m\r\n")
	check("a\r\nb\r\n\x1b[2;3r\x1b[3;2H\x1b[32m")
	check("$ lazygit\r\n\x1b[?1049h\x1b[?25l\x1b[2;3HPANEL\x1b[4;1Hstatus")
	check("abc\x1b7\x1b[4;1H")
	check("go \x1b]8;;https://example.com\x1b\\here, still in the link")
}

// Restoring a full-screen app puts the client's cursor where the main screen's
// was saved before the alt-screen switch saves it, so the app's exit lands
// back on the shell's line.
func TestScreenPaintAltSavesMainCursor(t *testing.T) {
	g := got.T(t)

	s, _ := feed(12, 4, "$ vim\r\n\x1b[?1049hPAINT\x1b[3;3H")
	copied, _ := feed(12, 4, string(s.paint()), "\x1b[?1049l")
	g.Eq(copied.shown(), []string{"$ vim", "", "", ""})
	g.Eq([]int{copied.x, copied.y}, []int{0, 1})
}

// snapshot renders the main screen as history text: wrapped rows joined,
// trailing blank rows left out — and nothing of the alt screen.
func TestScreenSnapshot(t *testing.T) {
	g := got.T(t)

	s, _ := feed(5, 4, "abcdefg\r\nh")
	g.Eq(string(s.snapshot()), "abcdefg\nh\n")

	s, _ = feed(5, 4, "")
	g.Len(s.snapshot(), 0)

	s, _ = feed(5, 4, "$ top\r\n\x1b[?1049hPAINT")
	g.Eq(string(s.snapshot()), "$ top\n")
}

// A sequence split across chunks is reassembled; an unterminated one past the
// hold bound is dropped so it can't grow without limit.
func TestScreenSplitSequences(t *testing.T) {
	g := got.T(t)

	s, _ := feed(10, 2, "\x1b[3", "1mred\x1b", "[0m")
	g.Eq(s.shown()[0], "\x1b[31mred\x1b[0m")

	s, _ = feed(10, 2, "\x1b]0;title\x1b", "\\ok")
	g.Eq(s.shown()[0], "ok")

	s = newScreen(10, 2)
	s.write([]byte("\x1b]" + strings.Repeat("x", maxHeldBytes+1)))
	g.Len(s.held, 0)
}

// A width change re-wraps the main screen's logical lines, as a reflowing
// terminal does: narrowing and widening back restores the screen, the cursor
// riding along on its character.
func TestScreenReflow(t *testing.T) {
	g := got.T(t)

	s, _ := feed(10, 4, "0123456789abcde\r\nshort\r\n$ ")
	orig := s.shown()
	g.Eq(orig, []string{"0123456789", "abcde", "short", "$"})

	g.Eq(string(s.resize(5, 6)), "")
	g.Eq(s.shown(), []string{"01234", "56789", "abcde", "short", "$", ""})
	g.Eq([]int{s.x, s.y}, []int{2, 4})

	g.Eq(string(s.resize(10, 4)), "")
	g.Eq(s.shown(), orig)
	g.Eq([]int{s.x, s.y}, []int{2, 3})

	// A cursor waiting to wrap at the edge moves on past its character.
	s, _ = feed(5, 3, "abcde")
	s.resize(10, 3)
	s.write([]byte("f"))
	g.Eq(s.shown(), []string{"abcdef", "", ""})
}

// Rows a narrowing pushes off the top are history, and a logical line split by
// the push is still recorded as one line once the rest of it scrolls off.
func TestScreenReflowPushesHistory(t *testing.T) {
	g := got.T(t)

	s, _ := feed(10, 3, "0123456789abc\r\n$ ")

	hist := string(s.resize(5, 3))
	g.Eq(hist, "01234")
	g.Eq(s.shown(), []string{"56789", "abc", "$"})

	hist += string(s.write([]byte("\r\n\r\n\r\n")))
	g.Eq(hist, "0123456789abc\n$\n")
}

// The alt screen is cut, not reflowed — its app repaints for the new size —
// while the main screen beneath it reflows around the cursor the app's
// alt-screen switch saved.
func TestScreenReflowUnderAltScreen(t *testing.T) {
	g := got.T(t)

	s, _ := feed(10, 3, "0123456789abc\r\n$ \x1b[?1049h\x1b[H0123456789")
	s.resize(5, 5)
	g.Eq(s.shown(), []string{"01234", "", "", "", ""})

	s.write([]byte("\x1b[?1049l"))
	g.Eq(s.shown(), []string{"01234", "56789", "abc", "$", ""})
	g.Eq([]int{s.x, s.y}, []int{2, 3})
}

// OSC 8 hyperlinks stay on the text they were printed with: in history, on
// screen, across a wrap (each row closes its own), and through a repaint of a
// program still inside one. Other OSCs (a title) put nothing on screen, and an
// oversized link is dropped, its text kept.
func TestScreenHyperlinks(t *testing.T) {
	g := got.T(t)

	const (
		linkOpen = "\x1b]8;;https://example.com\x1b\\"
		linkEnd  = "\x1b]8;;\x1b\\"
	)

	_, hist := feed(20, 2, "see "+linkOpen+"docs"+linkEnd+" now\r\n", "\r\n")
	g.Eq(hist, "see "+linkOpen+"docs"+linkEnd+" now\n")

	s, _ := feed(5, 2, "\x1b]8;id=1;http://x\aabcdefg")
	g.Eq(s.shown(), []string{
		"\x1b]8;id=1;http://x\x1b\\abcde" + linkEnd,
		"\x1b]8;id=1;http://x\x1b\\fg" + linkEnd,
	})

	s.resize(10, 2)
	g.Eq(s.shown(), []string{"\x1b]8;id=1;http://x\x1b\\abcdefg" + linkEnd, ""})

	s, _ = feed(10, 1, "\x1b]0;title\ax")
	g.Eq(s.shown()[0], "x")

	s, _ = feed(10, 1, "\x1b]8;;"+strings.Repeat("u", maxLinkBytes)+"\x1b\\x")
	g.Eq(s.shown()[0], "x")
}
