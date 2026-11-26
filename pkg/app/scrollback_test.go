//go:build unix

package app_test

import (
	"strings"
	"testing"
)

// These tests all guard the same promise: leaving a session — however you leave it
// — must not cost the user their scrollback. A session's PTY has its own terminal
// state, and tm forwards it live, so leaving abandons the outer terminal
// mid-state; the reset that fixes that must stop short of the sequences that wipe
// history. Each way out (quit without attaching, the shell exiting, a menu detach,
// leaving a full-screen app) is a different code path, so each gets a test.

// mustNotWipe asserts that a slice of output leaves the terminal's history alone:
// no alt-screen leave — a bare rmcup makes many terminals (xterm.js and the VS
// Code terminal among them) "restore" a stale primary screen, taking the
// scrollback with it — and no screen or scrollback erase.
func mustNotWipe(tm *term, what, out string) {
	tm.Helper()
	tm.Desc("%s must not emit rmcup (it wipes scrollback): %q", what, out).
		False(strings.Contains(out, "\x1b[?1049l"))
	tm.Desc("%s must not erase the scrollback: %q", what, out).
		False(strings.Contains(out, "\x1b[3J"))
	tm.Desc("%s must not erase the whole screen: %q", what, out).
		False(strings.Contains(out, "\x1b[2J"))
}

// TestImmediateQuitPreservesScrollback drives the real binary from a shell prompt
// (not inside a session) and quits the menu straight away with esc, without ever
// attaching. Because the menu renders inline and no relay runs, tm must leave the
// outer terminal exactly as it found it, so the shell's history survives.
//
// Regression: tm used to write a blanket terminal reset on every exit path,
// including this one. That reset began with "leave alternate screen" even though
// nothing had entered it.
func TestImmediateQuitPreservesScrollback(t *testing.T) {
	e := setupTM(t)

	tm := e.startShell("OUTER-SCROLLBACK-MARK")
	tm.must(menuRow)

	mark := tm.mark()

	tm.write(keyEsc) // quit immediately, no session attached
	tm.waitExit()

	out := tm.since(mark)

	// Never having shown a session, tm must not enter the alternate screen either.
	tm.Desc("must not enter the alternate screen: %q", out).
		False(strings.Contains(out, "\x1b[?1049h"))
	mustNotWipe(tm, "quitting the menu", out)

	// The shell prompt printed before tm started must still be on screen: the
	// inline picker sat below it and is erased on quit, leaving the prompt intact.
	screen := tm.screen().visible()
	tm.Desc("the pre-menu scrollback must survive the quit: %q", screen).
		True(strings.Contains(screen, "OUTER-SCROLLBACK-MARK"))
}

// TestExitFromShellPreservesScrollback attaches to a session on the main screen and
// exits its shell. That is the relay's own teardown — a different code path from a
// menu detach — and it must not wipe anything either.
func TestExitFromShellPreservesScrollback(t *testing.T) {
	e := setupTM(t)

	tm := e.start()
	tm.newSession()
	tm.run("IN-SHELL")

	mark := tm.mark()

	// Exit the shell: the session ends and tm returns to the top-level menu.
	tm.write("exit" + keyEnter)
	tm.mustFrom(mark, menuRow)

	mustNotWipe(tm, "exiting the session's shell", tm.since(mark))

	tm.write(keyEsc) // esc at the top-level menu leaves tm
	tm.waitExit()
}

// TestDetachFromShellPreservesScrollback attaches to a session that never leaves
// the main screen, then detaches via the menu: nothing entered the alternate
// screen, so detaching must not leave it — while still resetting the harmless
// input/display modes.
//
// Regression: detaching (and exiting) used to write the full reset unconditionally,
// clearing the session's history from the outer terminal even from a plain shell.
func TestDetachFromShellPreservesScrollback(t *testing.T) {
	e := setupTM(t)

	tm := e.start()
	tm.newSession()
	tm.run("IN-SHELL")

	mark := tm.mark()
	tm.detach()
	tm.waitExit()

	out := tm.since(mark)
	mustNotWipe(tm, "detaching", out)

	// The harmless mode reset is still sent (scroll region reset, cursor shown).
	tm.Desc("detach should still reset terminal modes: %q", out).
		True(strings.Contains(out, "\x1b[r"))
	tm.Desc("detach should show the cursor: %q", out).
		True(strings.Contains(out, "\x1b[?25h"))
}

