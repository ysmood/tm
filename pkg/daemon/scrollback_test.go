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

func newSB(t *testing.T, g got.G) *scrollback {
	sb, err := newScrollback(filepath.Join(t.TempDir(), "log"))
	g.E(err)
	g.Cleanup(func() { _ = sb.Close() })

	return sb
}

func TestHistoryWindow(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)
	sb.Write([]byte("l1\nl2\nl3\nl4"))

	g.Eq(string(sb.History(2)), "l3\nl4")
	g.Eq(string(sb.History(100)), "l1\nl2\nl3\nl4")
}

func TestHistoryTrailingNewline(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)
	sb.Write([]byte("a\nb\n"))

	// A trailing newline terminates "b"; the last 2 lines are the whole log.
	g.Eq(string(sb.History(2)), "a\nb\n")
	g.Eq(string(sb.History(1)), "b\n")
}

// A window of no rows is no history at all — nothing to replay.
func TestHistoryNoRows(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)
	sb.Write([]byte("anything"))
	g.Len(sb.History(0), 0)
}

// The log file is the only source of truth, so the window survives across
// writes and is read back from disk rather than a memory buffer.
func TestHistoryReadsBackTheLog(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)
	sb.Write([]byte("first\n"))
	sb.Write([]byte("second\n"))

	g.Eq(string(sb.History(24)), "first\nsecond\n")
}

// A session that has run for a long time only pays for the tail: History reads
// at most tailBytes back, so the window it returns comes from the end of
// the log, not the whole of it.
func TestHistoryReadsBoundedTail(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)

	// One huge line (no newlines) longer than the tail bound, then a short line.
	sb.Write(bytes.Repeat([]byte("x"), tailBytes+1000))
	sb.Write([]byte("\ntail\n"))

	hist := sb.History(24)
	g.True(bytes.HasSuffix(hist, []byte("\ntail\n")))
	g.Lte(len(hist), tailBytes)
}

// TermState reflects the session's live terminal state — and survives a
// history wipe: clearing erases the recorded past, not the alt screen a
// still-running full-screen app is on.
func TestTermState(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)

	restore, alt := sb.TermState()
	g.False(alt)
	g.Len(restore, 0)

	sb.Write([]byte("$ lazygit\n\x1b[?1049h\x1b[?25lPAINT"))

	restore, alt = sb.TermState()
	g.True(alt)
	g.Has(string(restore), "\x1b[?1049h")
	g.Has(string(restore), "\x1b[?25l")

	g.E(sb.Clear())

	_, alt = sb.TermState()
	g.True(alt) // the wipe cleared the log, not the terminal's state

	sb.Write([]byte("\x1b[?25h\x1b[?1049l$ "))

	restore, alt = sb.TermState()
	g.False(alt)
	g.Len(restore, 0)
}

// Clear truncates the log, so no later replay can show what came before; output
// after the clear is recorded from a clean slate (the O_APPEND handle writes at
// the truncated file's end).
func TestClear(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)
	sb.Write([]byte("secret\n"))

	g.E(sb.Clear())
	g.Len(sb.History(24), 0)

	sb.Write([]byte("after\n"))
	g.Eq(string(sb.History(24)), "after\n")
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
	sb := newSB(t, g)
	sb.limit = 40

	for i := range 6 {
		sb.Write(fmt.Appendf(nil, "line-%d\n", i)) // 7 bytes each
	}

	// The 6th line took the log to 42 bytes: only the newest lines lying wholly
	// in its last 20 bytes survive (line-3 straddles the cut, so it goes too).
	g.Eq(readLog(g, sb), "line-4\nline-5\n")
	g.Eq(sb.size, int64(14))

	sb.Write([]byte("line-6\n"))
	g.Eq(readLog(g, sb), "line-4\nline-5\nline-6\n")
	g.Eq(string(sb.History(2)), "line-5\nline-6\n")
}

// The cut always lands on a line start, so the log never opens with a torn line
// (or a torn escape sequence or rune within it).
func TestLogTrimCutsAtLineStart(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)
	sb.limit = 20

	sb.Write([]byte("aaaaaaaaaa\nbbbbbbbbbb\nc\n")) // 25 bytes; the newest 10 start mid-b

	g.Eq(readLog(g, sb), "c\n")
}

// A line longer than half the limit cannot be kept whole, so it is dropped
// rather than kept torn.
func TestLogTrimDropsOversizedLine(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)
	sb.limit = 20

	sb.Write([]byte(strings.Repeat("x", 30) + "\n"))
	g.Eq(readLog(g, sb), "")
	g.Eq(sb.size, int64(0))

	sb.Write([]byte("next\n"))
	g.Eq(readLog(g, sb), "next\n")
}

// A daemon opening a log already past the limit (left by an older tm) picks up
// its size, so the first write trims it.
func TestLogTrimsExistingOversizedLog(t *testing.T) {
	g := got.T(t)
	p := filepath.Join(t.TempDir(), "log")
	g.E(os.WriteFile(p, []byte(strings.Repeat("old\n", 10)), 0o600))

	sb, err := newScrollback(p)
	g.E(err)
	g.Cleanup(func() { _ = sb.Close() })

	g.Eq(sb.size, int64(40))

	sb.limit = 20
	sb.Write([]byte("new\n"))
	g.Eq(readLog(g, sb), "old\nnew\n")
}

// Clear resets the tracked size along with the file, so the limit is measured
// from the wipe.
func TestClearResetsLogSize(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)
	sb.Write([]byte("secret\n"))

	g.E(sb.Clear())
	g.Eq(sb.size, int64(0))
}

// The in-place copy spans many chunks when the kept half is large, each one
// writing over bytes already read; the result must be exactly the newest lines.
func TestLogTrimCopiesManyChunks(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)
	sb.limit = 20 * trimBufSize

	var all []byte

	for i := 0; int64(len(all)) <= sb.limit; i++ {
		line := fmt.Appendf(nil, "\x1b[31mline %07d\x1b[0m\n", i)
		all = append(all, line...)
		sb.Write(line)
	}

	log := readLog(g, sb)
	g.Lte(int64(len(log)), sb.limit/2)
	g.Gt(len(log), 0)
	g.True(strings.HasSuffix(string(all), log))
	g.True(bytes.Contains(all, []byte("\n"+log))) // kept from a line start
}
