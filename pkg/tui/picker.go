package tui

import (
	"io"
	"strings"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// pickerItem is one selectable row. text overrides the fuzzy-match target (it
// defaults to label). isCmd marks a bracketed command action (rather than a
// session or namespace name) so the delegate colors it apart. hint is an
// optional shortcut label (e.g. "Ctrl-T") the delegate renders dimmed at the
// row's right edge. payload carries whatever the caller needs when the row is
// chosen.
type pickerItem struct {
	label string
	text  string
	isCmd bool
	hint  string
	// hintFn, when set, recomputes hint from the current filter query on every
	// refilter, so a row's shortcut hint can react to what is typed (see the
	// [exit] row in menuItems).
	hintFn  func(query string) string
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

// pickerDelegate renders one row: the cursor glyph plus the label when selected,
// an indented label otherwise — bracketed command rows in the command color and
// names (sessions, namespaces) in the plain name color, so the two read apart by
// color as well as by their brackets.
type pickerDelegate struct{}

func (pickerDelegate) Height() int                         { return 1 }
func (pickerDelegate) Spacing() int                        { return 0 }
func (pickerDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (pickerDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	a, ok := item.(listAdapter)
	if !ok {
		return
	}

	th := styles()
	selected := index == m.Index()

	// The selected row gets the cursor glyph and the selection color; other rows
	// are indented two cells (matching the glyph plus its trailing space) so labels
	// stay aligned, with command rows in the command color.
	var row string
	if selected {
		row = th.sel.Render(cursorGlyph + " " + a.label)
	} else {
		style := th.item
		if a.isCmd {
			style = th.cmd
		}

		row = "  " + style.Render(a.label)
	}

	// A shortcut hint sits flush against the row's right edge. On the focused row
	// it takes the selection color too, so the whole row highlights together;
	// otherwise it stays an even dimmer grey (see theme.key), reading as secondary
	// to the label. The list width matches the box's content width, so padding the
	// row to it lands the hint on the right border.
	if a.hint != "" {
		hintStyle := th.key
		if selected {
			hintStyle = th.sel
		}

		hint := hintStyle.Render(a.hint)
		gap := max(1, m.Width()-lipgloss.Width(row)-lipgloss.Width(hint))
		row += strings.Repeat(" ", gap) + hint
	}

	_, _ = io.WriteString(w, row)
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
	width int
	// extra, when set, contributes query-dependent rows appended after the ranked
	// matches and recomputed on every keystroke, so they track the typed text. It
	// backs the [use namespace] picker's "create <query>" row.
	extra func(query string) []pickerItem
}

func newPicker() picker {
	l := list.New(nil, pickerDelegate{}, 80, maxRows)
	l.SetFilteringEnabled(false)
	l.SetShowTitle(false)
	l.SetShowFilter(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	// No pagination chrome: the inline list is sized to its visible rows (see
	// applySize), so the page dots would only steal a row and split the items.
	// Navigation still pages internally when the list overflows maxRows.
	l.SetShowPagination(false)
	l.SetStatusBarItemName("match", "matches") // renders "No matches." when empty
	l.DisableQuitKeybindings()

	in := newInput("> ")
	_ = in.Focus()

	return picker{list: l, input: in, width: 80}
}

// setItems replaces the full item set, clears the query and drops any extra-row
// hook from a previous menu.
func (p *picker) setItems(items []pickerItem) {
	p.all = items
	p.extra = nil
	p.input.SetValue("")
	p.refilter()
}

// setExtra installs a hook that contributes query-dependent rows after the
// ranked matches (see picker.extra). Call it after setItems.
func (p *picker) setExtra(fn func(query string) []pickerItem) {
	p.extra = fn
	p.refilter()
}

// setWidth sets the picker's width and re-applies its size. The height is not a
// parameter: the menu renders inline (not in the alternate screen), so the list
// is kept just tall enough for its rows — see applySize — instead of filling the
// terminal, which would scroll the user's existing screen out of view.
func (p *picker) setWidth(w int) {
	p.width = w
	p.input.SetWidth(w)
	p.applySize()
}

// applySize sizes the list to the visible row count, capped at maxRows, so the
// inline picker stays compact and the screen above it is preserved. A minimum of
// one row keeps the "No matches." empty state visible.
func (p *picker) applySize() {
	p.list.SetSize(p.width, max(1, min(len(p.list.Items()), maxRows)))
}

// refilter recomputes the visible rows from the current query.
func (p *picker) refilter() {
	query := p.input.Value()
	order := rankItems(p.all, query)
	items := make([]list.Item, 0, len(order)+1)

	for _, idx := range order {
		it := p.all[idx]
		if it.hintFn != nil {
			it.hint = it.hintFn(query)
		}

		items = append(items, listAdapter{it})
	}

	if p.extra != nil {
		for _, it := range p.extra(query) {
			items = append(items, listAdapter{it})
		}
	}

	p.list.SetItems(items)
	p.list.Select(0)
	p.applySize()
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
	case keyCtrlD:
		// Ctrl-D is the terminal EOF (VEOF): on an empty filter it ends the menu
		// just like esc; with a query typed it does nothing, so it never inserts a
		// stray character.
		if p.input.Value() == "" {
			return pickCanceled, nil
		}

		return pickNothing, nil
	}

	before := p.input.Value()

	var cmd tea.Cmd

	// Word motion and word deletes are handled by wordKey, punctuation-aware,
	// instead of by the textarea's whitespace-only versions; everything else is
	// text editing forwarded to the query textarea. Either way, a changed value
	// (a word delete, typed text) narrows the list.
	if !wordKey(&p.input, msg) {
		p.input, cmd = p.input.Update(msg)
	}

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

// inputView renders the query line; listView renders the list below it. They are
// exposed separately so the menu can frame each in its own bordered section (the
// query box and the list box — see Model.box).
func (p picker) inputView() string { return p.input.View() }
func (p picker) listView() string  { return p.list.View() }
