package daemon

import (
	"strings"
	"testing"

	"github.com/ysmood/got"
)

// cook a whole input in one shot and return the completed lines plus the
// in-progress tail joined, which is the full visible form of the stream.
func cookAll(in string) string {
	var c cooker

	out := c.cook([]byte(in))

	return string(out) + string(c.tail())
}

func TestCookVisibleForm(t *testing.T) {
	g := got.T(t)

	check := func(in, want string) {
		g.Helper()
		g.Eq(cookAll(in), want)
	}

	// Plain text and SGR color pass through; a styled line closes its own style.
	check("hello", "hello")
	check("\x1b[31mred\x1b[0m", "\x1b[31mred\x1b[0m")
	check("a\x1b[31mred\x1b[0mb", "a\x1b[31mred\x1b[0mb")

	// CRLF line endings lose the carriage return — the source of the ^M in the
	// pager. A bare LF is kept as the line terminator.
	check("a\r\nb\r\n", "a\nb\n")
	check("a\nb", "a\nb")

	// A carriage return returns to column 0; later text overwrites in place, so a
	// redrawn line records only its final form (progress bars, prompt redraws).
	check("aaaa\rbb", "bbaa")
	check("12345\rabc", "abc45")
	// A full-width redraw (progress bar) leaves only the last frame.
	check("[   ]\r[## ]\r[###]", "[###]")

	// zsh's end-of-line marker: a "%", spaces to fill the row, a carriage return,
	// then the next prompt overwrites from column 0 — the "%" is gone and the pad
	// spaces are trimmed.
	check("%     \r$ ", "$")
	// With no following prompt (end of output) the marker itself is what shows.
	check("%     ", "%")

	// Backspace moves the cursor back a column; the next write overwrites there.
	check("abc\b\bXY", "aXY")
	check("ab\b", "ab") // a bare backspace just moves the cursor

	// Non-SGR escapes are dropped: cursor motion, erases, and reply-soliciting
	// queries all vanish, leaving only the text around them.
	check("\x1b[2J\x1b[Hhi", "hi")      // clear screen + cursor home
	check("a\x1b[Kb", "ab")             // erase-to-end-of-line
	check("\x1b[6nprompt$ ", "prompt$") // DSR cursor-position query dropped
	check("x\x1b[18t\x1b[>qy", "xy")    // XTWINOPS + XTVERSION probes dropped
	check("t\x1b]0;title\x07u", "tu")   // OSC title set dropped (not visible text)
	check("a\x1b]11;?\x07b", "ab")      // OSC color query dropped
	check("a\x1bP+q544e\x1b\\b", "ab")  // DCS XTGETTCAP dropped

	// Other control bytes are dropped: bell, form feed, DEL.
	check("a\x07b\x0cc\x7fd", "abcd")

	// Tabs are kept as visible whitespace.
	check("a\tb", "a\tb")

	// Trailing spaces on a line are dropped (terminal padding); interior spaces
	// stay.
	check("a b   \n", "a b\n")
}

// SGR attributes accumulate and a reset clears them, so re-emitting the stored
// style reproduces the same appearance after an in-line overwrite.
func TestCookStyleAccumulates(t *testing.T) {
	g := got.T(t)

	// Bold then red, both active on the text.
	g.Eq(cookAll("\x1b[1m\x1b[31mx"), "\x1b[1m\x1b[31mx\x1b[0m")

	// A reset drops the accumulated attributes; text after it is unstyled.
	g.Eq(cookAll("\x1b[1mA\x1b[0mB"), "\x1b[1mA\x1b[0mB")

	// "\x1b[m" is a bare reset, same as "\x1b[0m".
	g.Eq(cookAll("\x1b[1mA\x1b[mB"), "\x1b[1mA\x1b[0mB")

	// A reset mixed with new attributes replaces the style in one step.
	g.Eq(cookAll("\x1b[31mA\x1b[0;1mB"), "\x1b[31mA\x1b[0m\x1b[0;1mB\x1b[0m")
}

