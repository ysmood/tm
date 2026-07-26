//go:build unix

package app_test

import (
	"syscall"
	"testing"
)

// TestMenuRendersUnderPTY runs the real binary inside a pseudo-terminal so Bubble
// Tea starts in full TTY mode, confirms the command palette renders, then quits
// with Ctrl-C.
func TestMenuRendersUnderPTY(t *testing.T) {
	e := setupTM(t)

	tm := e.start()
	tm.must(menuRow)

	tm.write(keyCtrlC)
	tm.waitExit()
}

// TestMenuCreateAttachDetach drives the real menu through the whole flow: create a
// session, attach, run a command in its shell, then detach — which leaves tm for
// the launching shell while the session keeps running. The assertion matches
// executed output ("ok-42" from "$((6*7))"), not the echoed input, so it proves
// the shell actually ran.
func TestMenuCreateAttachDetach(t *testing.T) {
	e := setupTM(t)

	tm := e.start()
	tm.newSession()
	tm.run("ok")

	tm.detach()
	tm.waitExit()
}

// TestNewSessionShowsInitialPrompt creates a new session through the real binary
// and asserts the shell's first prompt is visible immediately, without typing
// anything. The daemon reports ready as soon as the shell starts, which can be
// after the shell has already written its prompt into the log (common on Linux,
// rare on macOS); a new session must therefore replay its window so that captured
// prompt is shown, instead of opening to a blank screen until the user presses
// Enter.
func TestNewSessionShowsInitialPrompt(t *testing.T) {
	e := setupTM(t)

	tm := e.start()
	tm.must(menuRow)
	tm.send("ns" + keyEnter) // fuzzy-filter to [new session] and select it
	tm.must(namePrompt)

	mark := tm.mark()

	tm.write(keyEnter) // accept the default name -> spawn + attach

	// The prompt must appear on its own, with no command typed.
	tm.mustFrom(mark, testPrompt)

	tm.detach()
	tm.waitExit()
}

// TestMenuReattachCycle drives the real menu through repeated detach/re-attach
// cycles to prove a session survives detaching and can be picked back out of the
// menu's session list more than once. It creates a session and runs a command,
// then detaches, and twice more relaunches tm — each launch is the user running
// tm again, so it gets its own terminal — selects the session from the list, and
// runs another command. Each marker is executed output from the session's own
// shell, not the echoed input, so it only appears if that attach actually reached
// the still-live session.
func TestMenuReattachCycle(t *testing.T) {
	e := setupTM(t)

	tm := e.start()
	tm.newSession()
	tm.run("step1")

	// The daemon recorded the session under its generated name; the name is what
	// identifies its row when we come back to the menu.
	sessions, err := e.st.ListSessions()
	e.E(err)
	e.Len(sessions, 1)
	name := sessions[0].Name

	tm.detach()
	tm.waitExit()

	for _, marker := range []string{"step2", "step3"} {
		tm = e.start()
		tm.enter(name)
		tm.run(marker)

		tm.detach()
		tm.waitExit()
	}
}

// TestMenuReapsUnreachableSession reproduces the "can't re-enter a session, it
// bounces forever" bug and proves the fix. A session whose daemon died without a
// clean shutdown (SIGKILL here; equivalently a reboot that clears the socket dir)
// leaves a record with a stale PID. Attaching to it makes the relay fail to dial
// and exit, bouncing back to the menu. Before the fix the dead session stayed
// listed, so reselecting it bounced forever; now the failed attach reaps it and it
// drops out of the menu.
//
// To hold the menu on a doomed-but-still-listed session, the session is spawned
// outside the menu so it survives the startup prune, then killed while the menu is
// up.
func TestMenuReapsUnreachableSession(t *testing.T) {
	e := setupTM(t)
	pid := e.spawn("victim")

	tm := e.start()
	tm.must("victim") // the live session is listed in the menu

	// Kill the daemon hard while the menu is up: the record and socket file linger
	// but nothing serves, so attaching will fail to dial.
	e.E(syscall.Kill(pid, syscall.SIGKILL))
	e.True(pidGone(pid))

	tm.write(keyEnter) // pick the session -> attach -> bounces back to the menu

	// The fix: the failed attach reaps the dead session, so the menu reports it was
	// removed rather than silently re-listing it...
	tm.must("unreachable")

	// ...and it is gone from the store, so it cannot be picked again.
	left, _ := e.st.ListSessions()
	e.Desc("the dead session should have been reaped").Len(left, 0)

	tm.write(keyCtrlC)
	tm.waitExit()
}
