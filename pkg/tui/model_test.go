package tui

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
)

// fakeCtrl is a no-op Controller for driving the model in tests.
type fakeCtrl struct{}

func (fakeCtrl) AttachCmd(string, proto.HistMode, uint32) *exec.Cmd { return exec.Command("true") }
func (fakeCtrl) CreateAndSpawn(string, string) (string, error)      { return "id", nil }
func (fakeCtrl) CurrentSession() (string, string)                   { return "", "" }
func (fakeCtrl) Switch(string, proto.HistMode, uint32) error        { return nil }
func (fakeCtrl) DefaultSessionName(string) string                   { return "default-name" }
func (fakeCtrl) Reap() int                                          { return 0 }

// sessCtrl reports a current-session id and name, simulating a tm launched from
// within a session's shell, and records the id of any switch it is asked to
// perform.
type sessCtrl struct {
	fakeCtrl
	id         string
	name       string
	switchedTo *string
}

func (c sessCtrl) CurrentSession() (string, string) { return c.id, c.name }

func (c sessCtrl) Switch(id string, _ proto.HistMode, _ uint32) error {
	*c.switchedTo = id

	return nil
}

func newStore(g got.G, t *testing.T) *store.Store {
	p := config.Paths{Home: t.TempDir(), Runtime: t.TempDir()}
	g.E(p.EnsureDirs())

	return store.New(p)
}

func send(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)

	return next.(Model)
}

func typeStr(m Model, s string) Model {
	for _, r := range s {
		m = send(m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	return m
}

var (
	keyEnterMsg = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyDownMsg  = tea.KeyPressMsg{Code: tea.KeyDown}
)

func has(list []string, v string) bool {
	return slices.Contains(list, v)
}

// reapCtrl is a Controller whose Reap prunes a backing store, so tests can
// observe the menu dropping a dead session after a failed attach.
type reapCtrl struct {
	fakeCtrl
	st     *store.Store
	called bool
}

func (c *reapCtrl) Reap() int {
	c.called = true

	before, _ := c.st.ListSessions()
	// All sessions in this fake are treated as dead (PID 0 stays, real PIDs go);
	// the test seeds a dead one to be removed.
	_ = c.st.Prune(func(s store.Session) bool { return false })
	after, _ := c.st.ListSessions()

	return len(before) - len(after)
}

// sessionRows counts the session (non-command) rows currently in the menu.
func sessionRows(m Model) int {
	n := 0

	for _, it := range m.pick.all {
		if p, ok := it.payload.(menuPayload); ok && !p.isCmd {
			n++
		}
	}

	return n
}

// A failed attach (the relay couldn't reach the daemon) reaps the dead session
// so it drops out of the menu instead of luring the user into an endless
// select-bounce loop.
func TestModelReapsDeadSessionOnAttachError(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "dead", Name: "dead", Namespace: store.DefaultNamespace}))

	ctrl := &reapCtrl{st: st}
	m := New(st, ctrl)
	g.Eq(sessionRows(m), 1) // the session shows up at first

	// Simulate the relay exiting with an error (e.g. "connection refused").
	m = send(m, relayDoneMsg{err: errors.New("connection refused")})

	g.True(ctrl.called)
	g.Eq(sessionRows(m), 0) // the dead session is gone, so it can't be reselected
	g.Has(m.status, "unreachable")
}

// A clean return from a session — the user detached (Ctrl-\) or the session's
// shell exited — quits tm back to the launching shell rather than re-showing
// the menu.
func TestModelCleanRelayReturnQuits(t *testing.T) {
	g := got.T(t)
	m := New(newStore(g, t), fakeCtrl{})

	next, cmd := m.Update(relayDoneMsg{})

	g.True(next.(Model).quit)
	g.NotNil(cmd)
	_, ok := cmd().(tea.QuitMsg)
	g.True(ok)
}

// Launched from within a session, the menu header shows that session's name so
// the user can see they are nested; otherwise it shows no such hint.
func TestModelShowsCurrentSession(t *testing.T) {
	g := got.T(t)

	in := New(newStore(g, t), sessCtrl{name: "my-work"})
	g.Has(in.viewPick(), "in session: my-work")

	out := New(newStore(g, t), fakeCtrl{})
	g.Eq(out.curSession, "")
	g.True(!strings.Contains(out.viewPick(), "in session"))
}

