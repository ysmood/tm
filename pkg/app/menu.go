package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/ysmood/tm/pkg/attach"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/naming"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
	"github.com/ysmood/tm/pkg/tui"
)

// controller implements tui.Controller using the store and process spawning.
// Attaching, switching and reaping are driven by Run (not the menu), so they are
// plain methods here rather than part of tui.Controller.
type controller struct{ st *store.Store }

// CurrentSession returns the id and name of the session this tm is running
// inside, or "", "" if tm was not launched from within a session's shell. It
// reads the session marker the daemon sets in every session shell's environment,
// then resolves the name from the store (returning "", "" if the session no
// longer exists).
func (c *controller) CurrentSession() (id, name string) {
	id = os.Getenv(config.EnvSession)
	if id == "" {
		return "", ""
	}

	s, err := c.st.GetSession(id)
	if err != nil {
		return "", ""
	}

	return id, s.Name
}

// Switch hands the relay of the session this tm is running inside over to another
// session, so selecting a session from within one moves this terminal there
// instead of nesting a new relay. It dials the current session's daemon, sends a
// switch request, and waits for the daemon to forward it (signalled by the daemon
// closing the connection). It is only meaningful when CurrentSession() != "".
func (c *controller) Switch(id string, hist proto.HistMode, lines uint32) error {
	cur := os.Getenv(config.EnvSession)
	if cur == "" {
		return errors.New("not running inside a session")
	}

	nc, err := proto.Dial(proto.SockAddr(c.st.Paths(), cur))
	if err != nil {
		return err
	}

	defer func() { _ = nc.Close() }()

	conn := proto.NewConn(nc)

	tgt := proto.SwitchTarget{ID: id, Name: sessionName(c.st, id), Hist: hist, Lines: lines}
	if err := conn.Write(proto.MsgSwitch, tgt.Encode()); err != nil {
		return err
	}

	// Block until the daemon has forwarded the request and closed, so the switch
	// is delivered before this menu exits.
	_ = awaitClose(conn)

	return nil
}

// awaitClose blocks until the daemon closes conn (or a read deadline set on the
// underlying connection expires), discarding any frames and returning the read
// error that ended the wait. The control requests (switch, kill) are
// acknowledged by the daemon closing the connection once it has acted, so
// waiting for the close is how a sender knows its request was carried out.
func awaitClose(conn *proto.Conn) error {
	for {
		if _, _, err := conn.Read(); err != nil {
			return err
		}
	}
}

// DefaultSessionName proposes a unique default name for a new session in ns.
func (c *controller) DefaultSessionName(ns string) string {
	cwd, _ := os.Getwd()
	base := naming.Generate(cwd, time.Now())
	taken := map[string]bool{}

	if sessions, err := c.st.ListByNamespace(ns); err == nil {
		for _, s := range sessions {
			taken[s.Name] = true
		}
	}

	return naming.Unique(base, taken)
}

// CreateAndSpawn writes a new session record and starts its detached daemon.
func (c *controller) CreateAndSpawn(ns, name string) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}

	cwd, _ := os.Getwd()

	sess := store.Session{
		ID:        id,
		Name:      name,
		Namespace: ns,
		Shell:     shellPath(),
		Cwd:       cwd,
		CreatedAt: time.Now(),
	}
	if err := c.st.SaveSession(sess); err != nil {
		return "", err
	}

	if err := Spawn(c.st.Paths(), sess); err != nil {
		_ = c.st.DeleteSession(id)

		return "", err
	}

	return id, nil
}

// killTimeout bounds how long KillSession waits for a daemon to tear down. A
// wedged daemon — say, blocked writing output to a suspended client — would
// otherwise hang the menu forever. A var so tests can shorten it.
var killTimeout = 5 * time.Second

