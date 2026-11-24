package daemon

import (
	"bufio"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
)

// tailBytes bounds how much of the log file's end History reads to find the last
// window of output. A window is one screen of lines, so this only has to be
// comfortably more than a screenful of bytes — even a wide terminal's rows,
// escape sequences and all, fit many times over. Reading a bounded tail (rather
// than the whole file) keeps an attach cheap no matter how long the session has
// been running.
const tailBytes = 256 << 10 // 256 KiB

// maxLogBytes caps a session's log file. The cooker already collapses most
// redraws to nothing (an alt-screen app records nothing, a carriage-return
// progress bar settles into one line), but a program that repaints on the main
// screen with cursor motion — an inline TUI, a `clear` loop — re-records its
// lines on every frame, and a session can run for weeks. Past the cap the log's
// oldest output is dropped (see trim), keeping its newest half, so the file
// stays between half and all of this size however much the session prints.
const maxLogBytes = 64 << 20 // 64 MiB

// trimBufSize is the chunk size trim copies the kept half of the log in.
const trimBufSize = 64 << 10 // 64 KiB

// scrollback records a session's output to an append-only log file, which is the
// only place its history lives. Raw PTY output is cooked to its visible form
// (see cooker) before being written, so the log holds clean text and color —
// what a pager shows for [history], and what an attach replays — rather than the
// raw control stream a terminal would act on. Only newline-terminated lines are
// written; the in-progress line is held in the cooker and surfaced by History.
type scrollback struct {
	mu    sync.Mutex
	path  string
	log   *os.File
	size  int64 // the log file's length, tracked so Write needn't stat per chunk
	limit int64 // the size past which the log is trimmed (maxLogBytes)
	cook  cooker
}

// newScrollback opens (creating it if needed) the log file the session appends
// its output to. It is opened for reading too, so History can read back the tail
// it just wrote.
func newScrollback(logPath string) (*scrollback, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, err
	}

	return &scrollback{path: logPath, log: f, size: info.Size(), limit: maxLogBytes}, nil
}

// Write cooks a chunk of raw output to its visible form and appends the lines it
// completes to the log; the unfinished last line stays in the cooker until a
// newline settles it (or History reads it as the live tail). A write that takes
// the log past its limit trims it (see trim).
func (s *scrollback) Write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.log == nil {
		return
	}

	n, _ := s.log.Write(s.cook.cook(p))
	s.size += int64(n)

	if s.size > s.limit {
		s.trim()
	}
}

// trim drops the log's oldest output, keeping the newest whole lines that fit in
// half the limit — so trimming is rare (once per half-limit of output) and its
// copying amortizes to about one extra write per byte recorded. The cut lands on
// a line start, so the log never opens with a torn line, escape or rune.
//
// It works in place: the kept tail is copied to the front of the same file
// through a second, non-append handle, then the file is truncated to it, and the
// O_APPEND handle carries on writing at the new end. Replacing the file by
// renaming a fresh copy over it would be atomic, but Windows refuses to rename
// over a file that is open — and the daemon (and maybe a [history] pager) always
// has this one open. The copy only ever writes over bytes it has already read
// (the destination trails the source), so a forward copy is safe.
//
// A failure leaves the file in whatever state it reached; its size is re-read
// so the next write over the limit retries.
func (s *scrollback) trim() {
	if err := s.compact(s.size - s.limit/2); err != nil {
		if info, err := s.log.Stat(); err == nil {
			s.size = info.Size()
		}
	}
}

// compact moves the log's lines from the first one starting at or after from
// to the front of the file and truncates the rest. from must be > 0.
func (s *scrollback) compact(from int64) error {
	// Scan from the byte before from: if it is a newline, from already starts a
	// line. A kept half with no newline at all (one giant line) keeps nothing.
	br := bufio.NewReaderSize(io.NewSectionReader(s.log, from-1, s.size-from+1), trimBufSize)

	for {
		_, err := br.ReadSlice('\n')
		if err == nil || errors.Is(err, io.EOF) {
			break
		}

		if !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}

	w, err := os.OpenFile(s.path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	kept, err := br.WriteTo(io.NewOffsetWriter(w, 0))
	if err != nil {
		return err
	}

	if err := s.log.Truncate(kept); err != nil {
		return err
	}

	s.size = kept

	return nil
}

// History returns the last window of recorded output to replay on attach: the
// final rows lines of the log file (see tailBytes for the read bound).
func (s *scrollback) History(rows int) []byte {
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
	off := max(size-tailBytes, 0)

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

// TermState returns the escape sequences that put a fresh terminal into the
// session's current terminal state (nil when it is the default state), and
// whether the session is on the alternate screen. It is replayed after the
// history window on attach, so a still-running full-screen app gets back the
// screen buffer and modes it set up (see termModes) — its content is then
// rebuilt by the app itself (see Daemon.nudgeRepaint), since the alt screen is
// deliberately not recorded.
func (s *scrollback) TermState() (restore []byte, alt bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cook.modes.restore(), s.cook.modes.altOn()
}

// Clear discards the session's recorded history by truncating the log file in
// place. The file stays open — it was opened with O_APPEND, so later writes land
// at the new (zero) end — and the session keeps running; only its recorded past
// is dropped, so nothing of it (say, a secret echoed to the terminal) can be
// replayed on a later attach.
func (s *scrollback) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cook.reset() // drop the in-progress line too, so nothing survives the wipe

	if s.log == nil {
		return nil
	}

	if err := s.log.Truncate(0); err != nil {
		return err
	}

	s.size = 0

	return nil
}

// Close closes the log file.
func (s *scrollback) Close() error {
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
