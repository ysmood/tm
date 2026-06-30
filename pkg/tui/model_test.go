package tui

import (
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

func (fakeCtrl) CreateAndSpawn(string, string) (string, error) { return "id", nil }
func (fakeCtrl) CurrentSession() (string, string)              { return "", "" }
func (fakeCtrl) DefaultSessionName(string) string              { return "default-name" }

// sessCtrl reports a current-session id and name, simulating a tm launched from
// within a session's shell.
type sessCtrl struct {
	fakeCtrl

	id   string
	name string
}

func (c sessCtrl) CurrentSession() (string, string) { return c.id, c.name }

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

// Launched from within a session, the menu header shows that session's name so
// the user can see they are nested; otherwise it shows no such hint.
func TestModelShowsCurrentSession(t *testing.T) {
	g := got.T(t)

	in := New(newStore(g, t), sessCtrl{name: "my-work"})
	g.Has(in.viewPick(), "session:") // the header label
	g.Has(in.viewPick(), "my-work")  // the session name

	out := New(newStore(g, t), fakeCtrl{})
	g.Eq(out.curSession, "")
	g.True(!strings.Contains(out.viewPick(), "session:")) // only "namespace:" when not nested
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

// Inside a session, picking another session resolves to a switch (hand the
// current relay over) rather than an attach, and quits the menu. app.Run performs
// the handover after the menu tears down, so the menu records it but doesn't act.
func TestModelInSessionSwitchesInsteadOfNesting(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "target", Name: "target", Namespace: store.DefaultNamespace}))

	m := New(st, sessCtrl{name: "current"})

	m = send(m, keyEnterMsg) // the cursor starts on the session -> scrollback chooser
	g.Eq(m.pickFor, pickScrollback)

	next, cmd := m.Update(keyEnterMsg) // choose "All history"
	g.NotNil(cmd)

	final := next.(Model)
	g.True(final.quit)

	_, isQuit := cmd().(tea.QuitMsg)
	g.True(isQuit)

	res := final.Result()
	g.Eq(res.Action, ActionSwitch)
	g.Eq(res.ID, "target")
	g.Eq(res.Hist, proto.HistAll)
}

// Not inside a session, picking one resolves to an attach (run the relay on this
// terminal), and quits the menu for app.Run to carry out.
func TestModelNotInSessionAttaches(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "sid", Name: "solo", Namespace: store.DefaultNamespace}))

	m := New(st, fakeCtrl{})

	m = send(m, keyEnterMsg) // the cursor starts on the session -> scrollback chooser
	g.Eq(m.pickFor, pickScrollback)

	next, cmd := m.Update(keyEnterMsg) // choose "All history"
	g.NotNil(cmd)

	final := next.(Model)
	g.True(final.quit)

	res := final.Result()
	g.Eq(res.Action, ActionAttach)
	g.Eq(res.ID, "sid")
	g.Eq(res.Hist, proto.HistAll)
}

// Selecting [detach session] quits tm via ActionDetach (sessions keep running in
// the background). The dedicated action lets a menu opened mid-session with Ctrl-\
// tell "detach to my shell" apart from a plain esc, which resumes the session.
func TestModelDetachSessionQuits(t *testing.T) {
	g := got.T(t)
	m := New(newStore(g, t), fakeCtrl{})

	m = typeStr(m, "ds")
	next, cmd := m.Update(keyEnterMsg)

	final := next.(Model)
	g.True(final.quit)
	g.Eq(final.Result().Action, ActionDetach)
	g.NotNil(cmd)
	_, ok := cmd().(tea.QuitMsg)
	g.True(ok)
}

// Selecting [help] opens the detailed help screen; any key dismisses it back to
// the main menu. The key hints live here rather than cluttering every frame.
func TestModelHelpScreen(t *testing.T) {
	g := got.T(t)
	m := New(newStore(g, t), fakeCtrl{})

	m = typeStr(m, "help") // only [help] fuzzy-matches "help"
	m = send(m, keyEnterMsg)
	g.Eq(m.mode, modeHelp)

	v := m.View().Content
	g.Has(v, "tm — help")
	g.Has(v, "[new session]")
	g.Has(v, "press any key to go back")

	m = send(m, keyEnterMsg) // any key returns to the menu
	g.Eq(m.mode, modePick)
	g.Eq(m.pickFor, pickMenu)
}

// [use namespace] also creates: typing a name no namespace has yet surfaces a
// create row that makes it and switches to it.
func TestModelCreateNamespace(t *testing.T) {
	g := got.T(t)
	m := New(newStore(g, t), fakeCtrl{})

	m = typeStr(m, "un")
	m = send(m, keyEnterMsg) // select [use namespace] -> namespace picker
	g.Eq(m.pickFor, pickUseNamespace)

	// "work" matches no existing namespace, so the only row is "[new namespace]
	// work"; select it to create and switch.
	m = typeStr(m, "work")
	g.Has(m.View().Content, "[new namespace] work")

	m = send(m, keyEnterMsg)
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