// KillSession ends a session by asking its daemon to shut down, which kills the
// shell and removes the session's files. It blocks (bounded by killTimeout)
// until the daemon closes the connection — teardown is done — so the menu
// rebuilds its list only after the session is gone. A session whose daemon
// can't be asked at all falls back to killUnreachable.
func (c *controller) KillSession(id string) error {
	nc, err := proto.Dial(proto.SockAddr(c.st.Paths(), id))
	if err != nil {
		return c.killUnreachable(id)
	}

	defer func() { _ = nc.Close() }()

	conn := proto.NewConn(nc)
	if err := conn.Write(proto.MsgKill, nil); err != nil {
		return c.killUnreachable(id)
	}

	// On timeout the record is left in place: the daemon may still be mid-teardown,
	// and if it truly is wedged the session is at least still visible (and
	// retryable) rather than silently orphaned.
	_ = nc.SetReadDeadline(time.Now().Add(killTimeout))

	if err := awaitClose(conn); errors.Is(err, os.ErrDeadlineExceeded) {
		return errors.New("timed out waiting for the session to shut down")
	}

	return nil
}

// killUnreachable handles killing a session whose daemon can't be asked to shut
// down (the dial or the kill request failed). Only a session whose process is
// gone is treated as dead and removed from the store — mirroring Reap — because
// a live daemon can lose its socket file (e.g. to a /tmp cleaner pruning the
// runtime dir) while it and its shell run on; deleting the record then would
// orphan them untracked.
func (c *controller) killUnreachable(id string) error {
	sess, err := c.st.GetSession(id)
	if err != nil {
		return err
	}

	if sessionLive(sess) {
		return errors.New("session is alive but unreachable (pid " + strconv.Itoa(sess.PID) + ")")
	}

	return c.st.DeleteSession(id)
}

// sessionLive reports whether a session's daemon is still running. A
// not-yet-recorded PID (<= 0, set only briefly while a session is spawning)
// counts as live so a session mid-spawn is never reaped.
func sessionLive(s store.Session) bool { return s.PID <= 0 || processAlive(s.PID) }

// Reap drops sessions whose daemon is no longer running and reports how many it
// removed. The menu calls it after a failed attach: a session whose daemon was
// killed (or whose socket vanished, e.g. after a reboot) lingers in the store
// with a stale PID, so attaching bounces straight back to the menu — and
// without reaping it the user can reselect it and bounce forever.
func (c *controller) Reap() int {
	before, _ := c.st.ListSessions()
	_ = c.st.Prune(sessionLive)
	after, _ := c.st.ListSessions()

	return len(before) - len(after)
}

// Run is the entrypoint for tm with no arguments. Launched from a plain shell it
// owns the terminal and runs the menu loop (runMenu); launched from inside a
// session's shell it instead drives that session's existing relay (runInSession).
func Run() error {
	st, err := store.Open()
	if err != nil {
		return err
	}

	ctrl := &controller{st: st}

	// Inside a session this tm has no relay of its own: picking a session hands the
	// outer relay over via the daemon, so there is no menu/relay loop to run here.
	if os.Getenv(config.EnvSession) != "" {
		return runInSession(ctrl)
	}

	return runMenu(st, ctrl)
}

// runMenu owns the terminal for a tm launched from a plain shell, alternating
// between the interactive menu and the relay. Picking a session attaches the
// relay; pressing the menu key (Ctrl-\) inside the session returns here and
// reopens the menu — framed as in-session — so esc resumes that session, picking
// another switches to it, and [detach session] drops back to the top-level menu
// with the session still running (esc there leaves tm). Exiting the session's
// shell likewise ends the session and drops back to the top-level menu, so the
// user can pick or start another session. A failed attach reaps the unreachable
// session and reopens the menu with a note.
//
// The menu always acts only once it has fully torn down (the inline picker is
// erased before the relay's output or a history replay takes over), so the menu
// never runs the relay itself.
//
// It does no terminal reset on the way out: the menu renders inline (never the
// alternate screen), so Bubble Tea's teardown leaves the terminal — and its
// scrollback — sane, and the relay resets the terminal itself whenever it hands
// it back.
// pickTarget maps a menu result to the session the relay should (re)attach, or
// reports leave=true (return to the launching shell) or toMenu=true (drop to the
// top-level menu). curID is the session the menu was framed as running inside
// ("" at the top level).
//
// A picked session (attach or switch — in the relay-menu both just resolve to a
// target on this terminal) yields that target. esc/Ctrl-C (ActionNone) leaves tm
// at the top level but resumes the current session when reopened from one,
// replaying nothing (HistNone) so it doesn't reprint a screen of history over the
// session's still-visible output. [detach session] from within a session returns
// to the top-level menu (toMenu) with every session still running, first
// resetting the terminal so the menu redraws clean; at the top level, where there
// is no session to detach from, it leaves tm like esc.
func pickTarget(
	res tui.Result, curID string, curAlt bool,
) (targetID string, hist proto.HistMode, lines uint32, leave, toMenu bool) {
	switch res.Action {
	case tui.ActionAttach, tui.ActionSwitch:
		return res.ID, res.Hist, res.Lines, false, false
	case tui.ActionNone:
		if curID == "" {
			return "", 0, 0, true, false
		}

		return curID, proto.HistNone, 0, false, false
	case tui.ActionDetach:
		if curID != "" {
			// Detaching from a session returns to the top-level menu (the session
			// keeps running), not out of tm. Reset the terminal first — sending the
			// alt-screen exit only when the session was on the alt screen (curAlt), so
			// detaching at a plain shell prompt keeps the scrollback — so the top-level
			// menu redraws on a clean screen, the way a session switch does.
			_, _ = os.Stdout.Write(attach.RestoreFor(curAlt))

			return "", 0, 0, false, true
		}

		return "", 0, 0, true, false
	case tui.ActionExit:
		// [exit] (Ctrl-D): leave tm from anywhere. When leaving from within a
		// session, reset the terminal for the launching shell first — the alt-screen
		// exit only when the session was on it (curAlt), so exiting at a plain shell
		// prompt keeps the scrollback intact.
		if curID != "" {
			_, _ = os.Stdout.Write(attach.RestoreFor(curAlt))
		}

		return "", 0, 0, true, false
	}

	return "", 0, 0, true, false
}

