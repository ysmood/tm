// Package tui implements the interactive Bubble Tea menu: a fuzzy-filterable
// list of commands and sessions. Selecting a session (or creating one) hands
// the terminal to the relay via tea.ExecProcess. A clean return — the user
// detached or the session's shell exited — quits tm back to the launching
// shell; only a failed attach drops back into the menu (reaping dead sessions).
//
// Every menu — the main list, the scrollback chooser, the namespace chooser —
// is the same type-to-filter picker (see picker.go), so they share keys and
// behavior. All text entry — the picker's filter and the free-text prompts
// (naming a session or namespace, a custom line count) — is a single-line
// textarea built by newInput.
package tui

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
)

// Controller performs the side effects the menu needs but can't do itself
// (spawning processes). It is implemented by package app.
type Controller interface {
	// AttachCmd builds the relay command for attaching to a session.
	AttachCmd(id string, hist proto.HistMode, lines uint32) *exec.Cmd
	// CreateAndSpawn creates a session in ns and starts its daemon, returning its id.
	CreateAndSpawn(ns, name string) (string, error)
	// DefaultSessionName proposes a unique default name for a new session in ns.
	DefaultSessionName(ns string) string
	// Reap drops sessions whose daemon is no longer running and reports how many
	// it removed. The menu calls it after a failed attach so a dead session stops
	// reappearing in the list (otherwise selecting it again bounces back here forever).
	Reap() int
}

type cmdID int

const (
	cmdNewSession cmdID = iota
	cmdDetachSession
	cmdNewNamespace
	cmdUseNamespace
	cmdDropNamespace
)

type paletteCmd struct {
	id      cmdID
	label   string
	aliases []string
}

// palette holds the fixed commands. Each lists alias tokens (mnemonic first) so
// typing "ns"/"nn"/"un"/"dn"/"ds" — or a word like "detach"/"drop" — selects
// the intended command deterministically.
var palette = []paletteCmd{
	{cmdNewSession, "[new session]", []string{"ns", "new session", "new"}},
	{cmdDetachSession, "[detach session]", []string{"ds", "detach session", "detach"}},
	{cmdNewNamespace, "[new namespace]", []string{"nn", "new namespace", "namespace"}},
	{cmdUseNamespace, "[use namespace]", []string{"un", "use namespace", "use"}},
	{cmdDropNamespace, "[drop namespace]", []string{"dn", "drop namespace", "drop"}},
}

// menuPayload is the data attached to a main-menu row: either a command or a
// session.
type menuPayload struct {
	isCmd bool
	cmdID cmdID
	sess  store.Session
}

// scrollbackPayload is the data attached to a scrollback-chooser row.
type scrollbackPayload struct {
	hist   proto.HistMode
	lines  uint32
	custom bool // prompt for a line count instead of attaching directly
}

type mode int

const (
	modePick  mode = iota // a type-to-filter menu (main, scrollback, namespace)
	modeInput             // a free-text prompt
)

// pickPurpose says what the active picker selects, so Enter dispatches correctly.
type pickPurpose int

const (
	pickMenu pickPurpose = iota
	pickScrollback
	pickUseNamespace
	pickDropNamespace
)

type inputPurpose int

const (
	inputNewSession inputPurpose = iota
	inputNewNamespace
	inputCustomLines
)

// Model is the Bubble Tea menu model.
type Model struct {
	st   *store.Store
	ctrl Controller
	ns   string

	mode mode

	pick    picker
	pickFor pickPurpose

	input        textarea.Model
	inputPurpose inputPurpose

	pendingID string // session awaiting a scrollback choice

	status        string
	quit          bool
	width, height int
}

// New builds the menu model over a store and controller.
func New(st *store.Store, ctrl Controller) Model {
	m := Model{st: st, ctrl: ctrl, ns: store.DefaultNamespace, input: newInput("> "), pick: newPicker()}
	m.showMenu()

	return m
}

