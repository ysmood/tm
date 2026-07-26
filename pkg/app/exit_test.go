//go:build unix

package app_test

import "testing"

// The ways a session or tm itself can end, each of which lands the user somewhere
// specific: exiting the shell ends the session and returns to the top-level menu,
// [detach session] returns there with the session still running, [exit] leaves tm
// entirely, and [kill session] aimed at the current session ends what is on the
// terminal and drops back to the menu.

// TestSessionExitReturnsToMenu proves that exiting the session's shell ends the
// session and drops back to the top-level tm menu (rather than leaving tm), so the
// user can pick or start another session.
func TestSessionExitReturnsToMenu(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")
	tm.run("IN-SHELL")

	mark := tm.mark()

	tm.write("exit" + keyEnter)

	// The relay notes which session ended, then the menu redraws.
	tm.mustFrom(mark, exitedSess)
	tm.mustFrom(mark, menuRow)

	// The exited session is really gone — its daemon removed the session's files,
	// so this was an exit, not a detach.
	e.True(e.recordGone("aaa"))

	tm.write(keyEsc) // esc at the top-level menu leaves tm
	tm.waitExit()
}

// TestDetachFromSessionReturnsToMenu proves the headline of [detach session]:
// detaching from inside a session no longer leaves tm — it drops back to the
// top-level menu with the session still running, so the user can pick it (or
// another) straight away. It detaches, checks tm stayed up with the session still
// in the store, then re-attaches from that menu and runs a command to prove both
// the session and tm survived.
func TestDetachFromSessionReturnsToMenu(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")
	tm.run("IN-SESSION")

	tm.menu()

	mark := tm.detachToMenu()
	tm.mustFrom(mark, detachedMsg)

	// The session is still alive — detach is not exit, so its record stays.
	_, err := e.st.GetSession("aaa")
	e.Desc("the session must still exist after detach").E(err)

	// Re-attach from the top-level menu and run a command: it only reaches a still
	// live session, proving detach kept both it and tm running.
	tm.enter("aaa")
	tm.run("BACK")

	tm.detach()
	tm.waitExit()
}

// TestExitCommandLeavesTM proves [exit] leaves tm from inside a session — unlike
// [detach session], which returns to the menu, and unlike esc, which resumes. tm
// exits to the launching shell while the session keeps running.
func TestExitCommandLeavesTM(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")
	tm.run("IN-SESSION")

	tm.menu()

	mark := tm.mark()
	tm.write("exit" + keyEnter) // fuzzy-filter to [exit] and run it

	tm.waitExit()
	tm.mustFrom(mark, exitedMsg)

	// The session is still alive — [exit] leaves tm, it does not end sessions.
	_, err := e.st.GetSession("aaa")
	e.Desc("the session must still exist after [exit]").E(err)
}

// TestKillCurrentSessionFromMenu drives [kill session] aimed at the session the
// menu was opened over (Ctrl-\): it ends it — shell and daemon — and drops back to
// the top-level menu with a kill notice, instead of resuming a dead session or
// leaving the terminal in a broken state.
func TestKillCurrentSessionFromMenu(t *testing.T) {
	e := setupTM(t)
	pid := e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")
	tm.menu()

	mark := tm.killCurrent()

	// The kill is noted on the terminal and the top-level menu reopens.
	tm.mustFrom(mark, killedMsg)
	tm.mustFrom(mark, menuRow)

	// Session record and daemon are gone.
	e.True(e.recordGone("aaa"))
	e.True(pidGone(pid))

	// The reopened menu is the top level: esc leaves tm.
	tm.write(keyEsc)
	tm.must(exitedMsg)
	tm.waitExit()
}

// TestKillCurrentSessionFromWithin drives the hairiest path: a tm run from INSIDE
// the session's shell kills its own session. The kill takes the shell — and that tm
// process — with it; the outer relay sees the session end and falls back to the
// top-level menu, so the terminal is never left stranded.
func TestKillCurrentSessionFromWithin(t *testing.T) {
	e := setupTM(t)
	pid := e.spawn("aaa")

	tm := e.start()
	tm.enter("aaa")

	// Make sure the shell is at its prompt before typing a command.
	tm.write("echo SH-READY" + keyEnter)
	tm.must("SH-READY")

	tm.innerTM() // run tm inside the session; its menu is framed inside aaa

	mark := tm.killCurrent()

	// The session ends out from under the inner tm: the outer relay reports the
	// exit and falls back to the top-level menu.
	tm.mustFrom(mark, exitedSess)
	tm.mustFrom(mark, menuRow)

	e.True(e.recordGone("aaa"))
	e.True(pidGone(pid))

	tm.write(keyEsc)
	tm.mustFrom(mark, exitedMsg)
	tm.waitExit()
}
