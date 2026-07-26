//go:build unix

package app_test

import (
	"strings"
	"testing"
)

// TestMenuKeySwitchesAndResumes proves the headline of the menu key: while
// attached to a session, Ctrl-\ pops the in-session menu (rather than detaching),
// from which picking another session switches this terminal to it in place, and
// esc resumes the current session right where it was. Each WHO/RESUMED marker is
// printed by the session's own shell from $TM_SESSION, so it only appears if that
// session is actually the one driving the terminal.
func TestMenuKeySwitchesAndResumes(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa", "bbb")

	tm := e.start()
	tm.enter("aaa")

	tm.write("echo WHO=$TM_SESSION" + keyEnter)
	tm.must("WHO=aaa")

	// Ctrl-\ opens the in-session menu instead of detaching; pick bbb to switch.
	tm.menu()
	tm.switchTo("bbb")

	tm.write("echo WHO=$TM_SESSION" + keyEnter)
	tm.must("WHO=bbb")

	// Ctrl-\ again, then esc resumes bbb (no switch) right where it left off.
	tm.menu()
	tm.resume()

	tm.write("echo RESUMED=$TM_SESSION" + keyEnter)
	tm.must("RESUMED=bbb")

	mark := tm.mark()
	tm.detach()
	tm.waitExit()

	// Leaving tm for the launching shell prints an exited notice.
	tm.mustFrom(mark, exitedMsg)
}

// TestMenuKeyOpensInline proves the menu key opens the menu inline — like running
// tm inside the session — instead of clearing the screen. Opening it must not
// switch to the alternate screen buffer nor emit the rmcup leave (\e[?1049l) that
// wipes the screen and scrollback on many terminals, and the session's output must
// stay visible with the menu drawn beneath it.
func TestMenuKeyOpensInline(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")
	tm.run("INLINE-MARK")

	mark := tm.menu()

	opening := tm.since(mark)
	tm.Desc("opening the menu must not enter the alternate screen: %q", opening).
		False(strings.Contains(opening, "\x1b[?1049h"))
	tm.Desc("opening the menu must not wipe the screen with rmcup: %q", opening).
		False(strings.Contains(opening, "\x1b[?1049l"))

	// Through a terminal model, the session output must still be visible with the
	// menu drawn beneath it — not a blanked screen.
	screen := tm.screen().visible()

	tm.Desc("the session output must remain on screen: %q", screen).
		True(strings.Contains(screen, "INLINE-MARK-42"))
	tm.Desc("the menu must be shown inline beneath it: %q", screen).
		True(strings.Contains(screen, "session: aaa"))

	tm.detachFromMenu() // the menu is already open
	tm.waitExit()
}

// TestMenuKeyResumeDoesNotReplay proves that opening the menu with Ctrl-\ and then
// pressing esc to cancel just drops back into the session — it does not replay a
// screen of history, which would reprint what is already shown. The session must
// still be live afterwards (a fresh command runs).
func TestMenuKeyResumeDoesNotReplay(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")
	tm.run("RESUME-MARK")

	tm.menu()

	mark := tm.mark()
	tm.resume()

	resumed := tm.since(mark)
	tm.Desc("resume must not replay history (no reprint of the marker): %q", resumed).
		False(strings.Contains(resumed, "RESUME-MARK-42"))
	tm.Desc("resume must not trigger the daemon's soft-reset replay: %q", resumed).
		False(strings.Contains(resumed, "\x1b[!p"))

	tm.run("SECOND-MARK") // the session is still live after resuming

	tm.detach()
	tm.waitExit()
}

// TestCtrlBackslashEscPreservesPrompt drives the real binary through the relay
// menu: attach to a session, set a distinctive shell prompt, open the menu with
// Ctrl-\ and immediately cancel it with esc. Resuming the session must leave the
// shell's prompt line untouched (like fzf on esc), not swallow it, and must put
// the cursor back where the prompt left it. The fix saves and restores the cursor
// with DECSC/DECRC and draws the picker below the prompt.
func TestCtrlBackslashEscPreservesPrompt(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")

	// Give the session shell a distinctive prompt, then settle on it. The prompt
	// text is assembled from pieces so the echoed command line never contains the
	// marker contiguously — only the live prompt does, so the assertion can't match
	// the command echo by accident. "ZZMARK> " is 8 cells, so the cursor sits at
	// column index 8.
	const promptEndCol = 8

	tm.write(`A=ZZ; B=MARK; PS1="$A$B> "` + keyEnter)
	tm.must("ZZMARK>")

	// Open the menu (Ctrl-\) and immediately cancel it (esc) to resume aaa.
	tm.menu()
	tm.resume()

	// Wait for the resumed screen to settle on the prompt: the picker's erase and
	// the cursor restore are separate writes, so a mid-teardown render would read as
	// a stray menu row on the last line.
	v := tm.waitScreen(func(v *vt) bool {
		return strings.Contains(lastNonEmptyLine(v.visible()), "ZZMARK>")
	})
	screen := v.visible()

	// After esc the resumed shell is sitting at its prompt; that prompt must be the
	// last visible line, exactly as it was before Ctrl-\ (like fzf on esc). If the
	// menu teardown swallowed it, the last line is the menu/command echo instead.
	last := lastNonEmptyLine(screen)
	tm.Desc("the shell prompt must survive Ctrl-\\ then esc, last line: %q\nfull screen: %q", last, screen).
		True(strings.Contains(last, "ZZMARK>"))

	// The cursor must be back on the prompt line, just past it, so the resumed shell
	// keeps typing in the right place.
	tm.Desc("cursor row %d must be on the prompt line: %q", v.cr, v.cursorLine()).
		True(strings.Contains(v.cursorLine(), "ZZMARK>"))
	tm.Desc("cursor must be restored just past the prompt, got column %d", v.cc).Eq(v.cc, promptEndCol)

	tm.detach()
	tm.waitExit()
}