// newInput builds the single-line textarea used for every text field: the
// picker's filter and the free-text prompts. textarea is multi-line by nature,
// so it is pinned to one row with newlines disabled and the cursor-line
// highlight, line numbers and blink stripped for a plain "prompt text" look.
func newInput(prompt string) textarea.Model {
	ta := textarea.New()
	ta.Prompt = prompt
	ta.ShowLineNumbers = false
	ta.MaxHeight = 1
	ta.SetHeight(1)
	ta.KeyMap.InsertNewline.SetEnabled(false)

	azure := lipgloss.Color("#00e6cb")

	s := ta.Styles()
	s.Cursor.Blink = false
	s.Focused.CursorLine = lipgloss.NewStyle()
	s.Blurred.CursorLine = lipgloss.NewStyle()
	s.Focused.Prompt = s.Focused.Prompt.Foreground(azure)
	s.Blurred.Prompt = s.Blurred.Prompt.Foreground(azure)
	ta.SetStyles(s)

	return ta
}

// Init satisfies tea.Model.
func (m Model) Init() tea.Cmd { return nil }

// menuItems builds the main menu: the sessions in the active namespace first
// (attaching is the common action, so they sit at the top and the cursor starts
// on one), followed by the fixed commands. Ranking is independent of this order,
// so typing a mnemonic still surfaces its command (see rankItems).
func (m *Model) menuItems() []pickerItem {
	items := make([]pickerItem, 0, len(palette)+8)

	sessions, _ := m.st.ListByNamespace(m.ns)
	for _, s := range sessions {
		label := s.Name
		if m.ns == store.AllNamespaces {
			label = s.Name + "  (" + s.Namespace + ")"
		}

		items = append(items, pickerItem{label: label, text: s.Name, payload: menuPayload{sess: s}})
	}

	for _, c := range palette {
		items = append(items, pickerItem{
			label:   c.label,
			aliases: c.aliases,
			payload: menuPayload{isCmd: true, cmdID: c.id},
		})
	}

	return items
}

// showMenu returns to the main command/session list.
func (m *Model) showMenu() {
	m.mode = modePick
	m.pickFor = pickMenu
	m.pick.setItems(m.menuItems())
}

func (m *Model) showScrollback() {
	m.mode = modePick
	m.pickFor = pickScrollback
	m.pick.setItems([]pickerItem{
		{label: "All history", payload: scrollbackPayload{hist: proto.HistAll}},
		{label: "One page", payload: scrollbackPayload{hist: proto.HistPage}},
		{label: "Last 100 lines", payload: scrollbackPayload{hist: proto.HistLines, lines: 100}},
		{label: "Last 1000 lines", payload: scrollbackPayload{hist: proto.HistLines, lines: 1000}},
		{label: "Custom number of lines…", payload: scrollbackPayload{custom: true}},
	})
}

func (m *Model) showNamespaces(p pickPurpose) {
	m.mode = modePick
	m.pickFor = p

	names, _ := m.st.ListNamespaces()

	var items []pickerItem
	if p == pickUseNamespace {
		items = append(items, pickerItem{label: "* (all sessions)", text: "* all sessions", payload: store.AllNamespaces})
	}

	for _, n := range names {
		if p == pickDropNamespace && n == store.DefaultNamespace {
			continue
		}

		items = append(items, pickerItem{label: n, payload: n})
	}

	m.pick.setItems(items)
}

// relayDoneMsg is delivered when the relay subprocess returns.
type relayDoneMsg struct{ err error }

// spawnedMsg is delivered when a new session's daemon has started.
type spawnedMsg struct {
	id  string
	err error
}

// Update satisfies tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.pick.setSize(msg.Width, m.listHeight())
		m.input.SetWidth(msg.Width)

		return m, nil
	case relayDoneMsg:
		if msg.err != nil {
			// A relay error means the daemon was unreachable. Reap any sessions
			// whose daemon has died so the dead one drops out of the menu instead
			// of luring the user into selecting it and bouncing back here again.
			if n := m.ctrl.Reap(); n > 0 {
				m.status = "removed " + reapNoun(n)
			} else {
				m.status = "session ended: " + msg.err.Error()
			}

			m.showMenu()

			return m, nil
		}

		// A clean return from a session — the user detached (Ctrl-\) or the
		// session's shell exited — means we're done driving a session: leave tm
		// and drop back to the launching shell, with every session still running.
		// Run tm again to pick up another one.
		m.quit = true

		return m, tea.Quit
	case spawnedMsg:
		if msg.err != nil {
			m.status = "failed to start session: " + msg.err.Error()
			m.showMenu()

			return m, nil
		}

		return m, m.attach(msg.id, proto.HistNone, 0)
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.quit = true

			return m, tea.Quit
		}

		switch m.mode {
		case modePick:
			return m.updatePick(msg)
		case modeInput:
			return m.updateInput(msg)
		}
	}

	return m, nil
}

