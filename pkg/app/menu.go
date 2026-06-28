package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/ysmood/tm/pkg/attach"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/naming"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
	"github.com/ysmood/tm/pkg/tui"
	"golang.org/x/term"
)

// controller implements tui.Controller using the store and process spawning.
type controller struct{ st *store.Store }

// AttachCmd builds the `tm __attach` relay command for a session.
func (c *controller) AttachCmd(id string, hist proto.HistMode, lines uint32) *exec.Cmd {
	self, _ := os.Executable()

	return exec.Command(self, "__attach",
		"--id", id,
		"--hist", strconv.Itoa(int(hist)),
		"--lines", strconv.Itoa(int(lines)),
	)
}

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

// Run launches the interactive menu. It prunes dead sessions on entry.
func Run() error {
	st, err := store.Open()
	if err != nil {
		return err
	}

	_ = st.Prune(sessionLive)

	prog := tea.NewProgram(tui.New(st, &controller{st: st}))
	_, err = prog.Run()

	// The menu runs the relay via tea.ExecProcess, which re-enters the alternate
	// screen when the relay returns and then tears the menu down — a path that does
	// not reliably restore terminal state, so the relay's own reset on detach can be
	// clobbered. Have the last word here, once Bubble Tea is fully done, so the
	// terminal (and its scrollback) is left sane on the way back to the shell.
	if term.IsTerminal(int(os.Stdout.Fd())) {
		_, _ = os.Stdout.Write(attach.TerminalRestore)
	}

	return err
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
