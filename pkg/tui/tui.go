// Package tui implements the interactive Bubble Tea menu: a fuzzy-filterable
// list of commands and sessions. Selecting a session (or creating one) records
// what to do (see Result) and quits; app.Run carries it out — attaching the
// relay or, inside a session, switching it — once the menu has torn down, so the
// inline picker is erased from the screen first. The menu itself runs no relay.
//
// Every menu — the main list, the scrollback chooser, the namespace chooser —
// is the same type-to-filter picker (see picker.go), so they share keys and
// behavior. All text entry — the picker's filter and the free-text prompts
// (naming a session or namespace, a custom line count) — is a single-line
// textarea built by newInput.
package tui

import (
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
//
// The menu does not run the relay or perform the switch itself: it records what
// the user chose (see Result) and quits, and app.Run carries it out once the menu
// has torn down. So attaching, switching and reaping live in app.Run, not here.
type Controller interface {
	// CreateAndSpawn creates a session in ns and starts its daemon, returning its id.
	CreateAndSpawn(ns, name string) (string, error)
	// CurrentSession returns the id and name of the session this tm is running
	// inside (when launched from a session's shell), or "", "" if not in a session.
	// The id lets the menu hide the current session from the attach list (you are
	// already in it); the name is shown in the header as a nesting hint, and its
	// presence decides whether a pick attaches or switches (see Result).
	CurrentSession() (id, name string)
	// DefaultSessionName proposes a unique default name for a new session in ns.
	DefaultSessionName(ns string) string
}

// Action is what the user resolved the menu to when it exited; app.Run carries
// it out after the menu has torn down (so the picker is erased from the screen
// first).
type Action int

const (
	// ActionNone means leave tm — the user cancelled, quit, or detached.
	ActionNone Action = iota
	// ActionAttach means run the relay for Result.ID on this terminal (the menu
	// was not launched from within a session).
	ActionAttach
	// ActionSwitch means hand the current session's relay to Result.ID (the menu
	// was launched from within a session, so picking another moves this terminal
	// there instead of nesting a new relay).
	ActionSwitch
)

// Result is the menu's outcome: what to do and the chosen session's replay.
type Result struct {
	Action Action
	ID     string
	Hist   proto.HistMode
	Lines  uint32
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
	id    cmdID
	label string
}

// palette holds the fixed commands. The bracketed labels are fuzzy-matched like
// everything else (see rankItems), so typing the letters of a command in order
// surfaces it — "ds" finds [detach session], "[n" the two [new …] commands.
var palette = []paletteCmd{
	{cmdNewSession, "[new session]"},
	{cmdDetachSession, "[detach session]"},
	{cmdNewNamespace, "[new namespace]"},
	{cmdUseNamespace, "[use namespace]"},
	{cmdDropNamespace, "[drop namespace]"},
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

	// curSession is the name of the session this tm is running inside, or "" when
	// not launched from within a session. Shown in the header as a nesting hint.
	curSession string
	// curSessionID is the id of that session, used to hide it from the attach list
	// (re-attaching to the session you are already in is a no-op).
	curSessionID string

	mode mode

	pick    picker
	pickFor pickPurpose

	input        textarea.Model
	inputPurpose inputPurpose

	pendingID string // session awaiting a scrollback choice

	// result is what the menu resolved to; app.Run reads it via Result after the
	// program exits and carries it out (attach, switch, or nothing).
	result Result

	status string
	quit   bool
}

// New builds the menu model over a store and controller.
func New(st *store.Store, ctrl Controller) Model {
	m := Model{st: st, ctrl: ctrl, ns: store.DefaultNamespace, input: newInput("> "), pick: newPicker()}
	m.curSessionID, m.curSession = ctrl.CurrentSession()
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
// so fuzzy-typing a command's letters still surfaces it (see rankItems). The session
// this tm is running inside is left out — you are already attached to it, so
// re-selecting it would do nothing useful.
func (m *Model) menuItems() []pickerItem {
	items := make([]pickerItem, 0, len(palette)+8)

	sessions, _ := m.st.ListByNamespace(m.ns)
	for _, s := range sessions {
		if s.ID == m.curSessionID {
			continue
		}

		label := s.Name
		if m.ns == store.AllNamespaces {
			label = s.Name + "  (" + s.Namespace + ")"
		}

		items = append(items, pickerItem{label: label, text: s.Name, payload: menuPayload{sess: s}})
	}

	for _, c := range palette {
		items = append(items, pickerItem{
			label:   c.label,
			cmd:     true,
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

// spawnedMsg is delivered when a new session's daemon has started.
type spawnedMsg struct {
	id  string
	err error
}

// Update satisfies tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.pick.setWidth(msg.Width)
		m.input.SetWidth(msg.Width)

		return m, nil
	case spawnedMsg:
		if msg.err != nil {
			m.status = "failed to start session: " + msg.err.Error()
			m.showMenu()

			return m, nil
		}

		return m.attach(msg.id, proto.HistNone, 0)
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

// attach resolves a chosen session into the menu's Result and quits. It never
// runs the relay or switches here: app.Run does that once the menu has fully torn
// down, so the inline picker is erased before the target session's output lands
// (like fzf clearing its prompt on exit). Inside a session the pick switches that
// session's relay; otherwise it attaches a relay on this terminal.
func (m Model) attach(id string, hist proto.HistMode, lines uint32) (Model, tea.Cmd) {
	action := ActionAttach
	if m.curSession != "" {
		action = ActionSwitch
	}

	m.result = Result{Action: action, ID: id, Hist: hist, Lines: lines}
	m.quit = true

	return m, tea.Quit
}

// Result reports what the user resolved the menu to. It is meaningful once the
// program has exited; app.Run reads it to attach, switch, or just leave tm.
func (m Model) Result() Result { return m.result }

// WithStatus returns the model with a status line set, used to carry a note
// (e.g. a reaped dead session) into a freshly opened menu.
func (m Model) WithStatus(s string) Model {
	m.status = s

	return m
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

	return m.attach(id, p.hist, p.lines)
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

		return m.attach(id, proto.HistLines, uint32(n))
	}

	return m, nil
}

// View satisfies tea.Model. The menu renders inline (not in the alternate
// screen) so opening it does not blank the current session's screen: the picker
// appears beneath the existing output, and on a switch the target session's
// history replays right after it instead of onto a cleared screen.
func (m Model) View() tea.View {
	if m.quit {
		return tea.View{}
	}

	content := m.viewPick()
	if m.mode == modeInput {
		content = m.viewInput()
	}

	return tea.View{Content: content, AltScreen: false}
}

// maxRows caps the inline picker's list height so a long session list stays
// compact (paginating) rather than pushing the screen above it out of view.
const maxRows = 15

func (m Model) viewPick() string {
	th := styles()

	var b strings.Builder

	header := th.title.Render("tm") + "  " + th.dim.Render("namespace: "+m.ns)
	if m.curSession != "" {
		header += "  " + th.session.Render("in session: "+m.curSession)
	}

	b.WriteString(header)
	b.WriteString("\n\n")
	b.WriteString(m.pick.view())
	b.WriteString("\n")
	b.WriteString(m.footer())

	return b.String()
}

func (m Model) footer() string {
	th := styles()

	keys := `↑/↓ move · type to filter · enter select · esc back`
	if m.pickFor == pickMenu {
		if m.curSession != "" {
			// Inside a session, picking another one switches this terminal to it
			// rather than nesting; esc returns to the current session.
			keys = "↑/↓ move · type to filter · enter switch session · esc back to current session"
		} else {
			keys = "↑/↓ move · type to filter · enter select · " +
				`esc quit (sessions keep running) · Ctrl-\ in a session detaches to your shell`
		}
	}

	help := th.dim.Render(keys)
	if m.status != "" {
		return th.status.Render(m.status) + "\n" + help
	}

	return help
}

func (m Model) viewInput() string {
	th := styles()

	return th.title.Render("TM") + "\n\n" + m.input.View() + "\n\n" +
		th.dim.Render("enter confirm · esc cancel")
}

type theme struct {
	title   lipgloss.Style
	dim     lipgloss.Style
	sel     lipgloss.Style
	cmd     lipgloss.Style
	status  lipgloss.Style
	session lipgloss.Style
}

// styles builds the lipgloss styles once, on first render.
var styles = sync.OnceValue(func() theme {
	return theme{
		title:   lipgloss.NewStyle().Bold(true),
		dim:     lipgloss.NewStyle().Faint(true),
		sel:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		cmd:     lipgloss.NewStyle().Foreground(lipgloss.Color("13")),
		status:  lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
		session: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")),
	}
})
