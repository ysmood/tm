//go:build unix

package attach

import (
	"testing"

	"github.com/ysmood/got"
)

// The menu key must be recognized however the terminal encodes Ctrl-\: the
// legacy 0x1c byte, the kitty keyboard protocol's CSI-u forms (which lazygit
// and other tcell v3 apps switch on when the terminal supports them), and
// xterm's modifyOtherKeys form. Missing one leaves the menu key dead exactly
// while such an app is on screen.
func TestMenuKeyEncodings(t *testing.T) {
	g := got.T(t)

	pressed := func(input string) bool {
		r := &relay{}

		return r.forwardInput([]byte(input)) && r.menu.Load()
	}

	g.True(pressed("\x1c"))            // legacy byte
	g.True(pressed("\x1b[92;5u"))      // kitty / modifyOtherKeys CSI-u
	g.True(pressed("\x1b[92;5:1u"))    // kitty with event types: press
	g.True(pressed("\x1b[92;5:2u"))    // kitty with event types: repeat
	g.True(pressed("\x1b[27;5;92~"))   // modifyOtherKeys=2 legacy form
	g.True(pressed("ab\x1b[92;5ucd"))  // embedded mid-chunk
	g.False(pressed("plain text"))     // no key
	g.False(pressed("\x1b[92;5:3u"))   // a release is not a press
	g.False(pressed("\x1b[92;6u"))     // Ctrl+Shift+\ is a different key
	g.False(pressed("\x1b[92;5"))      // a truncated sequence is not the key
	g.False(pressed("\x1b[27;5;92u"))  // wrong final for the legacy form
	g.False(pressed("echo \\x1b[92;5u")) // literal text, not the escape byte
}

// findMenuKey reports the earliest hit when several encodings appear, so the
// bytes before the first press — and only those — are still forwarded.
func TestFindMenuKeyEarliest(t *testing.T) {
	g := got.T(t)

	g.Eq(findMenuKey([]byte("ab\x1b[92;5ucd\x1cef")), 2)
	g.Eq(findMenuKey([]byte("ab\x1ccd\x1b[92;5uef")), 2)
	g.Eq(findMenuKey([]byte("none")), -1)
}
