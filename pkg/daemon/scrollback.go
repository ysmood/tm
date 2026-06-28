package daemon

import (
	"os"
	"slices"
	"sync"

	"github.com/ysmood/tm/pkg/proto"
)

// DefaultRingBytes is the in-memory scrollback kept for "page"/"N lines" replay.
const DefaultRingBytes = 1 << 20 // 1 MiB

// Scrollback records raw terminal output: a capped in-memory ring for recent
// output plus an append-only log file for full ("all") history.
type Scrollback struct {
	mu       sync.Mutex
	ring     []byte
	maxBytes int
	log      *os.File
}

// NewScrollback creates a scrollback keeping up to max bytes in memory and
// appending all output to logPath (logPath may be empty to skip the log file).
func NewScrollback(maxBytes int, logPath string) (*Scrollback, error) {
	s := &Scrollback{maxBytes: maxBytes}

	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}

		s.log = f
	}

	return s, nil
}

// Write records a chunk of raw output.
func (s *Scrollback) Write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.log != nil {
		_, _ = s.log.Write(p)
	}

	s.ring = append(s.ring, p...)
	if len(s.ring) > s.maxBytes {
		trimmed := make([]byte, s.maxBytes)
		copy(trimmed, s.ring[len(s.ring)-s.maxBytes:])
		s.ring = trimmed
	}
}

// History returns the bytes to replay for the requested mode. For HistAll it
// reads the full log file (falling back to the ring if there is no log); for
// HistPage/HistLines it returns the tail of the ring by line count.
func (s *Scrollback) History(mode proto.HistMode, lines, rows int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch mode {
	case proto.HistAll:
		if s.log != nil {
			if data, err := os.ReadFile(s.log.Name()); err == nil {
				return data
			}
		}

		return clone(s.ring)
	case proto.HistPage:
		if rows <= 0 {
			rows = 24
		}

		return clone(tailLines(s.ring, rows))
	case proto.HistLines:
		return clone(tailLines(s.ring, lines))
	default:
		return nil
	}
}

// Close closes the log file.
func (s *Scrollback) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.log != nil {
		err := s.log.Close()
		s.log = nil

		return err
	}

	return nil
}

// tailLines returns the suffix of b containing its last n lines. A line is
// delimited by '\n'; a single trailing newline is treated as terminating the
// last line rather than starting an empty one. This counts physical newlines,
// not wrapped terminal rows — a deliberate simplification.
func tailLines(b []byte, n int) []byte {
	if n <= 0 || len(b) == 0 {
		return nil
	}

	scan := b
	if scan[len(scan)-1] == '\n' {
		scan = scan[:len(scan)-1]
	}

	count := 0

	for i, v := range slices.Backward(scan) {
		if v == '\n' {
			count++
			if count == n {
				return b[i+1:]
			}
		}
	}

	return b
}

func clone(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}

	out := make([]byte, len(b))
	copy(out, b)

	return out
}
