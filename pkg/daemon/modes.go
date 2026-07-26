package daemon

import (
	"bytes"
	"strconv"
)

// termModes tracks the terminal state a session's running program has set up:
// which screen buffer it is on and the DEC private modes it switched on. The
// alt screen's *content* is deliberately not recorded (see cooker) — the log is
// a history of the main screen — so this state is the only thing an attach can
// use to hand the terminal back to a still-running full-screen app: restore
// re-enters the alt screen and re-applies the modes, and the app itself is then
// nudged to repaint (see Daemon.nudgeRepaint). Without it, a re-attach into a
// running lazygit/vim/htop lands on the main screen with default modes, and the
// app's incremental updates paint garbage over the replayed history.
//
// The zero value is a terminal's default state.
type termModes struct {
	// altMode is the DEC mode the alt screen was entered with (47, 1047, or
	// 1049), or 0 when the session is on the main screen. The entering variant is
	// kept so the restore pairs with the leave sequence the app will eventually
	// send.
	altMode int
	// mouseTrack is the active mouse tracking mode (1000 click, 1002 button-drag,
	// or 1003 any-motion), or 0 when tracking is off. The modes replace one
	// another in a terminal, so only the last one set is kept.
	mouseTrack int
	// mouseEnc is the active mouse coordinate encoding (1005 UTF-8, 1006 SGR, or
	// 1015 urxvt), or 0 for the legacy default.
	mouseEnc int

	appCursor    bool // DECCKM (?1): cursor keys send application sequences
	appKeypad    bool // DECKPAM (ESC =): keypad sends application sequences
	cursorHidden bool // DECTCEM (?25) switched off
	wrapOff      bool // DECAWM (?7) switched off
	focus        bool // focus reporting (?1004)
	paste        bool // bracketed paste (?2004)
}

// track updates the state from one complete escape sequence, as delimited by
// scanEscape. Sequences that are not tracked mode switches are ignored.
func (m *termModes) track(seq []byte) {
	// The keypad toggles are bare two-byte escapes, not CSI.
	if len(seq) == 2 {
		switch seq[1] {
		case '=':
			m.appKeypad = true
		case '>':
			m.appKeypad = false
		}

		return
	}

	// Everything else tracked is a DEC private mode set/reset: "\x1b[?Pm...h/l",
	// where Pm is one or more ';'-separated mode numbers.
	if len(seq) < 5 || seq[1] != '[' || seq[2] != '?' {
		return
	}

	final := seq[len(seq)-1]
	if final != 'h' && final != 'l' {
		return
	}

	for f := range bytes.SplitSeq(seq[3:len(seq)-1], []byte{';'}) {
		m.set(modeParam(f), final == 'h')
	}
}

// set applies one mode number's h (on) or l (off) to the tracked state.
func (m *termModes) set(mode int, on bool) {
	switch mode {
	case 47, 1047, 1049:
		m.altMode = 0
		if on {
			m.altMode = mode
		}
	case 1:
		m.appCursor = on
	case 7:
		m.wrapOff = !on
	case 25:
		m.cursorHidden = !on
	case 1000, 1002, 1003:
		// An l of any tracking mode turns tracking off (a terminal has one such
		// mode active; resets disable them all in a row).
		m.mouseTrack = 0
		if on {
			m.mouseTrack = mode
		}
	case 1005, 1006, 1015:
		m.mouseEnc = 0
		if on {
			m.mouseEnc = mode
		}
	case 1004:
		m.focus = on
	case 2004:
		m.paste = on
	}
}

// altOn reports whether the session is on the alternate screen.
func (m termModes) altOn() bool { return m.altMode != 0 }

// restore returns the escape sequences that take a terminal in its default
// state to the tracked one, or nil when the state is the default. Entering the
// alt screen comes with an explicit clear-and-home (1049 clears on entry but 47
// and 1047 do not), giving the app's forthcoming repaint a blank screen either
// way. The soft reset a replay starts with (see softReset) has already put the
// terminal at the defaults these deviations are applied over.
func (m termModes) restore() []byte {
	var b []byte

	if m.altMode != 0 {
		b = append(b, privateMode(m.altMode, true)...)
		b = append(b, "\x1b[2J\x1b[H"...)
	}

	if m.appCursor {
		b = append(b, "\x1b[?1h"...)
	}

	if m.appKeypad {
		b = append(b, "\x1b="...)
	}

	if m.wrapOff {
		b = append(b, "\x1b[?7l"...)
	}

	if m.cursorHidden {
		b = append(b, "\x1b[?25l"...)
	}

	if m.focus {
		b = append(b, "\x1b[?1004h"...)
	}

	if m.paste {
		b = append(b, "\x1b[?2004h"...)
	}

	if m.mouseTrack != 0 {
		b = append(b, privateMode(m.mouseTrack, true)...)
	}

	if m.mouseEnc != 0 {
		b = append(b, privateMode(m.mouseEnc, true)...)
	}

	return b
}

// privateMode renders a DEC private mode set (on) or reset sequence.
func privateMode(mode int, on bool) []byte {
	final := byte('l')
	if on {
		final = 'h'
	}

	return append(strconv.AppendInt([]byte("\x1b[?"), int64(mode), 10), final)
}

// modeParam parses one numeric mode parameter, returning -1 (matching no mode)
// for an empty or non-numeric one.
func modeParam(f []byte) int {
	if len(f) == 0 {
		return -1
	}

	n := 0

	for _, b := range f {
		if b < '0' || b > '9' || n > 1<<16 {
			return -1
		}

		n = n*10 + int(b-'0')
	}

	return n
}
