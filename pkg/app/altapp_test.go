//go:build unix

package app_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake full-screen app the alt-screen tests run inside a session: it
// enters the alt screen the way lazygit does — alt screen, hidden cursor,
// mouse reporting, and the kitty keyboard protocol (which tcell v3 apps
// switch on when the terminal supports it) — then sits in the foreground.
const fakeAltApp = "printf '\\033[?1049h\\033[?25l\\033[?1002h\\033[?1006h\\033[=1u'; sleep 600"

// kittyMenuKey is what a terminal with the kitty keyboard protocol active
// sends for Ctrl-\ instead of the legacy 0x1c byte.
const kittyMenuKey = "\x1b[92;5u"

// menuVia opens the in-session menu pressing the given menu-key encoding and
// waits for a fresh render.
func (tm *term) menuVia(key string) {
	tm.Helper()

	mark := tm.mark()
	tm.write(key)
	tm.mustFrom(mark, sessionHint)
}

// TestMenuKeyAfterReattachIntoFullScreenApp: detach from a session running a
// full-screen app, re-attach (the replay restores the app's alt screen and
// modes), then press Ctrl-\ — the menu must still open and [detach session]
// must still work. This terminal sends the legacy 0x1c, as one without the
// kitty protocol would.
func TestMenuKeyAfterReattachIntoFullScreenApp(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")
	tm.run("PRE-MARK")

	tm.write(fakeAltApp + keyEnter)
	tm.must("\x1b[?1049h") // the app is up (its real toggle, not the echo)

	tm.detach()
	tm.waitExit()

	// Re-attach: the replay must land back inside the app's alt screen.
	tm2 := e.start()
	tm2.enter("aaa")
	tm2.must("\x1b[?1049h")

	tm2.menu()
	tm2.detachFromMenu()
	tm2.waitExit()
}

// TestResumeRepaintsFullScreenApp pins the fix for the menu leaving a hole in
// a live full-screen app: Ctrl-\ draws the menu over the app's alt screen, and
// esc (resume) erases the menu's rows — output the app knows nothing about.
// A resume replays nothing by design, so the daemon must instead nudge the
// app itself to repaint (it answers the size-change signal like any TUI).
func TestResumeRepaintsFullScreenApp(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	// A fake full-screen app that runs as its own foreground job — where a real
	// TUI lives, and where the PTY's size-change signal is delivered — and
	// "repaints" on it. The marker is emitted in two halves so no echo of the
	// script text can match it.
	dir := t.TempDir()
	app := filepath.Join(dir, "fakeapp.sh")
	e.E(os.WriteFile(app, []byte("#!/bin/sh\n"+
		"printf '\\033[?1049h\\033[?25l'\n"+
		"trap 'printf RE; printf PAINTED' WINCH\n"+
		"while :; do sleep 1; done\n"), 0o700))

	tm := e.start()
	tm.enter("aaa")
	tm.write(app + keyEnter)
	tm.must("\x1b[?1049h")

	tm.menu()

	mark := tm.mark()
	tm.write(keyEsc) // resume the session: no replay, but the app must repaint
	tm.mustFrom(mark, "REPAINTED")

	tm.Desc("a resume must stay replay-free: %q", tm.since(mark)).
		False(strings.Contains(tm.since(mark), "\x1b[!p"))

	tm.detach()
	tm.waitExit()
}

// TestMenuKeyUnderKittyProtocol simulates a terminal that honors the kitty
// keyboard protocol the app switched on: Ctrl-\ arrives as its CSI-u encoding,
// live and after a re-attach — whose replay must also re-arm the protocol for
// the app — and detaching after a menu resume must still leave the alt screen
// (the resume must not forget the session is on it).
func TestMenuKeyUnderKittyProtocol(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")
	tm.write(fakeAltApp + keyEnter)
	tm.must("\x1b[=1u") // the app switched the keyboard protocol on

	// Live: the encoded menu key must open the menu; detach and leave.
	tm.menuVia(kittyMenuKey)
	tm.detachFromMenu()
	tm.waitExit()

	// Re-attach: the replay re-enters the alt screen and re-arms the protocol.
	tm2 := e.start()
	tm2.enter("aaa")
	tm2.must("\x1b[=1;1u")

	// The encoded menu key works after the re-attach; resume, then open the
	// menu again and detach: the reset must leave the alt screen even though a
	// resume replays nothing (the relay keeps the alt state across it).
	tm2.menuVia(kittyMenuKey)
	tm2.resume()
	tm2.menuVia(kittyMenuKey)

	mark := tm2.mark()
	tm2.detachToMenu()
	tm2.Desc("detach after a resume must still leave the app's alt screen: %q", tm2.since(mark)).
		True(strings.Contains(tm2.since(mark), "\x1b[?1049l"))

	tm2.write(keyEsc)
	tm2.waitExit()
}