// The cooker is stateful across calls: a sequence, a multi-byte rune, or a line
// split across chunk boundaries is reassembled and produces the same result as
// if it arrived whole.
func TestCookAcrossChunks(t *testing.T) {
	g := got.T(t)

	feed := func(chunks ...string) string {
		var c cooker

		var b strings.Builder
		for _, ch := range chunks {
			b.Write(c.cook([]byte(ch)))
		}

		b.Write(c.tail())

		return b.String()
	}

	// An SGR sequence split mid-way is held and completed on the next chunk.
	g.Eq(feed("\x1b[3", "1mred\x1b[0m"), "\x1b[31mred\x1b[0m")

	// A line split across chunks: only completed lines come out per call, the
	// rest is the tail.
	g.Eq(feed("foo", "bar\nbaz"), "foobar\nbaz")

	// A multi-byte rune (é = 0xc3 0xa9) split across chunks is reassembled.
	g.Eq(feed("caf\xc3", "\xa9\n"), "café\n")
}

// Feeding a stream one byte at a time exercises every split point — every
// sequence and multi-byte rune straddles a boundary — and must produce the same
// result as feeding it whole, proving the held/joined buffer reuse never
// corrupts a straddling sequence.
func TestCookBytewiseMatchesWhole(t *testing.T) {
	g := got.T(t)

	in := "plain \x1b[1;31mbold-red\x1b[0m café\r\n" +
		"loading\rdone\x1b[K\n" +
		"prompt$ \x1b[6n\x1b]0;title\x07 tab\there\n" +
		"%     \r$ last"

	var whole cooker
	want := string(whole.cook([]byte(in))) + string(whole.tail())

	var oneByte cooker

	var got strings.Builder
	for i := range len(in) {
		got.Write(oneByte.cook([]byte(in[i : i+1])))
	}

	got.WriteString(string(oneByte.tail()))

	g.Eq(got.String(), want)
}

// Output with no newline must not grow the in-progress line without bound: past
// maxLineWidth the line soft-wraps, so cells stays capped while every character
// is still recorded.
func TestCookBoundsLineWithoutNewline(t *testing.T) {
	g := got.T(t)

	const n = maxLineWidth*2 + 100

	var c cooker

	var out strings.Builder
	// Feed in chunks the size of a real PTY read, never a newline.
	for fed := 0; fed < n; fed += 4096 {
		size := min(4096, n-fed)
		out.Write(c.cook([]byte(strings.Repeat("x", size))))

		g.Desc("cells must stay bounded while no newline arrives").
			Lte(len(c.cells), maxLineWidth)
	}

	out.WriteString(string(c.tail()))

	// Every character is preserved; the buffer was bounded only by inserting
	// soft-wrap newlines (two, for two full widths).
	g.Eq(strings.Count(out.String(), "x"), n)
	g.Eq(strings.Count(out.String(), "\n"), n/maxLineWidth)
}

// An unterminated escape sequence (no ST/BEL, as in a malformed or binary
// stream) must not grow the held buffer without bound.
func TestCookBoundsUnterminatedSequence(t *testing.T) {
	g := got.T(t)

	var c cooker

	// An OSC that never terminates, fed forever in PTY-sized chunks.
	c.cook([]byte("\x1b]0;"))

	for range 8 {
		c.cook([]byte(strings.Repeat("A", 32*1024)))

		g.Desc("held must stay bounded for an unterminated sequence").
			Lte(len(c.held), maxHeldBytes)
	}
}

// cook returns a buffer it reuses, so callers must consume it before the next
// call; within one run the returned lines are the completed ones and are stable
// until then.
func BenchmarkCook(b *testing.B) {
	// A chunk of typical shell output: a colored prompt, a command echo, some
	// plain output lines, and a CRLF-terminated tail.
	chunk := []byte("\x1b[32muser@host\x1b[0m:\x1b[34m~/src\x1b[0m$ ls -la\r\n" +
		"total 48\r\ndrwxr-xr-x  6 user staff  192 Jan  1 00:00 .\r\n" +
		"-rw-r--r--  1 user staff 1024 Jan  1 00:00 README.md\r\n")

	var c cooker

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = c.cook(chunk)
	}
}

// cook returns completed lines only; the in-progress line is held back until a
// newline settles it, so an overwrite can still revise it before it is recorded.
func TestCookReturnsCompletedLinesOnly(t *testing.T) {
	g := got.T(t)

	var c cooker

	g.Eq(string(c.cook([]byte("done\nin-progress"))), "done\n")
	g.Eq(string(c.tail()), "in-progress")

	// The newline that finishes it flushes the (now overwritten) line.
	g.Eq(string(c.cook([]byte("\rredrawn\n"))), "redrawnress\n")
	g.Eq(string(c.tail()), "")
}