// TestFirstLaunchAttachErasesPicker drives the real binary from a shell prompt
// (not inside a session): selecting a session attaches the relay, and the picker
// must be erased on the way — like fzf — leaving the shell prompt above and the
// session's history below, with no menu rows stranded between them.
func TestFirstLaunchAttachErasesPicker(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	// Give aaa a distinctive history line, then leave it.
	seed := e.attachRelay("aaa")
	seed.write("echo AAA-HIST-MARK" + keyEnter)
	seed.must("AAA-HIST-MARK")
	seed.write(keyMenu)
	seed.waitExit()

	// Launch the menu from a shell prompt and attach to aaa; its history replays.
	tm := e.startShell("OUTER-SHELL-PROMPT")
	tm.enter("aaa")

	// Settled means the replay has landed and the picker's rows are gone; both are
	// what the assertions below then pin down precisely.
	screen := tm.waitScreen(func(v *vt) bool {
		s := v.visible()

		return strings.Contains(s, "AAA-HIST-MARK") && !strings.Contains(s, menuRow)
	}).visible()

	tm.Desc("the shell prompt above the menu must remain: %q", screen).
		True(strings.Contains(screen, "OUTER-SHELL-PROMPT"))
	tm.Desc("the session history must show after attach: %q", screen).
		True(strings.Contains(screen, "AAA-HIST-MARK"))
	tm.Desc("the picker must be erased — no command rows left behind: %q", screen).
		False(strings.Contains(screen, menuRow))
	tm.Desc("the picker must be erased — no header left behind: %q", screen).
		False(strings.Contains(screen, "namespace: default"))

	tm.detach()
	tm.waitExit()
}

// TestInlineMenuClearsLikeFzf drives the real binary: while attached to a session,
// running tm shows the picker inline — without the alternate screen — so the
// session's existing output stays on screen. On a switch the picker is then erased
// (like fzf clearing its prompt) and the target session's history replays in its
// place, rather than the screen being blanked or the picker left stranded above the
// output.
func TestInlineMenuClearsLikeFzf(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa", "bbb")

	// Give bbb a distinctive history line, then leave it.
	seed := e.attachRelay("bbb")
	seed.write("echo BBB-HISTORY-MARK" + keyEnter)
	seed.must("BBB-HISTORY-MARK")
	seed.write(keyMenu)
	seed.waitExit()

	// Attach to aaa and print a marker we expect to stay on screen.
	tm := e.attachRelay("aaa")
	tm.run("AAA-VISIBLE-MARK")

	// Run tm inside aaa: the menu must appear without switching to the alt screen.
	mark := tm.innerTM()

	tm.Desc("inner menu must not enter the alternate screen: %q", tm.since(mark)).
		False(strings.Contains(tm.since(mark), "\x1b[?1049h"))

	// Pick bbb: the inner tm hands the outer relay over, switching in place.
	tm.switchTo("bbb")

	screen := tm.waitScreen(func(v *vt) bool {
		s := v.visible()

		return strings.Contains(s, "BBB-HISTORY-MARK") && !strings.Contains(s, menuRow)
	}).visible()

	tm.Desc("the pre-menu output must still be on screen: %q", screen).
		True(strings.Contains(screen, "AAA-VISIBLE-MARK-42"))
	tm.Desc("the target session history must replay: %q", screen).
		True(strings.Contains(screen, "BBB-HISTORY-MARK"))
	tm.Desc("the picker must be erased, not left on screen: %q", screen).
		False(strings.Contains(screen, menuRow))

	tm.write(keyMenu) // the bare relay has no menu loop: the key ends it
	tm.waitExit()
}
