package daemon

import (
	"bufio"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
)

// tailBytes bounds how much of the log file's end History reads to find its last
// lines. An attach wants one screenful of them, so this only has to be more than
// a screenful of bytes — escape sequences and all, which output dense with
// colors, links and wide rows inflates well past the visible text. Reading a
// bounded tail (rather than the whole file) keeps an attach's cost fixed no
// matter how long the session has been running.
const tailBytes = 10 << 20 // 10 MiB

// maxLogBytes caps a session's log file. Only what scrolls off the screen is
// recorded (see screen), so redraws cost nothing, but a session can run for
// weeks, and a program can still print without end (a log tail, a `clear` loop,
// each clear keeping a screenful). Past the cap the log's oldest output is
// dropped (see trim), keeping its newest half, so the file stays between half
// and all of this size however much the session prints.
const maxLogBytes = 500 << 20 // 500 MiB

// trimBufSize is the chunk size trim copies the kept half of the log in.
const trimBufSize = 64 << 10 // 64 KiB

// scrollback is a session's terminal state and history: the shadow screen raw
// PTY output is fed to (see screen), and the append-only log file the lines
// scrolling off that screen are recorded to — the only place the session's
// history lives. The log holds clean text and color — what a pager shows for
// /history — rather than the raw control stream a terminal would act on; what
// is still on screen is the screen's, until it scrolls off.
type scrollback struct {
	mu    sync.Mutex
	path  string
	log   *os.File
	size  int64 // the log file's length, tracked so Write needn't stat per chunk
	limit int64 // the size past which the log is trimmed (maxLogBytes)
	scr   *screen
}

// newScrollback opens (creating it if needed) the log file the session appends
// its history to, with a blank cols×rows screen in front of it. The file is
// opened for reading too, so History can read back the tail it just wrote.
func newScrollback(logPath string, cols, rows int) (*scrollback, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, err
	}

	return &scrollback{
		path: logPath, log: f, size: info.Size(), limit: maxLogBytes, scr: newScreen(cols, rows),
	}, nil
}

// Write feeds a chunk of raw output to the screen and appends the lines it
// pushed off the screen to the log.
func (s *scrollback) Write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.record(s.scr.write(p))
}

// Resize changes the screen's size, recording any rows a shrink pushed off.
func (s *scrollback) Resize(cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.record(s.scr.resize(cols, rows))
}

// record appends history to the log; a write that takes the log past its limit
// trims it (see trim). The caller holds s.mu.
func (s *scrollback) record(hist []byte) {
	if s.log == nil || len(hist) == 0 {
		return
	}

	n, _ := s.log.Write(hist)
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
// over a file that is open — and the daemon (and maybe a /history pager) always
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

// History returns the log's last rows lines (see tailBytes for the read bound):
// the history an attach puts in the client's scrollback above the painted screen.
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

	return tailLines(buf, rows)
}

// Paint returns the escape stream that reproduces the session's screen on a
// freshly reset client terminal of the screen's size (see screen.paint).
func (s *scrollback) Paint() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.scr.paint()
}

// Snapshot renders what the main screen shows as history text (see
// screen.snapshot): the part of the session's history not yet in the log.
func (s *scrollback) Snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.scr.snapshot()
}

// Blank reports whether the main screen shows no text at all.
func (s *scrollback) Blank() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return lastTextRow(s.scr.main) < 0
}

// Alt reports whether the session is on the alternate screen — inside a
// full-screen app.
func (s *scrollback) Alt() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.scr.onAlt
}

// Clear discards the session's recorded history: the log file is truncated in
// place and the screen is blanked, so nothing of it (say, a secret echoed to the
// terminal) can be paged or replayed on a later attach. The file stays open — it
// was opened with O_APPEND, so later writes land at the new (zero) end — and the
// session keeps running, its cursor, modes and screen buffer as they were.
func (s *scrollback) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.scr.clearContent()

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