func runMenu(st *store.Store, ctrl *controller) error {
	var (
		status  string
		curID   string // session the relay is (or just was) on; "" at the top level
		curName string
		curAlt  bool // the relay left that session on the alternate screen
		// paused is the attachment suspended mid-history-replay when the menu key
		// opened this menu (nil otherwise): esc resumes it in place, any other
		// choice aborts it so the rest of the history is never loaded.
		paused *attach.Paused
	)

	for {
		// Keep the inline picker from swallowing the shell's prompt (see promptGuard).
		guard := openMenuOverSession(curID)

		res, notices, err := showMenu(st, ctrl, status, curID, curName)
		if err != nil {
			return err
		}

		guard.restorePrompt(notices)

		status = ""

		// [rename session] may have renamed the session this menu was framed as
		// running inside, so re-read its name before the notices below quote it.
		if curID != "" {
			curName = sessionName(st, curID)
		}

		// [kill session] aimed at the session this menu sits over: handled before
		// pickTarget since it is neither an attach nor a plain way out — the session
		// (and whatever of it is on this screen) is being ended deliberately.
		if res.Action == tui.ActionKillCurrent {
			next := killCurrentSession(ctrl, paused, curID, curName, curAlt)
			paused = nil
			curID, curName, curAlt, status = next.curID, next.curName, next.curAlt, next.status

			continue
		}

		targetID, hist, lines, leave, toMenu := pickTarget(res, curID, curAlt)

		paused = settlePaused(paused, targetID, curID, leave, toMenu)

		if toMenu || leave {
			noteMenuExit(curName, toMenu)

			if leave {
				return nil
			}

			curID, curName, curAlt = "", "", false

			continue
		}

		targetName := sessionName(st, targetID)
		announceAttach(curID, targetID, targetName)

		outcome, alt, nowPaused, aerr := runOrResume(st, paused, targetID, targetName, hist, lines)
		paused = nowPaused

		next, done := afterRelay(st, ctrl, targetID, alt, outcome, paused, aerr)
		if done {
			return nil // local input ended (the terminal went away): leave tm
		}

		curID, curName, curAlt, status = next.curID, next.curName, next.curAlt, next.status
	}
}

// menuState is the framing the menu loop reopens with after a relay run: the
// session the menu sits over (empty at the top level) and a status note.
type menuState struct {
	curID   string
	curName string
	curAlt  bool
	status  string
}

