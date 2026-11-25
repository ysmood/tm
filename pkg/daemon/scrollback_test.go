package daemon

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ysmood/got"
)

func newSB(t *testing.T, g got.G, rows int) *scrollback {
	sb, err := newScrollback(filepath.Join(t.TempDir(), "log"), 80, rows)
	g.E(err)
	g.Cleanup(func() { _ = sb.Close() })

	return sb
}

// record appends history straight to the log, as lines scrolling off the screen
// would, for tests of the log itself.
func record(sb *scrollback, hist string) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	sb.record([]byte(hist))
}

// Only what scrolls off the screen reaches the log; the screen holds the rest,
// which Snapshot renders for [history].
func TestWriteRecordsScrolledLines(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 2)

	sb.Write([]byte("a\r\nb\r\nc\r\n"))
	g.Eq(readLog(g, sb), "a\nb\n")
	g.Eq(string(sb.Snapshot()), "c\n")
	g.False(sb.Blank())
}

// Rows a shrink pushes off the top are recorded like any that scroll off.
func TestResizeRecordsPushedRows(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 3)

	sb.Write([]byte("1\r\n2\r\n3"))
	sb.Resize(80, 1)
	g.Eq(readLog(g, sb), "1\n2\n")
	g.Eq(string(sb.Snapshot()), "3\n")
}

func TestHistoryWindow(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 24)
	record(sb, "l1\nl2\nl3\nl4\n")

	// A trailing newline terminates the last line rather than starting another.
	g.Eq(string(sb.History(2)), "l3\nl4\n")
	g.Eq(string(sb.History(100)), "l1\nl2\nl3\nl4\n")
	g.Len(sb.History(0), 0)
}

// A session that has run for a long time only pays for the tail: History reads
// at most tailBytes back, so the lines it returns come from the end of the log,
// not the whole of it.
func TestHistoryReadsBoundedTail(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 24)

	// One huge line longer than the tail bound, then a short line.
	record(sb, strings.Repeat("x", tailBytes+1000)+"\ntail\n")

	hist := sb.History(24)
	g.True(bytes.HasSuffix(hist, []byte("\ntail\n")))
	g.Lte(len(hist), tailBytes)
}

// Clear wipes the log and blanks the screen, so neither [history] nor a replay
// can show what came before — but not the terminal state a still-running
// full-screen app is in. Output after it is recorded from a clean slate (the
// O_APPEND handle writes at the truncated file's end).
func TestClear(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 2)

	sb.Write([]byte("old\r\nsecret\r\non screen"))
	g.Eq(readLog(g, sb), "old\n")

	g.E(sb.Clear())
	g.Eq(readLog(g, sb), "")
	g.True(sb.Blank())
	g.Len(sb.Snapshot(), 0)
	g.False(strings.Contains(string(sb.Paint()), "secret"))

	sb.Write([]byte("\r\nafter\r\n")) // two (wiped, blank) rows scroll off
	g.Eq(readLog(g, sb), "\n\n")
	g.Eq(string(sb.Snapshot()), "after\n")

	sb.Write([]byte("\x1b[?1049hPAINT"))
	g.E(sb.Clear())
	g.True(sb.Alt()) // the wipe cleared history, not the terminal's state
}

// readLog returns the log file's whole content, as a [history] pager sees it.
func readLog(g got.G, sb *scrollback) string {
	b, err := os.ReadFile(sb.path)
	g.E(err)

	return string(b)
}

// A log that outgrows its limit drops its oldest lines, keeping the newest whole
// lines that fit in half the limit; later output still lands at the end.
func TestLogTrimsToNewestHalf(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 24)
	sb.limit = 40

	for i := range 6 {
		record(sb, fmt.Sprintf("line-%d\n", i)) // 7 bytes each
	}

	// The 6th line took the log to 42 bytes: only the newest lines lying wholly
	// in its last 20 bytes survive (line-3 straddles the cut, so it goes too).
	g.Eq(readLog(g, sb), "line-4\nline-5\n")
	g.Eq(sb.size, int64(14))

	record(sb, "line-6\n")
	g.Eq(readLog(g, sb), "line-4\nline-5\nline-6\n")
	g.Eq(string(sb.History(2)), "line-5\nline-6\n")
}

// The cut always lands on a line start, so the log never opens with a torn line
// (or a torn escape sequence or rune within it).
func TestLogTrimCutsAtLineStart(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 24)
	sb.limit = 20

	record(sb, "aaaaaaaaaa\nbbbbbbbbbb\nc\n") // 25 bytes; the newest 10 start mid-b

	g.Eq(readLog(g, sb), "c\n")
}

// A line longer than half the limit cannot be kept whole, so it is dropped
// rather than kept torn.
func TestLogTrimDropsOversizedLine(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 24)
	sb.limit = 20

	record(sb, strings.Repeat("x", 30)+"\n")
	g.Eq(readLog(g, sb), "")
	g.Eq(sb.size, int64(0))

	record(sb, "next\n")
	g.Eq(readLog(g, sb), "next\n")
}

// A daemon opening a log already past the limit (left by an older tm) picks up
// its size, so the first write trims it.
func TestLogTrimsExistingOversizedLog(t *testing.T) {
	g := got.T(t)
	p := filepath.Join(t.TempDir(), "log")
	g.E(os.WriteFile(p, []byte(strings.Repeat("old\n", 10)), 0o600))

	sb, err := newScrollback(p, 80, 24)
	g.E(err)
	g.Cleanup(func() { _ = sb.Close() })

	g.Eq(sb.size, int64(40))

	sb.limit = 20
	record(sb, "new\n")
	g.Eq(readLog(g, sb), "old\nnew\n")
}

// Clear resets the tracked size along with the file, so the limit is measured
// from the wipe.
func TestClearResetsLogSize(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 24)
	record(sb, "secret\n")

	g.E(sb.Clear())
	g.Eq(sb.size, int64(0))
}

// The in-place copy spans many chunks when the kept half is large, each one
// writing over bytes already read; the result must be exactly the newest lines.
func TestLogTrimCopiesManyChunks(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, 24)
	sb.limit = 20 * trimBufSize

	var all []byte

	for i := 0; int64(len(all)) <= sb.limit; i++ {
		line := fmt.Appendf(nil, "\x1b[31mline %07d\x1b[0m\n", i)
		all = append(all, line...)
		record(sb, string(line))
	}

	log := readLog(g, sb)
	g.Lte(int64(len(log)), sb.limit/2)
	g.Gt(len(log), 0)
	g.True(strings.HasSuffix(string(all), log))
	g.True(bytes.Contains(all, []byte("\n"+log))) // kept from a line start
}
