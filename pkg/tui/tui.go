// Package tui implements the interactive Bubble Tea menu: a fuzzy-filterable
// list of commands and sessions. Selecting a session (or creating one) hands
// the terminal to the relay via tea.ExecProcess; on return the menu refreshes.
package tui

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"charm.land/bubbles/v2/textinput"
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

type listItem struct {
	label   string
	isCmd   bool
	cmdID   cmdID
	aliases []string // command mnemonics, for matching
	name    string   // session name, for matching
	sess    store.Session
}

type mode int

const (
	modeList mode = iota
	modeInput
	modeChoose
)

type inputPurpose int

const (
	inputNewSession inputPurpose = iota
	inputNewNamespace
	inputCustomLines
)

type choosePurpose int

const (
	chooseScrollback choosePurpose = iota
	chooseUseNamespace
	chooseDropNamespace
)

type choice struct {
	label string
	value string
}

// Model is the Bubble Tea menu model.
type Model struct {
	st   *store.Store
	ctrl Controller
	ns   string

	items  []listItem
	order  []int // filtered indices into items
	cursor int
	query  string

	mode         mode
	input        textinput.Model
	inputPurpose inputPurpose

	choices       []choice
	chooseCursor  int
	choosePurpose choosePurpose
	pendingID     string // session awaiting a scrollback choice

	status string
	quit   bool
}

// New builds the menu model over a store and controller.
func New(st *store.Store, ctrl Controller) Model {
	ti := textinput.New()
	ti.Prompt = "> "
	m := Model{st: st, ctrl: ctrl, ns: store.DefaultNamespace, input: ti}
	m.reload()

	return m
}

// Init satisfies tea.Model.
func (m Model) Init() tea.Cmd { return nil }

func (m *Model) reload() {
	items := make([]listItem, 0, len(palette)+8)
	for _, c := range palette {
		items = append(items, listItem{
			label:   c.label,
			isCmd:   true,
			cmdID:   c.id,
			aliases: c.aliases,
		})
	}

	sessions, _ := m.st.ListByNamespace(m.ns)
	for _, s := range sessions {
		label := s.Name
		if m.ns == store.AllNamespaces {
			label = s.Name + "  (" + s.Namespace + ")"
		}

		items = append(items, listItem{label: label, name: s.Name, sess: s})
	}

	m.items = items
	m.applyFilter()
}

func (m *Model) applyFilter() {
	m.order = filterItems(m.items, m.query)
	if m.cursor >= len(m.order) {
		m.cursor = 0
	}
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
	case relayDoneMsg:
		m.mode = modeList
		if msg.err != nil {
			m.status = "session ended: " + msg.err.Error()
		} else {
			m.status = ""
		}

		m.reload()

		return m, nil
	case spawnedMsg:
		if msg.err != nil {
			m.mode = modeList
			m.status = "failed to start session: " + msg.err.Error()
			m.reload()

			return m, nil
		}

		return m, m.attach(msg.id, proto.HistNone, 0)
	case tea.KeyPressMsg:
		switch m.mode {
		case modeList:
			return m.updateList(msg)
		case modeInput:
			return m.updateInput(msg)
		case modeChoose:
			return m.updateChoose(msg)
		}
	}

	return m, nil
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

func (m Model) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", keyEsc:
		m.quit = true

		return m, tea.Quit
	case "up", "ctrl+k":
		if m.cursor > 0 {
			m.cursor--
		}

		return m, nil
	case "down", "ctrl+j":
		if m.cursor < len(m.order)-1 {
			m.cursor++
		}

		return m, nil
	case keyEnter:
		return m.selectCurrent()
	case "backspace":
		if len(m.query) > 0 {
			m.query = m.query[:len(m.query)-1]
			m.applyFilter()
		}

		return m, nil
	default:
		if msg.Text != "" {
			m.query += msg.Text
			m.applyFilter()
		}

		return m, nil
	}
}