// afterRelay folds a relay run's outcome into the menu's next framing; done
// reports that local input ended (the terminal went away), so tm should leave.
//
// A relay error means the daemon was unreachable (a dead session): reap it so
// it stops reappearing in the menu — otherwise re-selecting it bounces back
// forever — and note it on the reopened top-level menu. OutcomeMenu (Ctrl-\)
// reopens the menu framed as in-session, so esc resumes and a pick switches,
// remembering whether the session left the terminal on the alt screen so a
// following [detach session] resets it correctly; a non-nil paused means the
// key landed mid-history-replay, which the status says (esc then resumes the
// replay). OutcomeSessionExited falls back to the top-level menu — the session
// is gone and its relay already reset the terminal — so the user can pick or
// start another session instead of leaving tm. OutcomeInterrupted (Ctrl-C
// mid-history-replay) likewise falls back to the top-level menu, but the
// session is still running: the user aborted loading its history, so they can
// re-enter it (perhaps with less history) or pick another one.
func afterRelay(
	st *store.Store, ctrl *controller, targetID string, alt bool,
	outcome attach.Outcome, paused *attach.Paused, aerr error,
) (menuState, bool) {
	switch {
	case aerr != nil:
		return menuState{status: afterAttachError(ctrl, aerr)}, false
	case outcome == attach.OutcomeMenu:
		next := menuState{curID: targetID, curName: sessionName(st, targetID), curAlt: alt}
		if paused != nil {
			next.status = "history replay paused"
		}

		return next, false
	case outcome == attach.OutcomeSessionExited:
		return menuState{}, false
	case outcome == attach.OutcomeInterrupted:
		// Ctrl-C mid-history-replay: the relay aborted the replay (the session
		// keeps running) and already reset the terminal, so fall back to the
		// top-level menu like a session exit, noting why.
		return menuState{status: "history replay canceled"}, false
	default:
		return menuState{}, true
	}
}

// noteMenuExit writes the scrollback notice for the plain ways out of the menu
// loop. Detaching from a session ([detach session] inside one) drops to the
// top-level menu with the session still running rather than leaving tm —
// pickTarget already reset the terminal, and the note keeps the detached
// session's name in the scrollback above the reopened menu (whose fresh line
// the notice's trailing newline also provides). Otherwise tm is left for the
// launching shell ([exit] / Ctrl-D, esc or top-level [detach session]), and the
// departure is noted so it stays in the scrollback.
func noteMenuExit(curName string, toMenu bool) {
	if toMenu {
		_, _ = os.Stdout.Write(attach.DetachedSessionNotice(curName))

		return
	}

	_, _ = os.Stdout.Write(attach.ExitedNotice())
}

// killCurrentSession ends the session the menu was framed inside and returns
// the menu's next framing. Any replay paused on it is aborted first — the rest
// of the history will never be shown, and holding the attachment would wedge
// the daemon's teardown. On success the terminal is reset the way the
// session-exited path does (the relay left the session's screen up for the
// menu) and the kill is noted, so the top-level menu reopens on a clean screen
// with the notice in the scrollback above it. On failure the session may still
// be running, so the menu stays framed inside it with the reason in its header.
func killCurrentSession(
	ctrl *controller, paused *attach.Paused, curID, curName string, curAlt bool,
) menuState {
	if paused != nil {
		paused.Abort()
	}

	if err := ctrl.KillSession(curID); err != nil {
		return menuState{
			curID: curID, curName: curName, curAlt: curAlt,
			status: "failed to kill session: " + err.Error(),
		}
	}

	_, _ = os.Stdout.Write(attach.RestoreFor(curAlt))
	_, _ = os.Stdout.Write(attach.KilledCurrentSessionNotice(curName))

	return menuState{}
}

// settlePaused decides whether the menu's outcome resumes a replay the menu key
// paused (esc back into the same session), returning the attachment when it
// does and aborting it otherwise: any other way out abandons the replay, so the
// daemon stops streaming history that would never be shown. A non-nil result
// therefore means "resume this" (see runOrResume).
func settlePaused(paused *attach.Paused, targetID, curID string, leave, toMenu bool) *attach.Paused {
	if paused == nil {
		return nil
	}

	if !leave && !toMenu && targetID == curID {
		return paused
	}

	paused.Abort()

	return nil
}

// runOrResume moves the relay onto targetID: continuing the paused replay on
// its suspended attachment — right where it stopped — when settlePaused kept
// one, and dialing a fresh relay otherwise.
func runOrResume(
	st *store.Store, paused *attach.Paused,
	targetID, targetName string, hist proto.HistMode, lines uint32,
) (attach.Outcome, bool, *attach.Paused, error) {
	if paused != nil {
		return paused.Resume()
	}

	return attach.Run(st.Paths(), targetID, attach.Options{Hist: hist, Lines: lines, Name: targetName})
}

