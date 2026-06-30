package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/store"
)

// sized builds a model with a window size so the bubbles list paginates and
// renders against a real viewport (the unit tests otherwise never call View).
func sized(g got.G, t *testing.T) Model {
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "s1", Name: "webserver", Namespace: store.DefaultNamespace}))
	g.E(st.SaveSession(store.Session{ID: "s2", Name: "api", Namespace: store.DefaultNamespace}))

	m := New(st, fakeCtrl{})

	return send(m, tea.WindowSizeMsg{Width: 80, Height: 24})
}

// The main menu renders commands and sessions, and narrows as the user types.
func TestPickerTypeToFilter(t *testing.T) {
	g := got.T(t)
	m := sized(g, t)

	v := m.View().Content
	g.Has(v, "[new session]")
	g.Has(v, "webserver")
	// Sessions are listed ahead of the commands.
	g.True(strings.Index(v, "webserver") < strings.Index(v, "[new session]"))

	// Typing filters live: the matching session stays, unrelated commands drop.
	typed := typeStr(m, "web")
	g.Eq(typed.pick.input.Value(), "web") // query captured by the textarea
	filtered := typed.View().Content
	g.Has(filtered, "webserver")
	g.True(!strings.Contains(filtered, "[use namespace]"))

	// A query that matches nothing shows the empty state.
	g.Has(typeStr(m, "zzzz").View().Content, "No matches")
}

// The scrollback chooser is the same picker, so it filters by typing too.
func TestPickerScrollbackFilters(t *testing.T) {
	g := got.T(t)
	m := sized(g, t)

	m = typeStr(m, "api")
	m = send(m, keyEnterMsg) // select the session -> scrollback chooser
	g.Eq(m.pickFor, pickScrollback)
	g.Has(m.View().Content, "All history")

	m = typeStr(m, "1000")
	v := m.View().Content
	g.Has(v, "Last 1000 lines")
	g.True(!strings.Contains(v, "All history"))
}

// ctrl+n / ctrl+p move the cursor like the arrow and ctrl+j / ctrl+k keys.
func TestPickerEmacsNavKeys(t *testing.T) {
	g := got.T(t)
	m := sized(g, t)

	ctrl := func(r rune) tea.KeyPressMsg {
		return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
	}

	// From the top, ctrl+n moves down one row; ctrl+p moves back up.
	g.Eq(m.pick.list.Index(), 0)
	m = send(m, ctrl('n'))
	g.Eq(m.pick.list.Index(), 1)
	m = send(m, ctrl('p'))
	g.Eq(m.pick.list.Index(), 0)
}
