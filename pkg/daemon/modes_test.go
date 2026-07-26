package daemon

import (
	"testing"

	"github.com/ysmood/got"
)

// modesAfter feeds a raw stream through a cooker and returns the tracked
// terminal state, the way the daemon sees it at attach time.
func modesAfter(chunks ...string) termModes {
	var c cooker
	for _, ch := range chunks {
		c.cook([]byte(ch))
	}

	return c.modes
}

// A fresh session has nothing to restore; a lazygit-style full-screen app
// (alt screen, hidden cursor, mouse reporting) restores exactly what it set,
// re-entering the alt screen first — with a clear, since the app repaints
// from scratch — and its clean exit leaves nothing to restore again.
func TestModesRestoreFullScreenApp(t *testing.T) {
	g := got.T(t)

	g.Len(modesAfter("$ ls\nplain output\n").restore(), 0)

	enter := "\x1b[?1049h\x1b[?25l\x1b[?1002h\x1b[?1006h"
	g.Eq(string(modesAfter("$ lazygit\n"+enter+"PAINT").restore()),
		"\x1b[?1049h\x1b[2J\x1b[H\x1b[?25l\x1b[?1002h\x1b[?1006h")

	exit := "\x1b[?1002l\x1b[?1006l\x1b[?25h\x1b[?1049l"
	g.Len(modesAfter(enter+"PAINT"+exit+"$ ").restore(), 0)
}

// The alt screen is restored with the same mode variant it was entered with,
// so the restore pairs with the leave sequence the app will eventually send.
func TestModesAltVariants(t *testing.T) {
	g := got.T(t)

	g.Eq(modesAfter("\x1b[?47h").altMode, 47)
	g.Eq(string(modesAfter("\x1b[?47h").restore()), "\x1b[?47h\x1b[2J\x1b[H")
	g.Eq(modesAfter("\x1b[?1047h").altMode, 1047)
	g.Eq(modesAfter("\x1b[?1047h", "\x1b[?1047l").altMode, 0)

	// Split across chunk boundaries, the toggle still lands in the tracker.
	g.Eq(modesAfter("\x1b[?10", "49h").altMode, 1049)
}

// A private-mode set with several parameters applies them all — both to the
// tracked state and to the recording toggle, so a combined
// "\x1b[?1049;2004h" still suppresses the alt-screen paint from the log.
func TestModesMultiParam(t *testing.T) {
	g := got.T(t)

	m := modesAfter("\x1b[?1049;2004h")
	g.Eq(m.altMode, 1049)
	g.True(m.paste)

	g.Eq(cookAll("a\x1b[?1049;2004hHIDDEN\x1b[?1049lb"), "ab")
}

// Mouse tracking modes replace one another (a terminal runs at most one), and
// switching any of them off ends tracking; input modes toggle independently.
func TestModesInputModes(t *testing.T) {
	g := got.T(t)

	g.Eq(modesAfter("\x1b[?1000h\x1b[?1003h").mouseTrack, 1003)
	g.Eq(modesAfter("\x1b[?1003h\x1b[?1003l").mouseTrack, 0)
	g.Eq(modesAfter("\x1b[?1005h\x1b[?1015h").mouseEnc, 1015)

	m := modesAfter("\x1b[?1h\x1b=\x1b[?7l\x1b[?1004h\x1b[?2004h")
	g.Eq(string(m.restore()), "\x1b[?1h\x1b=\x1b[?7l\x1b[?1004h\x1b[?2004h")

	g.False(modesAfter("\x1b=\x1b>").appKeypad)

	// Untracked private modes and non-mode escapes change nothing.
	g.Len(modesAfter("\x1b[?12h\x1b[2J\x1b[31m\x1b(B").restore(), 0)
}

// The keyboard protocols re-encode every modified key — including the relay's
// Ctrl-\ menu key — so the tracker must follow them exactly: kitty's set /
// or / not modes and its push/pop stack, and xterm's modifyOtherKeys, with
// the restore re-arming whatever is in effect.
func TestModesKeyboardProtocols(t *testing.T) {
	g := got.T(t)

	// kitty set form, with and without the explicit mode.
	g.Eq(modesAfter("\x1b[=1u").kittyFlags, 1)
	g.Eq(modesAfter("\x1b[=1;1u").kittyFlags, 1)
	g.Eq(modesAfter("\x1b[=1u", "\x1b[=0u").kittyFlags, 0)
	g.Eq(modesAfter("\x1b[=1u\x1b[=2;2u").kittyFlags, 3) // mode 2 ors flags in
	g.Eq(modesAfter("\x1b[=3u\x1b[=1;3u").kittyFlags, 2) // mode 3 clears them

	// push/pop keep a stack; popping past it resets, per the spec.
	g.Eq(modesAfter("\x1b[>1u").kittyFlags, 1)
	g.Eq(modesAfter("\x1b[=2u\x1b[>1u\x1b[<u").kittyFlags, 2)
	g.Eq(modesAfter("\x1b[<5u").kittyFlags, 0)

	// SCO restore-cursor and the kitty query are not flag changes.
	g.Eq(modesAfter("\x1b[=1u\x1b[u\x1b[?u").kittyFlags, 1)

	// modifyOtherKeys: only resource 4, with "\x1b[>4m" and the reset-all
	// "\x1b[>m" switching it off.
	g.Eq(modesAfter("\x1b[>4;2m").modifyKeys, 2)
	g.Eq(modesAfter("\x1b[>4;2m\x1b[>4m").modifyKeys, 0)
	g.Eq(modesAfter("\x1b[>4;2m\x1b[>m").modifyKeys, 0)
	g.Eq(modesAfter("\x1b[>4;2m\x1b[>1;2m").modifyKeys, 2) // other resources ignored

	// The restore re-arms both with the set form.
	g.Eq(string(modesAfter("\x1b[=1u\x1b[>4;2m").restore()), "\x1b[>4;2m\x1b[=1;1u")
	g.Len(modesAfter("\x1b[=1u\x1b[=0u\x1b[>4m").restore(), 0)
}

// A history wipe resets the cooker's buffered content but not the tracked
// terminal state: the session's terminal is still inside whatever app was
// running, so recording stays suppressed and an attach still restores it.
func TestModesSurviveReset(t *testing.T) {
	g := got.T(t)

	var c cooker

	c.cook([]byte("$ vim\n\x1b[?1049h\x1b[?25lPAINT"))
	c.reset()

	g.Eq(c.modes.altMode, 1049)
	g.True(c.modes.cursorHidden)

	// Still on the alt screen: paint after the wipe stays out of the log.
	g.Len(c.cook([]byte("MORE PAINT\n")), 0)

	// Leaving the alt screen resumes recording as usual.
	c.cook([]byte("\x1b[?25h\x1b[?1049l"))
	g.Eq(string(c.cook([]byte("$ done\n"))), "$ done\n")
	g.Len(c.modes.restore(), 0)
}
