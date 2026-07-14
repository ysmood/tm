package daemon

import (
	"os"
	"slices"
	"sync"
)

// TailBytes bounds how much of the log file's end History reads to find the last
// window of output. A window is one screen of lines, so this only has to be
// comfortably more than a screenful of bytes — even a wide terminal's rows,
// escape sequences and all, fit many times over. Reading a bounded tail (rather
// than the whole file) keeps an attach cheap no matter how long the session has
// been running.
const TailBytes = 256 << 10 // 256 KiB

// Scrollback records raw terminal output to an append-only log file, which is
// the only place a session's history lives: nothing is buffered in memory, so an
// attach reads its replay straight from the file.
type Scrollback struct {
	mu  sync.Mutex
	log *os.File
}

// NewScrollback opens (creating it if needed) the log file the session appends
// its output to. It is opened for reading too, so History can read back the tail
// it just wrote.
func NewScrollback(logPath string) (*Scrollback, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}

	return &Scrollback{log: f}, nil
}

// Write records a chunk of raw output.
func (s *Scrollback) Write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.log != nil {
		_, _ = s.log.Write(p)
	}
}

// History returns the last window of recorded output to replay on attach: the
// final rows lines of the log file (see TailBytes for the read bound).
func (s *Scrollback) History(rows int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.log == nil || rows <= 0 {
		return nil
	}

	info, err := s.log.Stat()
	if err != nil {
		return nil
	}

	size := info.Size()
	off := max(size-TailBytes, 0)

	buf := make([]byte, size-off)
	if _, err := s.log.ReadAt(buf, off); err != nil {
		return nil
	}

	return tailLines(buf, rows)
}

// Clear discards the session's recorded history by truncating the log file in
// place. The file stays open — it was opened with O_APPEND, so later writes land
// at the new (zero) end — and the session keeps running; only its recorded past
// is dropped, so nothing of it (say, a secret echoed to the terminal) can be
// replayed on a later attach.
func (s *Scrollback) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.log != nil {
		return s.log.Truncate(0)
	}

	return nil
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