// reapNoun renders a count of removed-because-unreachable sessions for the
// status line, e.g. "1 unreachable session" or "3 unreachable sessions".
func reapNoun(n int) string {
	if n == 1 {
		return "1 unreachable session"
	}

	return strconv.Itoa(n) + " unreachable sessions"
}

func (m Model) attach(id string, hist proto.HistMode, lines uint32) tea.Cmd {
	cmd := m.ctrl.AttachCmd(id, hist, lines)

	return tea.ExecProcess(cmd, func(err error) tea.Msg { return relayDoneMsg{err} })
}

// Key names compared against tea.KeyMsg.String().
const (
	keyEsc   = "esc"
	keyEnter = "enter"
)

func (m Model) updatePick(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	action, cmd := m.pick.update(msg)
	switch action {
	case pickCanceled:
		// On the main menu esc leaves tm (sessions keep running); in a
		// sub-menu it backs out to the main menu.
		if m.pickFor == pickMenu {
			m.quit = true

			return m, tea.Quit
		}

		m.showMenu()

		return m, nil
	case pickSelected:
		it, ok := m.pick.selected()
		if !ok {
			return m, nil
		}

		return m.selectPicked(it)
	case pickNothing:
	}

	return m, cmd
}

func (m Model) selectPicked(it pickerItem) (tea.Model, tea.Cmd) {
	// Each picker holds one payload type, set right where its items are built,
	// so these assertions hold by construction; the ok-checks just keep the
	// dispatch total.
	switch m.pickFor {
	case pickMenu:
		if p, ok := it.payload.(menuPayload); ok {
			return m.selectMenu(p)
		}
	case pickScrollback:
		if p, ok := it.payload.(scrollbackPayload); ok {
			return m.selectScrollback(p)
		}
	case pickUseNamespace:
		if ns, ok := it.payload.(string); ok {
			m.ns = ns
			m.status = "namespace: " + ns
			m.showMenu()
		}
	case pickDropNamespace:
		if ns, ok := it.payload.(string); ok {
			return m.dropNamespace(ns)
		}
	}

	return m, nil
}

func (m Model) selectMenu(p menuPayload) (tea.Model, tea.Cmd) {
	if !p.isCmd {
		m.pendingID = p.sess.ID
		m.showScrollback()

		return m, nil
	}

	switch p.cmdID {
	case cmdNewSession:
		m.enterInput(inputNewSession, "New session name:", m.ctrl.DefaultSessionName(m.targetNamespace()))
	case cmdDetachSession:
		// In tm's model the menu is the detached state: sessions are independent
		// daemons that keep running. "Detach" therefore means leave tm entirely
		// and return to the launching shell, with every session still alive.
		m.quit = true

		return m, tea.Quit
	case cmdNewNamespace:
		m.enterInput(inputNewNamespace, "New namespace name:", "")
	case cmdUseNamespace:
		m.showNamespaces(pickUseNamespace)
	case cmdDropNamespace:
		m.showNamespaces(pickDropNamespace)
	}

	return m, nil
}

func (m Model) selectScrollback(p scrollbackPayload) (tea.Model, tea.Cmd) {
	if p.custom {
		m.enterInput(inputCustomLines, "Number of lines:", "")

		return m, nil
	}

	id := m.pendingID
	m.showMenu()

	return m, m.attach(id, p.hist, p.lines)
}

func (m Model) dropNamespace(ns string) (tea.Model, tea.Cmd) {
	if err := m.st.DeleteNamespace(ns); err != nil {
		m.status = err.Error()
	} else {
		if m.ns == ns {
			m.ns = store.DefaultNamespace
		}

		m.status = "dropped namespace: " + ns
	}

	m.showMenu()

	return m, nil
}

