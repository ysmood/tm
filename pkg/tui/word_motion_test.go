package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/ysmood/got"
)

// The word-motion keys (option/alt+arrow and their ESC b/f, ctrl/meta+arrow
// forms) move through punctuation-separated values word by word, as at a shell
// prompt. The textarea's built-in motion knows only whitespace boundaries,
// which on a value like "default-name" jumped to the line's start or end
// instead of word by word.
func TestInputWordMotion(t *testing.T) {
	g := got.T(t)

	m := New(newStore(g, t), fakeCtrl{})
	m = send(m, ctrlKey('t')) // new-session prompt, prefilled "default-name"
	g.Eq(m.input.Value(), "default-name")
	g.Eq(m.input.Column(), 12) // cursor starts at the end

	col := func(msg tea.KeyPressMsg, want int) {
		g.Helper()

		m = send(m, msg)
		g.Eq(m.input.Column(), want)
	}

	altLeft := tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt}
	altRight := tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModAlt}

	col(altLeft, 8)   // to the start of "name"
	col(altLeft, 0)   // over the dash to the start of "default"
	col(altRight, 7)  // to the end of "default"
	col(altRight, 12) // over the dash to the end of "name"

	// The ESC b / ESC f forms (Terminal.app, VS Code) and the ctrl/meta+arrow
	// variants other terminals send move the same way.
	col(tea.KeyPressMsg{Code: 'b', Mod: tea.ModAlt}, 8)
	col(tea.KeyPressMsg{Code: 'f', Mod: tea.ModAlt}, 12)
	col(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModCtrl}, 8)
	col(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModMeta}, 12)

	g.Eq(m.input.Value(), "default-name") // motion only moves the cursor
}

// The picker's filter input gets the same punctuation-aware word motion, and
// moving never disturbs the typed query.
func TestPickerWordMotion(t *testing.T) {
	g := got.T(t)

	m := New(newStore(g, t), fakeCtrl{})
	m = typeStr(m, "kill-ses")
	g.Eq(m.pick.input.Column(), 8)

	col := func(msg tea.KeyPressMsg, want int) {
		g.Helper()

		m = send(m, msg)
		g.Eq(m.pick.input.Column(), want)
	}

	col(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt}, 5)  // to the start of "ses"
	col(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt}, 0)  // to the start of "kill"
	col(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModAlt}, 4) // to the end of "kill"

	g.Eq(m.pick.input.Value(), "kill-ses") // the query is untouched
}

// Option+backspace deletes one word back to the previous punctuation boundary,
// and alt+d / fn+option+delete one word forward — not the whole value, as the
// textarea's whitespace-only word deletes did on punctuation-separated values.
func TestInputWordDelete(t *testing.T) {
	g := got.T(t)

	m := New(newStore(g, t), fakeCtrl{})
	m = send(m, ctrlKey('t')) // new-session prompt, prefilled "default-name"

	// Backward: "name" first, then the dash and "default".
	m = send(m, tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModAlt})
	g.Eq(m.input.Value(), "default-")
	g.Eq(m.input.Column(), 8)

	m = send(m, tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModAlt})
	g.Eq(m.input.Value(), "")

	// Forward: alt+d from the start deletes "default", alt+delete the "-name"
	// left after it.
	m = typeStr(m, "default-name")
	m = send(m, ctrlKey('a')) // line start
	m = send(m, tea.KeyPressMsg{Code: 'd', Mod: tea.ModAlt})
	g.Eq(m.input.Value(), "-name")
	g.Eq(m.input.Column(), 0)

	m = send(m, tea.KeyPressMsg{Code: tea.KeyDelete, Mod: tea.ModAlt})
	g.Eq(m.input.Value(), "")
}

// A word delete in the picker's filter refilters the list, just as typing does.
func TestPickerWordDelete(t *testing.T) {
	g := got.T(t)

	m := New(newStore(g, t), fakeCtrl{})
	m = typeStr(m, "zzz-qqq") // matches nothing
	g.Len(m.pick.list.Items(), 0)

	m = send(m, tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModAlt})
	g.Eq(m.pick.input.Value(), "zzz-") // only "qqq" deleted
	g.Len(m.pick.list.Items(), 0)

	m = send(m, tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModAlt})
	g.Eq(m.pick.input.Value(), "")
	g.Gt(len(m.pick.list.Items()), 0) // refiltered: the full menu is back
}
