//go:build unix

package app_test

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ysmood/tm/pkg/proto"
)

// TestSpawnDetachedDaemon spawns a detached session daemon through the real
// binary, then attaches over its socket to prove the re-exec, the readiness
// handshake and the lifecycle cleanup all work end to end.
func TestSpawnDetachedDaemon(t *testing.T) {
	e := setupTM(t)

	pid := e.spawn("d1")
	e.Desc("the daemon must record its own pid into the session record").Gt(pid, 0)

	// Attach and run a command through the persistent shell.
	nc, err := proto.Dial(proto.SockAddr(e.paths, "d1"))
	e.E(err)

	defer func() { _ = nc.Close() }()

	c := proto.NewConn(nc)
	e.E(c.Write(proto.MsgAttach, proto.Attach{Cols: ptyCols, Rows: ptyRows}.Encode()))
	e.E(c.Write(proto.MsgInput, []byte("echo spawned-ok\n")))
	e.True(readContains(nc, c, "spawned-ok"))

	// Exiting the shell ends the daemon, which removes the session's files.
	e.E(c.Write(proto.MsgInput, []byte("exit\n")))
	e.True(e.recordGone("d1"))
}

// readContains reads output frames until want shows up, or the deadline passes.
func readContains(nc net.Conn, c *proto.Conn, want string) bool {
	_ = nc.SetReadDeadline(time.Now().Add(waitTimeout))

	var acc strings.Builder

	for {
		mt, payload, err := c.Read()
		if err != nil {
			return false
		}

		if mt == proto.MsgOutput {
			acc.Write(payload)

			if strings.Contains(acc.String(), want) {
				return true
			}
		}
	}
}

// TestRelayUnderPTY runs the relay directly under a PTY (no menu) to confirm it
// forwards input and output correctly, isolating it from the menu that normally
// drives it.
func TestRelayUnderPTY(t *testing.T) {
	e := setupTM(t)
	e.spawn("rly")

	tm := e.attachRelay("rly")
	tm.run("ok")

	tm.write(keyMenu) // the bare relay has no menu loop: the key just ends it
	tm.waitExit()
}

// TestRelaySwitchesSessions proves the real relay switches sessions in place
// instead of nesting: a relay attached to session aaa is told (via aaa's daemon,
// exactly as an in-session tm would) to hand over to bbb, and afterwards the same
// terminal is driving bbb. Each session reports its identity via $TM_SESSION, so
// the WHO= markers come from the session's own shell, not the echoed input.
func TestRelaySwitchesSessions(t *testing.T) {
	e := setupTM(t)
	e.spawn("aaa", "bbb")

	tm := e.attachRelay("aaa")

	tm.write("echo WHO=$TM_SESSION" + keyEnter)
	tm.must("WHO=aaa")

	// Ask aaa's daemon to hand the relay to bbb — what controller.Switch does,
	// including the target's display name so the relay can announce the switch.
	nc, err := proto.Dial(proto.SockAddr(e.paths, "aaa"))
	e.E(err)

	conn := proto.NewConn(nc)
	e.E(conn.Write(proto.MsgSwitch, proto.SwitchTarget{ID: "bbb", Name: "bbb"}.Encode()))

	// Block until the daemon closes the connection, which is how it acknowledges
	// having forwarded the request to the relay.
	for {
		if _, _, rerr := conn.Read(); rerr != nil {
			break
		}
	}

	_ = nc.Close()

	tm.must(switchedMsg) // the relay announces the session it switched to

	tm.write("echo WHO=$TM_SESSION" + keyEnter)
	tm.must("WHO=bbb")

	tm.write(keyMenu)
	tm.waitExit()
}

// TestReattachReplaysHistoryPastClear drives the real binary: a session records
// content, then a `clear`, then more content; re-attaching must still replay the
// pre-clear content (and must not replay the screen/scrollback clears that would
// wipe it). `clear` emits ED3+ED2, so before the log was cooked to visible text a
// single `clear` made the replay come back blank — the screen cleared instead of
// showing the history.
func TestReattachReplaysHistoryPastClear(t *testing.T) {
	e := setupTM(t)
	e.spawn("clr")

	// First attach: produce content, clear, then more content.
	first := e.attachRelay("clr")

	first.write("echo BEFORE-CLEAR-MARK" + keyEnter)
	first.must("BEFORE-CLEAR-MARK")

	first.send("clear" + keyEnter)

	first.write("echo AFTER-CLEAR-MARK" + keyEnter)
	first.must("AFTER-CLEAR-MARK")

	first.write(keyMenu) // detach; the session keeps running
	first.waitExit()

	// Re-attach: the replay redraws the session's last window from its log.
	second := e.start("__attach", "--id", "clr")
	second.must("AFTER-CLEAR-MARK")

	replay := second.out.String()

	// The pre-clear content the user wants to scroll back to is present...
	second.Desc("replay must contain pre-clear history: %q", replay).
		True(strings.Contains(replay, "BEFORE-CLEAR-MARK"))
	// ...and the screen/scrollback clears that would have wiped it are gone.
	second.Desc("replay must not re-run ED3 (clear scrollback): %q", replay).
		False(strings.Contains(replay, "\x1b[3J"))
	second.Desc("replay must not re-run ED2 (clear screen): %q", replay).
		False(strings.Contains(replay, "\x1b[2J"))

	second.write(keyMenu)
	second.waitExit()
}

// TestHistoryShowsScreen drives the real binary: /history pages the session's
// log followed by what its screen shows now, so output that hasn't scrolled off
// the screen yet — not in the log — is still in the history.
func TestHistoryShowsScreen(t *testing.T) {
	e := setupTM(t)
	e.spawn("hst")

	first := e.attachRelay("hst")
	first.write("echo ON-SCREEN-$((1+1))" + keyEnter) // the echo of the input can't match
	first.must("ON-SCREEN-2")
	first.write(keyMenu) // detach; the session keeps running
	first.waitExit()

	log, err := os.ReadFile(e.st.Paths().LogFile("hst"))
	e.E(err)
	e.Desc("the marker must still be on screen, not in the log").
		False(strings.Contains(string(log), "ON-SCREEN-2"))

	// less refuses to page on a terminal it can't identify (it stops to warn
	// first), and the test environment may have no TERM; a user's terminal has one.
	e.env = append(e.env, "TERM=xterm-256color")

	tm := e.start()
	tm.must(renameRow) // the menu is up with its command list

	chooser := tm.mark()
	tm.write("/hi" + keyEnter) // fuzzy-filter to /history (not /clear-history) and select it
	tm.mustFrom(chooser, "hst")

	pager := tm.mark()
	tm.write(keyEnter) // pick the session -> less opens on its history
	tm.mustFrom(pager, "ON-SCREEN-2")

	menu := tm.mark()
	tm.write("q") // quit less, back to the menu
	tm.mustFrom(menu, renameRow)
	tm.write(keyEsc)
	tm.waitExit()
}
