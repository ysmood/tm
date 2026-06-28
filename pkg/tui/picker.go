package tui

import (
	"io"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// pickerItem is one selectable row. aliases, when non-empty, enable mnemonic
// matching and rank the item in a band above plain fuzzy matches (used for the
// fixed commands). text overrides the fuzzy-match target (it defaults to
// label). payload carries whatever the caller needs when the row is chosen.
type pickerItem struct {
	label   string
	aliases []string
	text    string
	payload any
}

func (it pickerItem) matchText() string {
	if it.text != "" {
		return it.text
	}

	return it.label
}

// listAdapter adapts pickerItem to bubbles' list.Item. FilterValue is only here
// to satisfy the interface; the picker filters itself (see refilter) instead of
// using the list's built-in filtering.
type listAdapter struct{ pickerItem }

func (a listAdapter) FilterValue() string { return a.matchText() }

// cursorGlyph marks the highlighted row. It and the trailing space are one cell
// each, matching the two-space indent of unselected rows so labels stay aligned.
const cursorGlyph = "●"

// pickerDelegate renders one row: a purple circle plus the label when selected,
// a plain indented label otherwise.
type pickerDelegate struct{}

func (pickerDelegate) Height() int                         { return 1 }
func (pickerDelegate) Spacing() int                        { return 0 }
func (pickerDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (pickerDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	a, ok := item.(listAdapter)
	if !ok {
		return
	}

	if index == m.Index() {
		_, _ = io.WriteString(w, styles().sel.Render(cursorGlyph+" "+a.label))
	} else {
		_, _ = io.WriteString(w, "  "+a.label)
	}
}

// pickerAction is what a key press resolved to.
type pickerAction int

const (
	pickNothing pickerAction = iota
	pickSelected
	pickCanceled
)

// picker is a type-to-filter menu: bubbles' list handles rendering, the cursor
// and pagination, while a textarea drives the query line and the ranking is
// driven here so the list narrows as you type (the list's own filtering is
// disabled). Every menu in the TUI uses it, so they share keys and behavior.
type picker struct {
	list  list.Model
	input textarea.Model
	all   []pickerItem
}

func newPicker() picker {
	l := list.New(nil, pickerDelegate{}, 80, maxRows)
	l.SetFilteringEnabled(false)
	l.SetShowTitle(false)
	l.SetShowFilter(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetStatusBarItemName("match", "matches") // renders "No matches." when empty
	l.DisableQuitKeybindings()

	in := newInput("> ")
	_ = in.Focus()

	return picker{list: l, input: in}
}

// setItems replaces the full item set and clears the query.
func (p *picker) setItems(items []pickerItem) {
	p.all = items
	p.input.SetValue("")
	p.refilter()
}

func (p *picker) setSize(w, h int) {
	p.list.SetSize(w, h)
	p.input.SetWidth(w)
}

// refilter recomputes the visible rows from the current query.
func (p *picker) refilter() {
	order := rankItems(p.all, p.input.Value())
	items := make([]list.Item, len(order))

	for i, idx := range order {
		items[i] = listAdapter{p.all[idx]}
	}

	p.list.SetItems(items)
	p.list.Select(0)
}

// update handles one key press and reports whether it selected or canceled.
// Navigation and select/cancel keys are handled here; everything else is text
// editing forwarded to the query textarea, so letters that double as list or
// textarea shortcuts (j/k/g) filter rather than act.
func (p *picker) update(msg tea.KeyPressMsg) (pickerAction, tea.Cmd) {
	switch msg.String() {
	case "up", "ctrl+k", "ctrl+p":
		p.list.CursorUp()

		return pickNothing, nil
	case "down", "ctrl+j", "ctrl+n":
		p.list.CursorDown()

		return pickNothing, nil
	case keyEnter:
		return pickSelected, nil
	case keyEsc:
		return pickCanceled, nil
	}

	before := p.input.Value()

	var cmd tea.Cmd

	p.input, cmd = p.input.Update(msg)
	if p.input.Value() != before {
		p.refilter()
	}

	return pickNothing, cmd
}

// selected returns the highlighted item, if any.
func (p picker) selected() (pickerItem, bool) {
	a, ok := p.list.SelectedItem().(listAdapter)
	if !ok {
		return pickerItem{}, false
	}

	return a.pickerItem, true
}

// view renders the query line above the list.
func (p picker) view() string {
	return p.input.View() + "\n\n" + p.list.View()
}