// announceAttach writes the terminal reset and status notice for moving the relay
// onto targetName. Switching to a different session (curID set and != target): the
// relay left this session's screen up so the menu could open inline over it, so
// reset the terminal (leave the alternate screen, mouse modes, scroll region, …)
// and return to column 0 before the target's history replays, so the replay lands
// from a known column. Entering a session from the top-level menu (curID == "") is
// a fresh attach with nothing to undo. Resuming the same session (curID ==
// targetID) keeps the screen as-is — the point of the inline menu — so it says
// nothing.
func announceAttach(curID, targetID, targetName string) {
	switch {
	case curID != "" && targetID != curID:
		_, _ = os.Stdout.Write(attach.SwitchReset)
		_, _ = os.Stdout.Write(attach.SwitchedNotice(targetName))
	case curID == "":
		_, _ = os.Stdout.Write(attach.EnteredNotice(targetName))
	}
}

// promptGuard keeps the inline menu from swallowing the shell's prompt when it
// opens over a live session (Ctrl-\). Bubble Tea's inline picker takes over the
// line the cursor is on — the prompt line — and erases that whole block when it
// tears down, so without this the prompt vanishes on esc. The zero value is an
// inactive guard (used at the top level), whose methods are no-ops so the menu
// opens exactly as before.
type promptGuard struct {
	active bool
}

// openMenuOverSession arms a guard for a menu about to be drawn over the live
// session curID: it saves the cursor (DECSC) and pushes the picker onto a fresh
// line below the prompt, so the picker erases only its own lines. The inline
// picker never enters the alternate screen, so Bubble Tea leaves the saved cursor
// alone (it only saves/restores the cursor when toggling the alt screen). Inactive
// at the top level (curID == ""), where there is no prompt to protect.
func openMenuOverSession(curID string) promptGuard {
	if curID == "" {
		return promptGuard{}
	}

	_, _ = os.Stdout.Write([]byte("\x1b7\n")) // DECSC, then a spacer line for the picker

	return promptGuard{active: true}
}

// restorePrompt returns the cursor to where the prompt left it (DECRC) once the
// picker has torn down. The picker drew (and erased) below the saved position, so
// the prompt itself is intact and resuming the session leaves the screen exactly
// as it was, the way fzf does on esc. A switch or detach reuses the same restore;
// the target's replay (or the launching shell) then takes the screen from there.
//
// Notices the menu printed (renames) need more work: Bubble Tea inserts them
// directly above its picker, and the guard put the picker below the prompt — so
// they land between the prompt and the picker, where the resumed session's very
// next output would overwrite them, with the cursor sitting confusingly one line
// above them. So they are moved: the notice rows below the prompt are deleted,
// blank rows are inserted above the prompt (pushing it down), the notices are
// rewritten there, and the cursor ends back on the prompt — leaving the notices
// in the scrollback above it, the same trail the attach/detach notices leave.
// Everything is cursor-relative: only the terminal's DECSC register knows where
// the prompt is.
func (g promptGuard) restorePrompt(notices []string) {
	if !g.active {
		return
	}

	if len(notices) == 0 {
		_, _ = os.Stdout.Write([]byte("\x1b8")) // DECRC

		return
	}

	k := strconv.Itoa(len(notices))

	var b strings.Builder

	// The final cursor target — the prompt's row once the notices are inserted
	// above it, at the prompt's original column — is re-saved (DECSC) before any
	// line surgery: inserting and deleting lines moves the cursor to the first
	// column on VT/xterm-family terminals, so the column only survives the
	// surgery inside the terminal's save register.
	b.WriteString("\x1b8")           // DECRC: onto the prompt row
	b.WriteString("\x1b[" + k + "B") // down to where the prompt is about to land
	b.WriteString("\x1b7")           // DECSC: re-save — the session resumes here
	b.WriteString("\x1b[" + k + "A") // back up onto the prompt row
	b.WriteString("\x1b[B")          // down onto the first notice row
	b.WriteString("\x1b[" + k + "M") // delete the notice rows below the prompt
	b.WriteString("\x1b[A")          // up onto the prompt row
	b.WriteString("\x1b[" + k + "L") // insert blanks above it, pushing the prompt down
	b.WriteString("\r")              // column 0 for the rewrite (IL may already have)

	for i, n := range notices {
		if i > 0 {
			b.WriteString("\r\n")
		}

		b.WriteString(n)
	}

	b.WriteString("\x1b8") // DECRC: cursor right after the prompt

	_, _ = os.Stdout.Write([]byte(b.String()))
}

