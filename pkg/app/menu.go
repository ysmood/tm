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

	tgt := proto.SwitchTarget{ID: id, Hist: hist, Lines: lines}
	if err := conn.Write(proto.MsgSwitch, tgt.Encode()); err != nil {
		return err
	}

	// Block until the daemon has forwarded the request and closed, so the switch
	// is delivered before this menu exits.
	_, _, _ = conn.Read()

	return nil
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
// another switches to it, and [detach session] leaves tm for the launching shell.
// Exiting the session's shell ends the session and drops back to the top-level
// menu (rather than leaving tm), so the user can pick or start another session.
// A failed attach reaps the unreachable session and reopens the menu with a note.
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
// reports leave=true when the user chose to leave tm. curID is the session the
// menu was framed as running inside ("" at the top level).
//
// A picked session (attach or switch — in the relay-menu both just resolve to a
// target on this terminal) yields that target. esc/Ctrl-C (ActionNone) leaves tm
// at the top level but resumes the current session when reopened from one,
// replaying nothing (HistNone) so it doesn't reprint a screen of history over the
// session's still-visible output. [detach session] leaves tm with every session
// still running, first resetting the terminal if we came from a session so the
// launching shell gets it clean.
func pickTarget(res tui.Result, curID string, curAlt bool) (targetID string, hist proto.HistMode, lines uint32, leave bool) {
	switch res.Action {
	case tui.ActionAttach, tui.ActionSwitch:
		return res.ID, res.Hist, res.Lines, false
	case tui.ActionNone:
		if curID == "" {
			return "", 0, 0, true
		}

		return curID, proto.HistNone, 0, false
	case tui.ActionDetach:
		if curID != "" {
			// Detaching leaves tm for the launching shell. Send the alt-screen exit
			// only when the session was on the alt screen (curAlt), so detaching at a
			// plain shell prompt keeps the terminal's scrollback intact.
			_, _ = os.Stdout.Write(attach.RestoreFor(curAlt))
		}

		return "", 0, 0, true
	}

	return "", 0, 0, true
}

func runMenu(st *store.Store, ctrl *controller) error {
	var (
		status  string
		curID   string // session the relay is (or just was) on; "" at the top level
		curName string
		curAlt  bool // the relay left that session on the alternate screen
	)

	for {
		// Keep the inline picker from swallowing the shell's prompt (see promptGuard).
		guard := openMenuOverSession(curID)

		res, err := showMenu(st, ctrl, status, curID, curName)
		if err != nil {
			return err
		}

		guard.restorePrompt()

		status = ""

		targetID, hist, lines, leave := pickTarget(res, curID, curAlt)
		if leave {
			return nil
		}

		// Switching to a different session: the relay left this session's screen up
		// so the menu could open inline over it, so reset the terminal (leave the
		// alternate screen, mouse modes, scroll region, …) and return to column 0
		// before the target's history replays, so the replay lands from a known
		// column. Resuming the same session keeps the screen as-is (that is the point
		// of the inline menu), and the first attach from a clean shell has nothing to undo.
		if curID != "" && targetID != curID {
			_, _ = os.Stdout.Write(attach.SwitchReset)
		}

		outcome, alt, aerr := attach.Run(st.Paths(), targetID, attach.Options{Hist: hist, Lines: lines})
		if aerr != nil {
			// The relay couldn't reach the daemon (a dead session). Reap it so it
			// stops reappearing in the menu — otherwise reselecting it bounces back
			// here forever — and reopen the top-level menu with a note.
			status = afterAttachError(ctrl, aerr)
			curID, curName, curAlt = "", "", false

			continue
		}

		if outcome == attach.OutcomeMenu {
			// Ctrl-\: reopen the menu framed as in-session, so esc resumes and a pick
			// switches. Remember whether the session left the terminal on the alt
			// screen, so a following [detach session] resets it correctly.
			curID, curName, curAlt = targetID, sessionName(st, targetID), alt

			continue
		}

		if outcome == attach.OutcomeSessionExited {
			// The session's shell exited, so the session is gone (its daemon removed
			// it). Fall back to the top-level menu instead of leaving tm, so the user
			// can pick or start another session. The relay already reset the terminal
			// on its way out, so the menu draws on a clean screen.
			curID, curName, curAlt = "", "", false

			continue
		}

		return nil // local input ended (the terminal went away): leave tm
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
func (g promptGuard) restorePrompt() {
	if g.active {
		_, _ = os.Stdout.Write([]byte("\x1b8")) // DECRC
	}
}

// runInSession handles tm launched from inside a session's shell. The menu here
// drives the outer relay: picking another session asks the current session's
// daemon to hand that relay over (a switch), while esc or [detach session] just
// leaves this inner tm, dropping back into the session. There is no relay to run
// in this process, so unlike runMenu it never attaches.
func runInSession(ctrl *controller) error {
	res, err := showMenu(ctrl.st, ctrl, "", "", "")
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

	return nil
}

// showMenu prunes dead sessions, runs the interactive menu once, and returns what
// the user chose. A non-empty curID frames the menu as running inside that session
// even though this process has no $TM_SESSION — runMenu uses it to reopen the menu
// as in-session after Ctrl-\.
func showMenu(st *store.Store, ctrl *controller, status, curID, curName string) (tui.Result, error) {
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
		return tui.Result{}, err
	}

	model, ok := final.(tui.Model)
	if !ok {
		return tui.Result{}, nil
	}

	return model.Result(), nil
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
// detach); the full menu-on-Ctrl-\ flow lives in Run/runMenu.
func RunAttach(id string, hist proto.HistMode, lines uint32) error {
	p, err := config.New()
	if err != nil {
		return err
	}

	_, _, err = attach.Run(p, id, attach.Options{Hist: hist, Lines: lines})

	return err
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}
