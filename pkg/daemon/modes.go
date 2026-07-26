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

	// kittyFlags is the kitty keyboard protocol's active flag set ("\x1b[=1u"
	// and friends), 0 when the legacy encoding is in effect. kittyStack holds
	// the flag sets saved by pushes ("\x1b[>1u"), so a pop restores the right
	// one. Modern TUIs (tcell v3 — lazygit — and Bubble Tea) switch this on
	// when the terminal supports it, and it changes how every modified key —
	// including tm's own Ctrl-\ — reaches the relay.
	kittyFlags int
	kittyStack []int
	// modifyKeys is xterm's modifyOtherKeys level ("\x1b[>4;2m"), 0 when off —
	// the older cousin of the kitty protocol, with the same consequences.
	modifyKeys int

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

	if len(seq) < 4 || seq[1] != '[' {
		return
	}

	switch final := seq[len(seq)-1]; {
	case final == 'u':
		m.trackKitty(seq)

		return
	case final == 'm' && seq[2] == '>':
		m.trackModifyKeys(seq)

		return
	case final != 'h' && final != 'l':
		return
	}

	// What remains tracked is a DEC private mode set/reset: "\x1b[?Pm...h/l",
	// where Pm is one or more ';'-separated mode numbers.
	if len(seq) < 5 || seq[2] != '?' {
		return
	}

	on := seq[len(seq)-1] == 'h'

	for f := range bytes.SplitSeq(seq[3:len(seq)-1], []byte{';'}) {
		m.set(modeParam(f), on)
	}
}

// kittyStackCap bounds the pushed-flags stack; kitty itself caps its stack and
// discards the oldest entry past the cap, and so does this.
const kittyStackCap = 16

// trackKitty applies a kitty keyboard protocol sequence: set ("\x1b[=f;mu"),
// push ("\x1b[>fu"), or pop ("\x1b[<nu"). A bare "\x1b[u" (SCO restore-cursor)
// and the "\x1b[?u" query pass through untouched.
func (m *termModes) trackKitty(seq []byte) {
	body := seq[3 : len(seq)-1]

	switch seq[2] {
	case '=':
		flags, mode := splitKittyParams(body)
		switch mode {
		case 1:
			m.kittyFlags = flags
		case 2:
			m.kittyFlags |= flags
		case 3:
			m.kittyFlags &^= flags
		}
	case '>':
		m.kittyStack = append(m.kittyStack, m.kittyFlags)
		if len(m.kittyStack) > kittyStackCap {
			m.kittyStack = m.kittyStack[1:]
		}

		flags, _ := splitKittyParams(body)
		m.kittyFlags = flags
	case '<':
		n := max(modeParam(body), 1)
		for ; n > 0; n-- {
			if len(m.kittyStack) == 0 {
				m.kittyFlags = 0 // popping past the stack resets, per the spec

				break
			}

			m.kittyFlags = m.kittyStack[len(m.kittyStack)-1]
			m.kittyStack = m.kittyStack[:len(m.kittyStack)-1]
		}
	}
}

// splitKittyParams parses "flags[;mode]" from a kitty set/push body. Absent or
// invalid flags mean 0; an absent mode defaults to 1 (set), per the protocol.
func splitKittyParams(body []byte) (flags, mode int) {
	f, m, found := bytes.Cut(body, []byte{';'})

	flags = max(modeParam(f), 0)

	mode = 1
	if found {
		mode = modeParam(m)
	}

	return flags, mode
}

// trackModifyKeys applies an xterm modify-keys resource sequence ("\x1b[>Pp;Pvm").
// Only resource 4 (modifyOtherKeys) is tracked; "\x1b[>4m" and the reset-all
// "\x1b[>m" switch it off.
func (m *termModes) trackModifyKeys(seq []byte) {
	body := seq[3 : len(seq)-1]
	if len(body) == 0 {
		m.modifyKeys = 0 // "\x1b[>m" resets every modify-keys resource

		return
	}

	p, v, found := bytes.Cut(body, []byte{';'})
	if modeParam(p) != 4 {
		return
	}

	m.modifyKeys = 0
	if found {
		m.modifyKeys = max(modeParam(v), 0)
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

	// The keyboard protocols are restored with the set form regardless of how
	// the app enabled them: the effective encoding is what matters, and the
	// replay's preamble (see softReset) has already cleared any stale state.
	if m.modifyKeys != 0 {
		b = append(b, "\x1b[>4;"...)
		b = strconv.AppendInt(b, int64(m.modifyKeys), 10)
		b = append(b, 'm')
	}

	if m.kittyFlags != 0 {
		b = append(b, "\x1b[="...)
		b = strconv.AppendInt(b, int64(m.kittyFlags), 10)
		b = append(b, ";1u"...)
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