// runInSession handles tm launched from inside a session's shell. The menu here
// drives the outer relay: picking another session asks the current session's
// daemon to hand that relay over (a switch), while esc or [detach session] just
// leaves this inner tm, dropping back into the session. There is no relay to run
// in this process, so unlike runMenu it never attaches.
func runInSession(ctrl *controller) error {
	res, _, err := showMenu(ctrl.st, ctrl, "", "", "")
	if err != nil {
		return err
	}

	if res.Action == tui.ActionSwitch {
		// Hand the relay to the target. Non-fatal — if the current session's daemon
		// can't be reached the user just stays put.
		if serr := ctrl.Switch(res.ID, res.Hist, res.Lines); serr != nil {
			fmt.Fprintln(os.Stderr, "tm: switch session:", serr)
		}
	}

	if res.Action == tui.ActionKillCurrent {
		// Killing the session this tm runs inside takes its shell — and with it
		// this very process — down; the outer relay sees the session end and falls
		// back to its menu. Error reporting is best-effort: on success this process
		// is usually gone before KillSession even returns.
		if kerr := ctrl.KillSession(res.ID); kerr != nil {
			fmt.Fprintln(os.Stderr, "tm: kill session:", kerr)
		}
	}

	return nil
}

// showMenu prunes dead sessions, runs the interactive menu once, and returns what
// the user chose plus the notices the menu printed above its picker (renames),
// which runMenu repositions when the menu sat over a session (see promptGuard).
// A non-empty curID frames the menu as running inside that session even though
// this process has no $TM_SESSION — runMenu uses it to reopen the menu as
// in-session after Ctrl-\.
func showMenu(st *store.Store, ctrl *controller, status, curID, curName string) (tui.Result, []string, error) {
	_ = st.Prune(sessionLive)

	m := tui.New(st, ctrl)
	if ns := strings.TrimSpace(os.Getenv(config.EnvNamespace)); ns != "" {
		m = m.WithNamespace(ns)
	}

	if curID != "" {
		m = m.WithCurrentSession(curID, curName)
	}

	if status != "" {
		m = m.WithStatus(status)
	}

	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return tui.Result{}, nil, err
	}

	model, ok := final.(tui.Model)
	if !ok {
		return tui.Result{}, nil, nil
	}

	return model.Result(), model.Notices(), nil
}

// sessionName resolves a session id to its name for the in-session header, or ""
// if the session is gone.
func sessionName(st *store.Store, id string) string {
	s, err := st.GetSession(id)
	if err != nil {
		return ""
	}

	return s.Name
}

// afterAttachError reaps the session whose relay just failed to connect and
// returns the status to show on the reopened menu.
func afterAttachError(ctrl *controller, relayErr error) string {
	if n := ctrl.Reap(); n > 0 {
		return "removed " + reapNoun(n)
	}

	return "session ended: " + relayErr.Error()
}

// reapNoun renders a count of removed-because-unreachable sessions for the status
// line, e.g. "1 unreachable session" or "3 unreachable sessions".
func reapNoun(n int) string {
	if n == 1 {
		return "1 unreachable session"
	}

	return strconv.Itoa(n) + " unreachable sessions"
}

// RunAttach is the entrypoint for the hidden `tm __attach` subcommand. It runs a
// bare relay with no menu loop, so the menu key just ends it (like the old
// detach) — a replay paused by the key is aborted, since there is no menu to
// resume from; the full menu-on-Ctrl-\ flow lives in Run/runMenu.
func RunAttach(id string, hist proto.HistMode, lines uint32) error {
	p, err := config.New()
	if err != nil {
		return err
	}

	_, _, paused, err := attach.Run(p, id, attach.Options{Hist: hist, Lines: lines})
	if paused != nil {
		paused.Abort()
	}

	return err
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}
