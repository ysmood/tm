package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
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

// Run drives the interactive menu loop. Each pass prunes dead sessions, shows
// the menu, and acts on what the user chose once it has fully torn down — so the
// inline picker is erased from the screen before the relay's output (or the
// target session's history on a switch) takes over, rather than the menu running
// the relay itself. A failed attach reaps the unreachable session and reopens the
// menu with a note; anything else (a clean detach, a switch, or a plain quit)
// leaves tm for the launching shell.
func Run() error {
	st, err := store.Open()
	if err != nil {
		return err
	}

	ctrl := &controller{st: st}

	var status string

	for {
		_ = st.Prune(sessionLive)

		m := tui.New(st, ctrl)
		if status != "" {
			m = m.WithStatus(status)
		}

		final, rerr := tea.NewProgram(m).Run()
		if rerr != nil {
			err = rerr

			break
		}

		model, ok := final.(tui.Model)
		if !ok {
			break
		}

		res := model.Result()
		if res.Action == tui.ActionAttach {
			if aerr := attach.Run(st.Paths(), res.ID, attach.Options{Hist: res.Hist, Lines: res.Lines}); aerr != nil {
				// The relay couldn't reach the daemon (a dead session). Reap it so it
				// stops reappearing in the menu — otherwise reselecting it bounces back
				// here forever — and reopen the menu with a note about what happened.
				status = afterAttachError(ctrl, aerr)

				continue
			}
		} else if res.Action == tui.ActionSwitch {
			// Inside a session: hand its relay to the target. Non-fatal — if the
			// current session's daemon can't be reached the user just stays put.
			if serr := ctrl.Switch(res.ID, res.Hist, res.Lines); serr != nil {
				fmt.Fprintln(os.Stderr, "tm: switch session:", serr)
			}
		}

		break
	}

	// No terminal reset here on purpose. The menu renders inline (never the
	// alternate screen), so Bubble Tea's own teardown leaves the terminal — and
	// its scrollback — sane on a plain quit. The attach relay resets the terminal
	// itself on detach (it is the only path that puts a session's full-screen
	// modes on the outer terminal); a switch is handled by the relay we run
	// inside. Writing TerminalRestore unconditionally here used to send a bare
	// "leave alternate screen" (\e[?1049l) even when nothing had entered it, which
	// drops the scrollback on terminals that pair rmcup with a buffer wipe.
	return err
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

// RunAttach is the entrypoint for the hidden `tm __attach` subcommand.
func RunAttach(id string, hist proto.HistMode, lines uint32) error {
	p, err := config.New()
	if err != nil {
		return err
	}

	return attach.Run(p, id, attach.Options{Hist: hist, Lines: lines})
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}