// TestDetachRestoresTerminal proves the other half: detaching from a session in
// which a full-screen program left the terminal in a non-default state (alternate
// screen buffer, mouse reporting, a scroll region) does restore the outer
// terminal, so the user gets their scrollback back. Without it the terminal is
// left stuck in the alternate screen buffer, where the wheel finds no history.
func TestDetachRestoresTerminal(t *testing.T) {
	e := setupTM(t)

	tm := e.start()
	tm.newSession()

	// Emulate a full-screen app inside the session: enter the alternate screen
	// buffer, turn on SGR mouse reporting, and set a scroll region — exactly the
	// state vim/less/htop leave the terminal in while running.
	tm.send(`printf '\033[?1049h\033[?1000h\033[?1006h\033[5;20r'` + keyEnter)
	tm.run("MARK")

	// Ctrl-\ opens the menu (resetting the terminal as it tears the relay down);
	// /detach-session then leaves tm. The reset asserted on below is emitted by
	// that teardown, exactly as a direct detach used to emit it.
	tm.detach()
	tm.waitExit()

	out := tm.out.String()
	count := func(s string) int { return strings.Count(out, s) }

	// The terminal must end on the main screen, not the alternate buffer: every
	// alt-screen enter is balanced by (or outnumbered by) a leave.
	tm.Desc("alt-screen left enabled: %d enters vs %d leaves\nstream: %q",
		count("\x1b[?1049h"), count("\x1b[?1049l"), out).
		True(count("\x1b[?1049l") >= count("\x1b[?1049h"))

	// Mouse reporting and the scroll region the session set must be turned off.
	tm.Desc("SGR mouse reporting not disabled: %q", out).True(strings.Contains(out, "\x1b[?1006l"))
	tm.Desc("normal mouse reporting not disabled: %q", out).True(strings.Contains(out, "\x1b[?1000l"))
	tm.Desc("scroll region not reset: %q", out).True(strings.Contains(out, "\x1b[r"))
}

// TestExitReopensMenuInline proves that when a session's shell exits and tm reopens
// the top-level menu, the menu is drawn inline below the just-exited session's
// output — not from the top of the screen. The terminal reset that runs on exit
// resets the scroll region (DECSTBM), which homes the cursor; if that home is not
// undone, the reopened inline menu renders from the top and erases the screen below
// it, wiping the session output the user just had. The reset wraps the scroll-region
// reset in save/restore-cursor so this cannot happen.
func TestExitReopensMenuInline(t *testing.T) {
	e := setupTM(t)

	tm := e.start()
	tm.newSession()
	tm.run("INSESSION")

	mark := tm.mark()

	tm.write("exit" + keyEnter)
	tm.mustFrom(mark, menuRow)

	// Look at the CURRENT screen (no scrollback): the session's output must still be
	// there, with the reopened menu below it. If the menu had rendered from a homed
	// cursor it would have erased the screen and this marker would be gone. The wait
	// is for the reopened menu to finish drawing, so a half-drawn screen can't read
	// as the menu being in the wrong place.
	screen := tm.waitScreen(func(v *vt) bool {
		s := v.screen()

		return strings.Index(s, menuRow) > strings.Index(s, "INSESSION-42")
	}).screen()

	tm.Desc("session output erased when the menu reopened:\n%s", screen).
		True(strings.Contains(screen, "INSESSION-42"))

	menuLine := strings.Index(screen, menuRow)
	sessLine := strings.Index(screen, "INSESSION-42")
	tm.Desc("menu did not reopen below the session output:\n%s", screen).
		True(menuLine > sessLine)

	tm.write(keyEsc) // leave tm
	tm.waitExit()
}