// targetNamespace is the namespace new sessions land in; "*" is a view, so new
// sessions there default to the default namespace.
func (m Model) targetNamespace() string {
	if m.ns == store.AllNamespaces {
		return store.DefaultNamespace
	}

	return m.ns
}

func (m *Model) enterInput(p inputPurpose, prompt, value string) {
	m.mode = modeInput
	m.inputPurpose = p
	m.input.Prompt = prompt + " "
	m.input.SetValue(value)
	m.input.CursorEnd()
	_ = m.input.Focus()
}

func (m Model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEsc:
		m.showMenu()

		return m, nil
	case keyEnter:
		return m.submitInput(strings.TrimSpace(m.input.Value()))
	}

	var cmd tea.Cmd

	m.input, cmd = m.input.Update(msg)

	return m, cmd
}

func (m Model) submitInput(val string) (tea.Model, tea.Cmd) {
	switch m.inputPurpose {
	case inputNewSession:
		if val == "" {
			m.status = "name cannot be empty"

			return m, nil
		}

		ns := m.targetNamespace()
		m.showMenu()
		m.status = "starting session…"

		return m, func() tea.Msg {
			id, err := m.ctrl.CreateAndSpawn(ns, val)

			return spawnedMsg{id: id, err: err}
		}
	case inputNewNamespace:
		if val == "" {
			m.status = "name cannot be empty"

			return m, nil
		}

		if err := m.st.CreateNamespace(val); err != nil {
			m.status = err.Error()
		} else {
			m.ns = val
			m.status = "namespace: " + val
		}

		m.showMenu()

		return m, nil
	case inputCustomLines:
		n, err := strconv.Atoi(val)
		if err != nil || n <= 0 {
			m.status = "enter a positive number"

			return m, nil
		}

		id := m.pendingID
		m.showMenu()

		return m, m.attach(id, proto.HistLines, uint32(n))
	}

	return m, nil
}

// View satisfies tea.Model. The menu runs in the alternate screen so the
// relayed shell (run via tea.ExecProcess) owns the main screen's scrollback.
func (m Model) View() tea.View {
	if m.quit {
		return tea.View{}
	}

	content := m.viewPick()
	if m.mode == modeInput {
		content = m.viewInput()
	}

	return tea.View{Content: content, AltScreen: true}
}

// maxRows is the picker height used until the first WindowSizeMsg (and the
// minimum thereafter).
const maxRows = 15

// listHeight is the height handed to the picker's list, leaving room for the
// title, query line and footer.
func (m Model) listHeight() int {
	if h := m.height - 7; h >= 3 {
		return h
	}

	return maxRows
}

func (m Model) viewPick() string {
	th := styles()

	var b strings.Builder

	b.WriteString(th.title.Render("tm") + "  " + th.dim.Render("namespace: "+m.ns) + "\n\n")
	b.WriteString(m.pick.view())
	b.WriteString("\n" + m.footer())

	return b.String()
}

func (m Model) footer() string {
	th := styles()

	keys := `↑/↓ move · type to filter · enter select · esc back`
	if m.pickFor == pickMenu {
		keys = "↑/↓ move · type to filter · enter select · " +
			`esc quit (sessions keep running) · Ctrl-\ in a session detaches to your shell`
	}

	help := th.dim.Render(keys)
	if m.status != "" {
		return th.status.Render(m.status) + "\n" + help
	}

	return help
}

func (m Model) viewInput() string {
	th := styles()

	return th.title.Render("tm") + "\n\n" + m.input.View() + "\n\n" +
		th.dim.Render("enter confirm · esc cancel")
}

type theme struct {
	title  lipgloss.Style
	dim    lipgloss.Style
	sel    lipgloss.Style
	status lipgloss.Style
}

// styles builds the lipgloss styles once, on first render.
var styles = sync.OnceValue(func() theme {
	return theme{
		title:  lipgloss.NewStyle().Bold(true),
		dim:    lipgloss.NewStyle().Faint(true),
		sel:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		status: lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
	}
})
