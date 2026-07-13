package daemon_test

import (
	"path/filepath"
	"testing"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/daemon"
	"github.com/ysmood/tm/pkg/proto"
)

func newSB(t *testing.T, g got.G, maxBytes int) *daemon.Scrollback {
	sb, err := daemon.NewScrollback(maxBytes, filepath.Join(t.TempDir(), "log"))
	g.E(err)
	g.Cleanup(func() { _ = sb.Close() })

	return sb
}

func TestHistoryLines(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, daemon.DefaultRingBytes)
	sb.Write([]byte("l1\nl2\nl3\nl4"))

	g.Eq(string(sb.History(proto.HistLines, 2, 0)), "l3\nl4")
	g.Eq(string(sb.History(proto.HistLines, 100, 0)), "l1\nl2\nl3\nl4")
}

func TestHistoryLinesTrailingNewline(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, daemon.DefaultRingBytes)
	sb.Write([]byte("a\nb\n"))

	// A trailing newline terminates "b"; last 2 lines is the whole buffer.
	g.Eq(string(sb.History(proto.HistLines, 2, 0)), "a\nb\n")
	g.Eq(string(sb.History(proto.HistLines, 1, 0)), "b\n")
}

func TestHistoryPageUsesRows(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, daemon.DefaultRingBytes)
	sb.Write([]byte("r1\nr2\nr3"))

	g.Eq(string(sb.History(proto.HistPage, 0, 2)), "r2\nr3")
}

func TestHistoryNone(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, daemon.DefaultRingBytes)
	sb.Write([]byte("anything"))
	g.Len(sb.History(proto.HistNone, 0, 0), 0)
}

// Clear drops both sides of the scrollback — the ring and the log file — so no
// history mode can replay what came before; output after the clear is recorded
// from a clean slate (the O_APPEND handle writes at the truncated file's end).
func TestClear(t *testing.T) {
	g := got.T(t)
	sb := newSB(t, g, daemon.DefaultRingBytes)
	sb.Write([]byte("secret\n"))

	g.E(sb.Clear())
	g.Len(sb.History(proto.HistAll, 0, 0), 0)
	g.Len(sb.History(proto.HistLines, 100, 0), 0)
	g.Len(sb.History(proto.HistPage, 0, 24), 0)

	sb.Write([]byte("after\n"))
	g.Eq(string(sb.History(proto.HistAll, 0, 0)), "after\n")
	g.Eq(string(sb.History(proto.HistLines, 100, 0)), "after\n")
}

func TestHistoryAllReadsFullLog(t *testing.T) {
	g := got.T(t)
	// Small ring so memory is trimmed but the log keeps everything.
	sb := newSB(t, g, 4)
	sb.Write([]byte("0123456789"))

	g.Eq(string(sb.History(proto.HistAll, 0, 0)), "0123456789")
	// Ring kept only the last 4 bytes.
	g.Eq(string(sb.History(proto.HistLines, 100, 0)), "6789")
}
