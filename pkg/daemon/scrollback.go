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

// Scrollback records a session's output to an append-only log file, which is the
// only place its history lives. Raw PTY output is cooked to its visible form
// (see cooker) before being written, so the log holds clean text and color —
// what a pager shows for [history], and what an attach replays — rather than the
// raw control stream a terminal would act on. Only newline-terminated lines are
// written; the in-progress line is held in the cooker and surfaced by History.
type Scrollback struct {
	mu   sync.Mutex
	log  *os.File
	cook cooker
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

// Write cooks a chunk of raw output to its visible form and appends the lines it
// completes to the log; the unfinished last line stays in the cooker until a
// newline settles it (or History reads it as the live tail).
func (s *Scrollback) Write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.log != nil {
		_, _ = s.log.Write(s.cook.cook(p))
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

	// The file holds only completed lines; append the in-progress one (the live
	// prompt) so a replay shows it too.
	tail := s.cook.tail()
	window := make([]byte, 0, len(buf)+len(tail))
	window = append(window, buf...)
	window = append(window, tail...)

	return tailLines(window, rows)
}

// Clear discards the session's recorded history by truncating the log file in
// place. The file stays open — it was opened with O_APPEND, so later writes land
// at the new (zero) end — and the session keeps running; only its recorded past
// is dropped, so nothing of it (say, a secret echoed to the terminal) can be
// replayed on a later attach.
func (s *Scrollback) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cook.reset() // drop the in-progress line too, so nothing survives the wipe

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
