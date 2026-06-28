package tui

import (
	"os/exec"
	"slices"
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
func (fakeCtrl) DefaultSessionName(string) string                   { return "default-name" }

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
