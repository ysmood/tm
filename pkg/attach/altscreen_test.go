//go:build unix

package attach

import (
	"bytes"
	"testing"

	"github.com/ysmood/got"
)

// TestRestoreFor checks the scrollback-preserving split: a session on the main
// screen gets the modes-only reset (no rmcup), while one left on the alternate
// screen gets the full restore that leaves it.
func TestRestoreFor(t *testing.T) {
	g := got.T(t)

	mainScreen := RestoreFor(false)
	g.False(bytes.Contains(mainScreen, []byte(altScreenExit))) // no rmcup -> scrollback kept
	g.True(bytes.Contains(mainScreen, []byte("\x1b[r")))       // modes still reset
	g.True(bytes.Contains(mainScreen, []byte("\x1b[?25h")))

	altScreen := RestoreFor(true)
	g.True(bytes.Contains(altScreen, []byte(altScreenExit))) // leaves the alt screen
	g.Eq(string(altScreen), string(TerminalRestore))
}

// TestSwitchResetReturnsToColumnZero pins the switch-vs-leave asymmetry: switching
// to a session ends the reset with a carriage return so the target's raw history
// replay starts from column 0 and lines up with how it was recorded (else a recorded
// partial prompt keeps zsh's "%" EOL marker on screen). It must NOT home the cursor
// (that would overwrite the leaving session's output instead of scrolling it into
// the scrollback), and leaving to the inline menu or the shell omits the CR entirely
// so the menu/prompt renders exactly in place.
func TestSwitchResetReturnsToColumnZero(t *testing.T) {
	g := got.T(t)

	g.True(bytes.HasSuffix(SwitchReset, []byte("\r")))         // switch returns to column 0
	g.False(bytes.Contains(SwitchReset, []byte("\x1b[H")))     // but never homes (no screen wipe)
	g.True(bytes.Contains(SwitchReset, []byte(altScreenExit))) // and still leaves the alt screen

	// Leaving to the menu / shell must not shift the cursor at all.
	g.False(bytes.Contains(TerminalModesReset, []byte("\r")))
	g.False(bytes.HasSuffix(TerminalRestore, []byte("\r")))
	g.False(bytes.HasSuffix(RestoreFor(false), []byte("\r")))
	g.False(bytes.HasSuffix(RestoreFor(true), []byte("\r")))
}

// TestTrackAltScreen feeds output chunks through the relay's tracker and checks the
// recorded alternate-screen state — the signal that decides whether leaving the
// session must emit rmcup.
func TestTrackAltScreen(t *testing.T) {
	g := got.T(t)

	feed := func(chunks ...string) bool {
		r := &relay{}
		for _, c := range chunks {
			r.trackAlt([]byte(c))
		}

		return r.alt.Load()
	}

	g.False(feed("hello, just a prompt $ "))    // no toggles -> main screen
	g.True(feed("\x1b[?1049h"))                 // enter alt screen
	g.False(feed("\x1b[?1049h", "\x1b[?1049l")) // enter then leave -> main
	g.True(feed("\x1b[?1049l", "\x1b[?1049h"))  // last toggle wins
	g.True(feed("vim\x1b[?1049hsome text"))     // toggle embedded mid-chunk
	g.True(feed("\x1b[?1047h"))                 // older alt-screen modes too
	g.True(feed("\x1b[?47h"))
	g.False(feed("\x1b[?47h", "\x1b[?47l"))

	// A toggle split across two chunks is still recognized via the carry buffer.
	g.True(feed("output then \x1b[?10", "49h more output"))
	g.False(feed("\x1b[?1049h", "leaving \x1b[?104", "9l done"))
}

// TestResetAltClearsState confirms each attachment starts tracking fresh, so a
// prior session's alt-screen state can't decide this one's restore.
func TestResetAltClearsState(t *testing.T) {
	g := got.T(t)

	r := &relay{}
	r.trackAlt([]byte("\x1b[?1049h"))
	g.True(r.alt.Load())

	r.resetAlt()
	g.False(r.alt.Load())

	// The carry is cleared too: a dangling partial from before the reset must not
	// combine with new output to fabricate a toggle.
	r.trackAlt([]byte("49h"))
	g.False(r.alt.Load())
}
