//go:build unix

package app_test

import (
	"strings"
	"testing"
)

// TestRenameSessionNoticePersists drives the real binary: renaming a session from
// the menu prints a notice, and that notice must survive both the picker's redraws
// (the menu stays open on the main list afterwards) and its teardown when tm exits
// — so it reads like the attach/detach notices, left behind in the scrollback
// rather than erased along with the menu.
func TestRenameSessionNoticePersists(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.must(renameRow)

	tm.rename("aaa", "-zzz")

	// The rename lands in the store...
	renamed, err := e.st.GetSession("aaa")
	e.E(err)
	e.Eq(renamed.Name, "aaa-zzz")

	// ...and the notice is still on screen while the menu is back on the main list,
	// which now lists the session under its new name.
	screen := tm.screen().screen()
	tm.Desc("the notice must survive the menu's redraws: %q", screen).
		True(strings.Contains(screen, "[tm renamed session aaa → aaa-zzz]"))
	tm.Desc("the menu must be back, not torn down: %q", screen).
		True(strings.Contains(screen, renameRow))

	// esc leaves tm from the top-level menu: the picker is erased, but the notice
	// stays behind in the scrollback along with the exit notice.
	tm.write(keyEsc)
	tm.must(exitedMsg)
	tm.waitExit()

	visible := tm.screen().visible()

	tm.Desc("the notice must outlive the picker: %q", visible).
		True(strings.Contains(visible, "[tm renamed session aaa → aaa-zzz]"))
	tm.Desc("the picker must be erased: %q", visible).
		False(strings.Contains(visible, renameRow))
}

// TestRenameFromSessionKeepsCursor drives the real binary: renaming a session from
// the menu opened over it (Ctrl-\) and then resuming with esc must leave the cursor
// back on the shell's prompt line — the notices printed above the picker must not
// shift where the restored cursor lands. A misplaced cursor makes the next command
// overwrite the line above the prompt.
func TestRenameFromSessionKeepsCursor(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")

	// The assertions below compare prompt prefixes, so the base command must land
	// after a rendered prompt.
	tm.must(testPrompt)

	// A known line above the prompt, so clobbering is detectable.
	tm.write("echo CURSOR-BASE-MARK" + keyEnter)
	tm.must("CURSOR-BASE-MARK")

	// Rename twice — aaa -> aaa-zzz -> aaa-zzz-yyy — so BOTH notice rows must be
	// relocated above the prompt; esc then resumes the session.
	tm.menu()
	tm.rename("aaa", "-zzz")
	tm.rename("aaa-zzz", "-yyy")

	// esc resumes the session. The wait is for the second notice being rewritten
	// above the prompt, which is the guard finishing its cursor surgery — typing
	// before that lands the command wherever the cursor happens to be mid-move —
	// and then for the resumed session to stop redrawing (the re-attach resizes its
	// PTY, and the shell repaints its prompt on the SIGWINCH that brings).
	tm.resumeAfterNotice("aaa-zzz → aaa-zzz-yyy")
	tm.quiet()

	// Type another command; with the cursor restored correctly it lands on the
	// prompt line, below everything already on screen.
	tm.write("echo CURSOR-AFTER-MARK" + keyEnter)
	tm.must("CURSOR-AFTER-MARK")

	// The echoed command must carry the shell's prompt as its prefix; wait for the
	// line to be fully drawn rather than catching it mid-render.
	screen := tm.waitScreen(func(v *vt) bool {
		return strings.Contains(v.visible(), testPrompt+"echo CURSOR-AFTER-MARK")
	}).visible()

	// Nothing that was on screen before the renames may be overwritten by the
	// resumed session's next command.
	tm.Desc("the pre-rename output must survive: %q", screen).
		True(strings.Contains(screen, "CURSOR-BASE-MARK"))
	tm.Desc("the first rename notice must survive the resumed session's output: %q", screen).
		True(strings.Contains(screen, "[tm renamed session aaa → aaa-zzz]"))
	tm.Desc("the second rename notice must survive too: %q", screen).
		True(strings.Contains(screen, "[tm renamed session aaa-zzz → aaa-zzz-yyy]"))

	// The new command must land below everything, in order: base output, the two
	// notices, then the new echo — i.e. the notices sit in the scrollback above the
	// prompt, and the cursor was back on the prompt line.
	base := strings.Index(screen, "echo CURSOR-BASE-MARK")
	first := strings.Index(screen, "[tm renamed session aaa →")
	second := strings.Index(screen, "[tm renamed session aaa-zzz →")
	after := strings.Index(screen, "echo CURSOR-AFTER-MARK")
	tm.Desc("expected base < first < second < after, got %d %d %d %d in %q",
		base, first, second, after, screen).
		True(base >= 0 && first > base && second > first && after > second)

	// The cursor must also be restored to the right COLUMN — right after the prompt,
	// not at the start of the line — so the resumed command carries the same shell
	// prompt prefix as the one typed before the menu opened.
	prefixOf := func(marker string) (string, bool) {
		for l := range strings.SplitSeq(screen, "\n") {
			if i := strings.Index(l, "echo "+marker); i >= 0 {
				return l[:i], true
			}
		}

		return "", false
	}

	basePrefix, ok := prefixOf("CURSOR-BASE-MARK")
	tm.True(ok)
	tm.Desc("the shell must have printed a prompt before the typed command: %q", screen).
		NotZero(len(basePrefix))

	afterPrefix, ok := prefixOf("CURSOR-AFTER-MARK")
	tm.True(ok)
	tm.Desc("the resumed command must start right after the prompt, not at column 0: %q", screen).
		Eq(afterPrefix, basePrefix)
}