func (m Model) selectCurrent() (tea.Model, tea.Cmd) {
	if len(m.order) == 0 {
		return m, nil
	}

	it := m.items[m.order[m.cursor]]
	if !it.isCmd {
		m.pendingID = it.sess.ID
		m.enterScrollbackChoose()

		return m, nil
	}

	switch it.cmdID {
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
		m.enterNamespaceChoose(chooseUseNamespace)
	case cmdDropNamespace:
		m.enterNamespaceChoose(chooseDropNamespace)
	}

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

func (m *Model) enterScrollbackChoose() {
	m.mode = modeChoose
	m.choosePurpose = chooseScrollback
	m.chooseCursor = 0
	m.choices = []choice{
		{"All history", "all"},
		{"One page", "page"},
		{"Last 100 lines", "100"},
		{"Last 1000 lines", "1000"},
		{"Custom number of lines…", "custom"},
	}
}

func (m *Model) enterNamespaceChoose(p choosePurpose) {
	m.mode = modeChoose
	m.choosePurpose = p
	m.chooseCursor = 0
	names, _ := m.st.ListNamespaces()

	var ch []choice
	if p == chooseUseNamespace {
		ch = append(ch, choice{"* (all sessions)", store.AllNamespaces})
	}

	for _, n := range names {
		if p == chooseDropNamespace && n == store.DefaultNamespace {
			continue
		}

		ch = append(ch, choice{n, n})
	}

	m.choices = ch
}

func (m Model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEsc:
		m.mode = modeList

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
		m.mode = modeList
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

		err := m.st.CreateNamespace(val)
		if err != nil {
			m.status = err.Error()
		} else {
			m.ns = val
			m.status = "namespace: " + val
		}

		m.mode = modeList
		m.query = ""
		m.reload()

		return m, nil
	case inputCustomLines:
		n, err := strconv.Atoi(val)
		if err != nil || n <= 0 {
			m.status = "enter a positive number"

			return m, nil
		}

		id := m.pendingID
		m.mode = modeList

		return m, m.attach(id, proto.HistLines, uint32(n))
	}

	return m, nil
}

func (m Model) updateChoose(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEsc:
		m.mode = modeList

		return m, nil
	case "up", "ctrl+k":
		if m.chooseCursor > 0 {
			m.chooseCursor--
		}

		return m, nil
	case "down", "ctrl+j":
		if m.chooseCursor < len(m.choices)-1 {
			m.chooseCursor++
		}

		return m, nil
	case keyEnter:
		if len(m.choices) == 0 {
			m.mode = modeList

			return m, nil
		}

		return m.submitChoose(m.choices[m.chooseCursor])
	}

	return m, nil
}

func (m Model) submitChoose(ch choice) (tea.Model, tea.Cmd) {
	switch m.choosePurpose {
	case chooseScrollback:
		id := m.pendingID

		switch ch.value {
		case "all":
			m.mode = modeList

			return m, m.attach(id, proto.HistAll, 0)
		case "page":
			m.mode = modeList

			return m, m.attach(id, proto.HistPage, 0)
		case "custom":
			m.enterInput(inputCustomLines, "Number of lines:", "")

			return m, nil
		default:
			n, _ := strconv.Atoi(ch.value)
			m.mode = modeList

			return m, m.attach(id, proto.HistLines, uint32(n))
		}
	case chooseUseNamespace:
		m.ns = ch.value
		m.status = "namespace: " + ch.value
		m.mode = modeList
		m.query = ""
		m.reload()

		return m, nil
	case chooseDropNamespace:
		err := m.st.DeleteNamespace(ch.value)
		if err != nil {
			m.status = err.Error()
		} else {
			if m.ns == ch.value {
				m.ns = store.DefaultNamespace
			}

			m.status = "dropped namespace: " + ch.value
		}

		m.mode = modeList
		m.query = ""
		m.reload()

		return m, nil
	}

	return m, nil
}

// View satisfies tea.Model. The menu runs in the alternate screen so the
// relayed shell (run via tea.ExecProcess) owns the main screen's scrollback.
func (m Model) View() tea.View {
	if m.quit {
		return tea.View{}
	}

	var content string

	switch m.mode {
	case modeInput:
		content = m.viewInput()
	case modeChoose:
		content = m.viewChoose()
	default:
		content = m.viewList()
	}

	return tea.View{Content: content, AltScreen: true}
}

const maxRows = 15

func (m Model) viewList() string {
	th := styles()

	var b strings.Builder

	b.WriteString(th.title.Render("tm") + "  " + th.dim.Render("namespace: "+m.ns) + "\n\n")
	b.WriteString("> " + m.query + "\n\n")

	if len(m.order) == 0 {
		b.WriteString(th.dim.Render("  (no matches)") + "\n")
	}

	for i, idx := range m.order {
		if i >= maxRows {
			b.WriteString(th.dim.Render("  …more") + "\n")

			break
		}

		it := m.items[idx]
		if i == m.cursor {
			b.WriteString("> " + th.sel.Render(it.label) + "\n")
		} else {
			b.WriteString("  " + it.label + "\n")
		}
	}

	b.WriteString("\n" + m.footer())

	return b.String()
}

func (m Model) footer() string {
	th := styles()

	help := th.dim.Render(`↑/↓ move · enter select · esc quit (sessions keep running) · Ctrl-\ back to menu from a shell`)

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

func (m Model) viewChoose() string {
	th := styles()

	var b strings.Builder

	b.WriteString(th.title.Render("tm") + "\n\n")

	for i, ch := range m.choices {
		if i == m.chooseCursor {
			b.WriteString("> " + th.sel.Render(ch.label) + "\n")
		} else {
			b.WriteString("  " + ch.label + "\n")
		}
	}

	b.WriteString("\n" + th.dim.Render("↑/↓ move · enter select · esc cancel"))

	return b.String()
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
