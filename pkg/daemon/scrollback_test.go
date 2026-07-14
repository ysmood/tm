package daemon_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/daemon"
)

func newSB(t *testing.T, g got.G) *daemon.Scrollback {
	sb, err := daemon.NewScrollback(filepath.Join(t.TempDir(), "log"))
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
// at most daemon.TailBytes back, so the window it returns comes from the end of
// the log, not the whole of it.
func TestHistoryReadsBoundedTail(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g)

	// One huge line (no newlines) longer than the tail bound, then a short line.
	sb.Write(bytes.Repeat([]byte("x"), daemon.TailBytes+1000))
	sb.Write([]byte("\ntail\n"))

	hist := sb.History(24)
	g.True(bytes.HasSuffix(hist, []byte("\ntail\n")))
	g.Lte(len(hist), daemon.TailBytes)
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