// The session this tm is running inside is left out of the attach list — you are
// already attached to it — while other sessions still appear.
func TestModelHidesCurrentSessionFromList(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "cur", Name: "current", Namespace: store.DefaultNamespace}))
	g.E(st.SaveSession(store.Session{ID: "oth", Name: "other", Namespace: store.DefaultNamespace}))

	m := New(st, sessCtrl{id: "cur", name: "current"})

	g.Eq(sessionRows(m), 1) // only the other session, not the one we are in

	var labels []string
	for _, it := range m.pick.all {
		if p, ok := it.payload.(menuPayload); ok && !p.isCmd {
			labels = append(labels, it.text)
		}
	}

	g.True(has(labels, "other"))
	g.True(!has(labels, "current"))
}

// Inside a session, picking another session hands the current relay over (a
// switch) instead of nesting a new one, then quits this menu.
func TestModelInSessionSwitchesInsteadOfNesting(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "target", Name: "target", Namespace: store.DefaultNamespace}))

	var switched string
	m := New(st, sessCtrl{name: "current", switchedTo: &switched})

	m = send(m, keyEnterMsg) // the cursor starts on the session -> scrollback chooser
	g.Eq(m.pickFor, pickScrollback)

	_, cmd := m.Update(keyEnterMsg) // choose "All history" -> switch cmd
	g.NotNil(cmd)

	msg, ok := cmd().(switchDoneMsg) // running it performs the switch
	g.True(ok)
	g.E(msg.err)
	g.Eq(switched, "target")

	// Delivering the result quits tm so the handed-over relay shows the target.
	final, qcmd := m.Update(msg)
	g.True(final.(Model).quit)
	g.NotNil(qcmd)
	_, isQuit := qcmd().(tea.QuitMsg)
	g.True(isQuit)
}

// Selecting [detach session] quits tm (sessions keep running in the background).
func TestModelDetachSessionQuits(t *testing.T) {
	g := got.T(t)
	m := New(newStore(g, t), fakeCtrl{})

	m = typeStr(m, "ds")
	next, cmd := m.Update(keyEnterMsg)

	g.True(next.(Model).quit)
	g.NotNil(cmd)
	_, ok := cmd().(tea.QuitMsg)
	g.True(ok)
}

// Typing "nn" then a name creates and switches to a new namespace.
func TestModelCreateNamespace(t *testing.T) {
	g := got.T(t)
	m := New(newStore(g, t), fakeCtrl{})

	m = typeStr(m, "nn")
	m = send(m, keyEnterMsg) // select [new namespace] -> input mode
	g.Eq(m.mode, modeInput)

	m = typeStr(m, "work")
	m = send(m, keyEnterMsg) // submit
	g.Eq(m.ns, "work")

	names, err := m.st.ListNamespaces()
	g.E(err)
	g.True(has(names, "work"))
}

// Typing "un" then choosing a namespace switches the active filter to it.
func TestModelUseNamespace(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.CreateNamespace("work"))

	m := New(st, fakeCtrl{})

	m = typeStr(m, "un")
	m = send(m, keyEnterMsg) // select [use namespace] -> namespace picker
	g.Eq(m.mode, modePick)
	g.Eq(m.pickFor, pickUseNamespace)

	// Choices are: "*", "default", "work" — move to "work".
	m = send(m, keyDownMsg)
	m = send(m, keyDownMsg)
	m = send(m, keyEnterMsg)
	g.Eq(m.ns, "work")
}

// Dropping a namespace reassigns its sessions to default and resets the view.
func TestModelDropNamespace(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.CreateNamespace("work"))
	g.E(st.SaveSession(store.Session{ID: "s1", Name: "s1", Namespace: "work"}))

	m := New(st, fakeCtrl{})
	m.ns = "work"

	m = typeStr(m, "dn")
	m = send(m, keyEnterMsg) // select [drop namespace] -> namespace picker
	g.Eq(m.mode, modePick)
	g.Eq(m.pickFor, pickDropNamespace)

	// Choices exclude default; "work" is the only one.
	m = send(m, keyEnterMsg)
	g.Eq(m.ns, store.DefaultNamespace)

	s1, err := st.GetSession("s1")
	g.E(err)
	g.Eq(s1.Namespace, store.DefaultNamespace)
}
