// Package tui implements the interactive Bubble Tea menu: a fuzzy-filterable
// list of commands and sessions. Selecting a session (or creating one) records
// what to do (see Result) and quits; app.Run carries it out — attaching the
// relay or, inside a session, switching it — once the menu has torn down, so the
// inline picker is erased from the screen first. The menu itself runs no relay.
//
// Every menu — the main list, the kill/rename/clear choosers, the namespace
// chooser — is the same type-to-filter picker (see picker.go), so they share keys
// and behavior. All text entry — the picker's filter and the free-text prompts
// (naming a session) — is a single-line textarea built by newInput.
package tui

import (
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ysmood/tm/pkg/attach"
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
	// KillSession ends the session with the given id: its daemon terminates the
	// shell and removes the session's files. A session whose daemon is unreachable
	// (already dead) is removed from the store directly.
	KillSession(id string) error
	// ClearHistory wipes the session's recorded history — its daemon truncates the
	// log file it records to — so nothing of it can leak through a later replay.
	// The session keeps running.
	ClearHistory(id string) error
}

// Action is what the user resolved the menu to when it exited; app.Run carries
// it out after the menu has torn down (so the picker is erased from the screen
// first).
type Action int

const (
	// ActionNone means the user cancelled the menu (esc or Ctrl-C). At the top
	// level app.Run leaves tm; when the menu was opened from within a session (via
	// Ctrl-\) app.Run instead resumes that session.
	ActionNone Action = iota
	// ActionAttach means run the relay for Result.ID on this terminal (the menu
	// was not launched from within a session).
	ActionAttach
	// ActionSwitch means hand the current session's relay to Result.ID (the menu
	// was launched from within a session, so picking another moves this terminal
	// there instead of nesting a new relay).
	ActionSwitch
	// ActionDetach is the [detach session] command: from within a session it
	// returns to the top-level menu (the session keeps running); at the top level,
	// where there is no session to detach from, app.Run leaves tm.
	ActionDetach
	// ActionExit is the explicit [exit] command: leave tm from anywhere with every
	// session still running. Unlike ActionNone (esc/Ctrl-C/Ctrl-D) it never resumes
	// the current session, so [exit] leaves tm even from a menu opened mid-session.
	ActionExit
	// ActionKillCurrent is [kill session] aimed at the session this menu is framed
	// inside (Result.ID). Unlike killing a background session — done inline while
	// the menu stays open — ending the current one takes the screen (and, for a tm
	// run from within the session's shell, this very process) with it, so the menu
	// quits and app.Run tears the relay down around the kill.
	ActionKillCurrent
)

// Result is the menu's outcome: what to do, and which session it applies to.
type Result struct {
	Action Action
	ID     string
}

type cmdID int

const (
	cmdNewSession cmdID = iota
	cmdRenameSession
	cmdKillSession
	cmdClearHistory
	cmdDetachSession
	cmdExit
	cmdUseNamespace
	cmdDropNamespace
	cmdHelp
)

type paletteCmd struct {
	id    cmdID
	label string
	// key is the key press that runs this command directly from the main menu
	// (matched against tea.KeyPressMsg.String()); hint is how that key is shown
	// beside the label. Both are empty for commands with no shortcut.
	key  string
	hint string
}

// palette holds the fixed commands. The bracketed labels are fuzzy-matched like
// everything else (see rankItems), so typing the letters of a command in order
// surfaces it — "ds" finds [detach session], "un" finds [use namespace]. Some
// also carry a direct shortcut (see cmdForKey), shown dimmed beside the label.
var palette = []paletteCmd{
	{cmdNewSession, "[new session]", "ctrl+t", "Ctrl-T"},
	{cmdRenameSession, "[rename session]", "", ""},
	{cmdKillSession, "[kill session]", "", ""},
	{cmdClearHistory, "[clear history]", "", ""},
	{cmdDetachSession, "[detach session]", "ctrl+\\", "Ctrl-\\"},
	{cmdExit, "[exit]", "", ""},
	{cmdUseNamespace, "[use namespace]", "ctrl+g", "Ctrl-G"},
	{cmdDropNamespace, "[drop namespace]", "", ""},
	{cmdHelp, "[help]", "", ""},
}

// cmdForKey maps a key press to the palette command it triggers directly, if
// any. It backs the main-menu shortcuts (see updatePick), so a key like Ctrl-\
// runs [detach session] without moving the cursor onto it first.
func cmdForKey(key string) (cmdID, bool) {
	for _, c := range palette {
		if c.key != "" && c.key == key {
			return c.id, true
		}
	}

	return 0, false
}

// menuPayload is the data attached to a main-menu row: either a command or a
// session.
type menuPayload struct {
	isCmd bool
	cmdID cmdID
	sess  store.Session
}

// newNamespacePayload is the [use namespace] picker's "create" row: choosing it
// makes the typed namespace and switches to it. It folds what was a separate
// [new namespace] command into [use namespace] — typing a name no namespace has
// yet surfaces this row (see showNamespaces).
type newNamespacePayload struct{ name string }

// renamePayload is the data attached to a rename-chooser row: the session the
// name prompt will rename, and the name it prefills with.
type renamePayload struct{ id, name string }

// killPayload is the data attached to a kill-chooser row: the session to end,
// and its name for the notice printed afterwards.
type killPayload struct{ id, name string }

// clearPayload is the data attached to a clear-history-chooser row: the session
// whose history to wipe, and its name for the notice printed afterwards.
type clearPayload struct{ id, name string }

// currentHint marks the session this menu is framed inside in choosers that
// keep it (rename, kill, clear history), so it reads apart from the others —
// and picking it in the kill chooser, which also ends what is on this
// terminal, is a deliberate act.
const currentHint = "current"

type mode int

const (
	modePick  mode = iota // a type-to-filter menu (main, namespace)
	modeInput             // a free-text prompt
	modeHelp              // the detailed help screen ([help] command)
)

// pickPurpose says what the active picker selects, so Enter dispatches correctly.
type pickPurpose int

const (
	pickMenu pickPurpose = iota
	pickRenameSession
	pickKillSession
	pickClearHistory
	pickUseNamespace
	pickDropNamespace
)

type inputPurpose int

const (
	inputNewSession inputPurpose = iota
	inputRenameSession
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

	pendingID string // session awaiting a scrollback choice or a new name
	// pendingName is that session's name as the rename prompt opened, kept so the
	// notice printed afterwards can name both sides of the change.
	pendingName string

	// width is the terminal width, used to size the bordered boxes and the picker
	// content inside them (see setWidth, box).
	width int

	// result is what the menu resolved to; app.Run reads it via Result after the
	// program exits and carries it out (attach, switch, or nothing).
	result Result

	// notices are the lines printed above the picker while this menu was open
	// (renames, kills, cleared histories — see printNotice). app.Run reads them
	// via Notices after the program exits: a menu opened over a session draws
	// below the shell's prompt, so tea.Println lands them below it too, where the
	// resumed session would overwrite them — the caller repositions them above
	// the prompt instead (see promptGuard).
	notices []string

	status string
	quit   bool
}

const (
	// boxChrome is the horizontal cells a bordered box adds around its content:
	// the two vertical borders plus a one-space pad on each side.
	boxChrome = 4
	// minBoxWidth keeps a box drawable before the first WindowSizeMsg arrives and
	// on very narrow terminals.
	minBoxWidth = 24
)

// New builds the menu model over a store and controller.
func New(st *store.Store, ctrl Controller) Model {
	m := Model{st: st, ctrl: ctrl, ns: store.DefaultNamespace, input: newInput("> "), pick: newPicker()}
	m.curSessionID, m.curSession = ctrl.CurrentSession()
	m.setWidth(80) // a sane default until the first WindowSizeMsg
	m.showMenu()

	return m
}

// setWidth records the terminal width and sizes the picker's input and list to
// the content area inside the box border (the terminal width minus the box
// chrome), so their rows fill each bordered line exactly without wrapping.
func (m *Model) setWidth(w int) {
	if w < minBoxWidth {
		w = minBoxWidth
	}

	m.width = w
	inner := w - boxChrome
	m.pick.setWidth(inner)
	m.input.SetWidth(inner)
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

		items = append(items, pickerItem{label: m.sessionLabel(s), text: s.Name, payload: menuPayload{sess: s}})
	}

	for _, c := range palette {
		// [detach session] only makes sense from within a session: at the top level
		// there is nothing to detach from, so the row is dropped and its Ctrl-\ key
		// acts as [exit] instead (see cmdForKey's caller), advertised on that row.
		if c.id == cmdDetachSession && m.curSession == "" {
			continue
		}

		item := pickerItem{
			label:   c.label,
			isCmd:   true,
			hint:    c.hint,
			payload: menuPayload{isCmd: true, cmdID: c.id},
		}
		// [exit]'s keys, top level only (esc resumes the session when the menu is
		// open over one). Ctrl-\ always leaves — with no session to detach from it
		// falls through to [exit] — and esc and Ctrl-D (VEOF) end the menu on an
		// empty filter, but Ctrl-D is a no-op once a query is typed — so react to
		// the query and drop it once there is content.
		if c.id == cmdExit && m.curSession == "" {
			item.hintFn = func(query string) string {
				if query == "" {
					return "Ctrl-\\, esc or Ctrl-D"
				}

				return "Ctrl-\\ or esc"
			}
		}

		items = append(items, item)
	}

	return items
}

// sessionLabel is how a session reads as a list row: its name, carrying its
// namespace in the "*" (all sessions) view where names from different namespaces
// sit side by side.
func (m *Model) sessionLabel(s store.Session) string {
	if m.ns == store.AllNamespaces {
		return s.Name + "  (" + s.Namespace + ")"
	}

	return s.Name
}

// showMenu returns to the main command/session list.
func (m *Model) showMenu() {
	m.mode = modePick
	m.pickFor = pickMenu
	m.pick.setItems(m.menuItems())
}

// showRenameSessions lists the sessions [rename session] can target, and reports
// false when the namespace holds none, so the caller says so rather than opening
// an empty picker. Unlike the main menu it keeps the session this tm is running
// inside — marked "current", like the other choosers that keep it — since
// renaming the session you are in is the common case, and a rename never
// attaches, so there is nothing to re-enter.
func (m *Model) showRenameSessions() bool {
	sessions, _ := m.st.ListByNamespace(m.ns)
	if len(sessions) == 0 {
		return false
	}

	items := make([]pickerItem, 0, len(sessions))

	for _, s := range sessions {
		item := pickerItem{
			label:   m.sessionLabel(s),
			text:    s.Name,
			payload: renamePayload{id: s.ID, name: s.Name},
		}
		if s.ID == m.curSessionID {
			item.hint = currentHint
		}

		items = append(items, item)
	}

	m.mode = modePick
	m.pickFor = pickRenameSession
	m.pick.setItems(items)

	return true
}

// showKillSessions lists the sessions [kill session] can target, and reports
// false when the namespace holds none, so the caller says so rather than opening
// an empty picker. Unlike the main attach list it keeps the session this tm is
// running inside — marked "current", since killing it also ends what is on this
// terminal (see ActionKillCurrent) — so a stuck shell can be put down without
// leaving it first.
func (m *Model) showKillSessions() bool {
	sessions, _ := m.st.ListByNamespace(m.ns)
	if len(sessions) == 0 {
		return false
	}

	items := make([]pickerItem, 0, len(sessions))

	for _, s := range sessions {
		item := pickerItem{
			label:   m.sessionLabel(s),
			text:    s.Name,
			payload: killPayload{id: s.ID, name: s.Name},
		}
		if s.ID == m.curSessionID {
			item.hint = currentHint
		}

		items = append(items, item)
	}

	m.mode = modePick
	m.pickFor = pickKillSession
	m.pick.setItems(items)

	return true
}

// showClearSessions lists the sessions [clear history] can target, and reports
// false when the namespace holds none, so the caller says so rather than
// opening an empty picker. Like the kill chooser it keeps the session this tm
// is running inside — marked "current" — since wiping the history of the
// session you are in is the common case (a secret was just echoed there), and a
// clear never disturbs what is on screen.
func (m *Model) showClearSessions() bool {
	sessions, _ := m.st.ListByNamespace(m.ns)
	if len(sessions) == 0 {
		return false
	}

	items := make([]pickerItem, 0, len(sessions))

	for _, s := range sessions {
		item := pickerItem{
			label:   m.sessionLabel(s),
			text:    s.Name,
			payload: clearPayload{id: s.ID, name: s.Name},
		}
		if s.ID == m.curSessionID {
			item.hint = currentHint
		}

		items = append(items, item)
	}

	m.mode = modePick
	m.pickFor = pickClearHistory
	m.pick.setItems(items)

	return true
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

	// [use namespace] also creates: typing a name no namespace has yet surfaces a
	// row that makes it and switches to it, so there is no separate [new namespace]
	// command. The row tracks the query, so it disappears once the name matches an
	// existing namespace exactly.
	if p == pickUseNamespace {
		m.pick.setExtra(func(q string) []pickerItem {
			q = strings.TrimSpace(q)
			if q == "" || slices.Contains(names, q) {
				return nil
			}

			return []pickerItem{{
				label:   "[new namespace] " + q,
				isCmd:   true,
				payload: newNamespacePayload{name: q},
			}}
		})
	}
}

// Update satisfies tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.setWidth(msg.Width)

		return m, nil
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
		case modeHelp:
			// Any key dismisses the help screen back to the main menu.
			m.showMenu()

			return m, nil
		}
	}

	return m, nil
}

// attach resolves a chosen session into the menu's Result and quits. It never
// runs the relay or switches here: app.Run does that once the menu has fully torn
// down, so the inline picker is erased before the target session's output lands
// (like fzf clearing its prompt on exit). Inside a session the pick switches that
// session's relay; otherwise it attaches a relay on this terminal. Either way the
// session's last window of output is replayed on arrival — there is no history to
// choose from, so picking a session is one keypress.
func (m Model) attach(id string) (Model, tea.Cmd) {
	action := ActionAttach
	if m.curSession != "" {
		action = ActionSwitch
	}

	m.result = Result{Action: action, ID: id}
	m.quit = true

	return m, tea.Quit
}

// Result reports what the user resolved the menu to. It is meaningful once the
// program has exited; app.Run reads it to attach, switch, or just leave tm.
func (m Model) Result() Result { return m.result }

// Notices reports the lines this menu printed above its picker (renames, kills,
// cleared histories), in order. Meaningful once the program has exited; app.Run
// uses them to fix the notices' position when the menu was drawn over a live
// session's prompt.
func (m Model) Notices() []string { return m.notices }

// printNotice prints line above the picker, where it stays in the scrollback.
// tea.Println lands the line unmanaged by the program, so it survives the
// picker's redraws and its teardown — the same trail the attach/detach notices
// leave. The line is recorded too (see Notices): a menu opened over a session
// draws below the shell's prompt, so tea.Println lands it below the prompt as
// well, where the resumed session's next output would overwrite it — app.Run
// repositions the recorded rows above the prompt instead. Truncated to the
// terminal width so it occupies exactly one row, since that repositioning
// counts rows and a wrapped line would throw the arithmetic off.
func (m *Model) printNotice(line string) tea.Cmd {
	notice := ansi.Truncate(line, max(1, m.width-1), "…")
	m.notices = append(m.notices, notice)

	return tea.Println(notice)
}

// WithStatus returns the model with a status line set, used to carry a note
// (e.g. a reaped dead session) into a freshly opened menu.
func (m Model) WithStatus(s string) Model {
	m.status = s

	return m
}

// WithNamespace opens the menu filtered to ns instead of the default namespace,
// backing the TM_NAMESPACE env var. New sessions created from the menu then land
// in ns (see targetNamespace).
func (m Model) WithNamespace(ns string) Model {
	m.ns = ns
	m.showMenu() // rebuild the list so it shows ns's sessions

	return m
}

// WithCurrentSession frames the menu as running inside the given session even
// when this tm was not launched from that session's shell. app.Run uses it when
// Ctrl-\ reopens the menu from the relay: that process has no $TM_SESSION, so the
// in-session framing — the header hint, hiding the current session from the list,
// and esc meaning "back to it" — has to be set explicitly from the session the
// relay is attached to.
func (m Model) WithCurrentSession(id, name string) Model {
	m.curSessionID = id
	m.curSession = name

	// Frame the menu in the current session's namespace too, not just by name. A
	// menu reopened over a session (Ctrl-\) runs in a top-level tm process with no
	// TM_NAMESPACE of its own, so without this it would revert to the default
	// namespace even when the session lives in another one.
	if s, err := m.st.GetSession(id); err == nil && s.Namespace != "" {
		m.ns = s.Namespace
	}

	m.showMenu() // rebuild the list so the current session drops out of it

	return m
}

// Key names compared against tea.KeyMsg.String().
const (
	keyEsc   = "esc"
	keyEnter = "enter"
	keyCtrlD = "ctrl+d"
)

func (m Model) updatePick(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Command shortcuts fire only on the main menu, where the bracketed commands
	// live; the scrollback and namespace sub-pickers keep these keys for
	// navigation and text entry. Checked before the picker sees the key so a
	// shortcut acts even mid-query (e.g. Ctrl-\ detaches while a filter is typed).
	if m.pickFor == pickMenu {
		if id, ok := cmdForKey(msg.String()); ok {
			// At the top level [detach session] is not offered — there is nothing to
			// detach from — so its Ctrl-\ runs [exit], the binding the menu advertises
			// on that row.
			if id == cmdDetachSession && m.curSession == "" {
				id = cmdExit
			}

			return m.selectMenu(menuPayload{isCmd: true, cmdID: id})
		}
	}

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
	case pickRenameSession:
		if p, ok := it.payload.(renamePayload); ok {
			m.pendingID, m.pendingName = p.id, p.name
			m.enterInput(inputRenameSession, "Rename session:", p.name)
		}
	case pickKillSession:
		if p, ok := it.payload.(killPayload); ok {
			return m.killSession(p)
		}
	case pickClearHistory:
		if p, ok := it.payload.(clearPayload); ok {
			return m.clearHistory(p)
		}
	case pickUseNamespace:
		return m.selectNamespace(it.payload)
	case pickDropNamespace:
		if ns, ok := it.payload.(string); ok {
			return m.dropNamespace(ns)
		}
	}

	return m, nil
}

// selectNamespace handles a row picked from the [use namespace] chooser: the
// create row makes the typed namespace and switches to it, an existing name
// just switches.
func (m Model) selectNamespace(payload any) (tea.Model, tea.Cmd) {
	switch pl := payload.(type) {
	case newNamespacePayload:
		return m.createNamespace(pl.name)
	case string:
		m.ns = pl
		m.status = "namespace: " + pl
		m.showMenu()
	}

	return m, nil
}

func (m Model) selectMenu(p menuPayload) (tea.Model, tea.Cmd) {
	if !p.isCmd {
		return m.attach(p.sess.ID)
	}

	switch p.cmdID {
	case cmdNewSession:
		m.enterInput(inputNewSession, "New session name:", m.ctrl.DefaultSessionName(m.targetNamespace()))
	case cmdRenameSession:
		// Pick the session to rename first (the name prompt follows), unless the
		// namespace is empty — then there is nothing to pick.
		if !m.showRenameSessions() {
			m.status = "no sessions to rename"
		}
	case cmdKillSession:
		// Pick the session to kill first, unless the namespace is empty — then
		// there is nothing to pick.
		if !m.showKillSessions() {
			m.status = "no sessions to kill"
		}
	case cmdClearHistory:
		// Pick the session whose history to wipe first, unless the namespace is
		// empty — then there is nothing to pick.
		if !m.showClearSessions() {
			m.status = "no sessions to clear"
		}
	case cmdDetachSession:
		// Detach from the current session back to the top-level menu (the session
		// keeps running); app.Run carries that out. Reported as ActionDetach — not a
		// plain cancel — so a menu opened mid-session with Ctrl-\ detaches here while
		// esc resumes the session. To leave tm entirely, use [exit].
		m.result = Result{Action: ActionDetach}
		m.quit = true

		return m, tea.Quit
	case cmdExit:
		// Leave tm from anywhere (ActionExit), sessions still running. Distinct from
		// esc/Ctrl-C/Ctrl-D (ActionNone), which resume the current session when the
		// menu was opened over one; selecting [exit] leaves tm even from there.
		m.result = Result{Action: ActionExit}
		m.quit = true

		return m, tea.Quit
	case cmdUseNamespace:
		m.showNamespaces(pickUseNamespace)
	case cmdDropNamespace:
		m.showNamespaces(pickDropNamespace)
	case cmdHelp:
		m.mode = modeHelp
	}

	return m, nil
}

// killSession ends the chosen session: the controller asks its daemon to shut
// down, which terminates the shell and removes the session's files. The menu
// then returns to the main list — rebuilt, so the session is gone from it — and
// the kill is printed above the picker (like a rename), where it stays in the
// scrollback.
//
// The session this menu is framed inside is the exception: killing it also ends
// what this terminal is showing, so instead of killing inline the menu quits
// with ActionKillCurrent and app.Run tears the relay down around the kill.
func (m Model) killSession(p killPayload) (tea.Model, tea.Cmd) {
	if p.id == m.curSessionID {
		m.result = Result{Action: ActionKillCurrent, ID: p.id}
		m.quit = true

		return m, tea.Quit
	}

	if err := m.ctrl.KillSession(p.id); err != nil {
		m.status = "failed to kill session: " + err.Error()
		m.showMenu()

		return m, nil
	}

	m.showMenu()

	return m, m.printNotice(attach.KilledSessionNotice(p.name))
}

// clearHistory wipes the chosen session's recorded history: the controller asks
// its daemon to empty the in-memory scrollback and truncate the log file, so
// nothing of the session's past (say, a secret echoed to the terminal) can be
// replayed on a later attach. The session keeps running and — unlike a kill —
// clearing disturbs nothing on screen, so even the current session is handled
// inline: the menu returns to the main list and the wipe is printed above it,
// where it stays in the scrollback.
func (m Model) clearHistory(p clearPayload) (tea.Model, tea.Cmd) {
	if err := m.ctrl.ClearHistory(p.id); err != nil {
		m.status = "failed to clear history: " + err.Error()
		m.showMenu()

		return m, nil
	}

	m.showMenu()

	return m, m.printNotice(attach.ClearedHistoryNotice(p.name))
}

// createNamespace makes ns and switches the active view to it. It backs the
// [use namespace] picker's create row (choosing a typed name no namespace has
// yet), so creating and switching are one step.
func (m Model) createNamespace(ns string) (tea.Model, tea.Cmd) {
	if err := m.st.CreateNamespace(ns); err != nil {
		m.status = err.Error()
	} else {
		m.ns = ns
		m.status = "namespace: " + ns
	}

	m.showMenu()

	return m, nil
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
	case keyCtrlD:
		// Ctrl-D (VEOF) backs out to the menu like esc, but only on an empty prompt;
		// with text typed it does nothing.
		if m.input.Value() == "" {
			m.showMenu()
		}

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

		// Spawn synchronously and attach in the same step, so the menu tears down
		// straight from the name prompt to the new session — no intermediate frame.
		// Bubble Tea is mid-update here, so it never repaints the menu in between.
		id, err := m.ctrl.CreateAndSpawn(m.targetNamespace(), val)
		if err != nil {
			m.status = "failed to start session: " + err.Error()
			m.showMenu()

			return m, nil
		}

		// The attach replays the session's window, which is what makes the shell's
		// first prompt show up: the daemon reports ready as soon as the shell starts,
		// which can be after it has already printed its prompt into the log — common
		// on Linux, rare on macOS — so without a replay the prompt is recorded but
		// never sent, and the new session opens to a blank screen until the user
		// presses Enter. A brand-new session's window is just that prompt, so the
		// replay shows nothing more.
		return m.attach(id)
	case inputRenameSession:
		// A rejected name (empty, or one a sibling session already holds) leaves the
		// prompt open with the typed text intact — the header carries the reason — so
		// the user can edit it rather than start over; esc backs out.
		if err := m.st.RenameSession(m.pendingID, val); err != nil {
			m.status = err.Error()

			return m, nil
		}

		// The header names the session this menu is framed as running inside, so it
		// has to follow the rename too.
		if m.pendingID == m.curSessionID {
			m.curSession = val
		}

		old := m.pendingName
		m.showMenu() // rebuild the list so the session reads by its new name

		if old == val {
			return m, nil // submitted the name it already had: nothing happened, say nothing
		}

		return m, m.printNotice(attach.RenamedSessionNotice(old, val))
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

	switch m.mode {
	case modeInput:
		content = m.viewInput()
	case modeHelp:
		content = m.viewHelp()
	}

	return tea.View{Content: content, AltScreen: false}
}

// maxRows caps the inline picker's list height so a long session list stays
// compact rather than pushing the screen above it out of view; beyond it the
// list pages internally as the cursor moves (see newPicker).
const maxRows = 10

// viewPick frames the menu as two stacked boxes sharing a divider: the query box
// (its top border titled with the session/namespace/status header) above the list
// box, matching the inline picker layout.
func (m Model) viewPick() string {
	return m.box(m.headerTitle(), []string{m.pick.inputView(), m.pick.listView()})
}

// headerTitle is the text embedded in the top border of the query box: the
// session (when nested), the active namespace, and any transient status note.
func (m Model) headerTitle() string {
	th := styles()

	title := th.dim.Render("namespace: ") + th.item.Render(m.ns)
	if m.curSession != "" {
		title = th.dim.Render("session: ") + th.item.Render(m.curSession) + th.dim.Render(" · ") + title
	}

	if m.status != "" {
		title += th.dim.Render(" · ") + th.status.Render(m.status)
	}

	return title
}

// box draws a rounded border around one or more stacked sections, the title
// embedded in the top border and a divider drawn between sections. Every menu
// view is framed with it; the width is m.width and each content line is fitted to
// the inner area so the borders stay aligned (see padTrunc).
func (m Model) box(title string, sections []string) string {
	th := styles()
	bd := lipgloss.RoundedBorder()
	contentW := m.width - boxChrome
	left := th.box.Render(bd.Left)
	right := th.box.Render(bd.Right)

	// rule draws a full-width horizontal edge between the given corners (the
	// divider and the bottom border).
	rule := func(l, r string) string {
		return th.box.Render(l + strings.Repeat(bd.Top, m.width-2) + r)
	}

	var b strings.Builder

	b.WriteString(m.topBorder(title))

	for i, sec := range sections {
		if i > 0 {
			b.WriteString("\n")
			b.WriteString(rule(bd.MiddleLeft, bd.MiddleRight))
		}

		for ln := range strings.SplitSeq(sec, "\n") {
			b.WriteString("\n")
			b.WriteString(left)
			b.WriteString(" ")
			b.WriteString(padTrunc(ln, contentW))
			b.WriteString(" ")
			b.WriteString(right)
		}
	}

	b.WriteString("\n")
	b.WriteString(rule(bd.BottomLeft, bd.BottomRight))

	return b.String()
}

// topBorder builds the box's top edge with the title sitting just after the left
// corner and border fill running out to the right corner, truncating the title
// when it would not fit.
func (m Model) topBorder(title string) string {
	th := styles()
	bd := lipgloss.RoundedBorder()

	left := bd.TopLeft + bd.Top                                              // "╭─"
	room := m.width - lipgloss.Width(left) - lipgloss.Width(bd.TopRight) - 2 // 2 = the spaces around the title
	title = ansi.Truncate(title, max(0, room), "…")
	label := " " + title + " "

	fill := max(0, m.width-lipgloss.Width(left)-lipgloss.Width(label)-lipgloss.Width(bd.TopRight))

	return th.box.Render(left) + label + th.box.Render(strings.Repeat(bd.Top, fill)+bd.TopRight)
}

// padTrunc fits s to exactly w display cells: ANSI-aware truncation when it is too
// wide, space padding when too short, so each boxed line ends flush at the border.
func padTrunc(s string, w int) string {
	w = max(0, w)
	s = ansi.Truncate(s, w, "")

	if gap := w - lipgloss.Width(s); gap > 0 {
		s += strings.Repeat(" ", gap)
	}

	return s
}

// viewHelp renders the detailed help reached via the [help] command. It documents
// the keys and what each command does — kept off the main menu so that screen
// stays uncluttered (see viewPick).
func (m Model) viewHelp() string {
	th := styles()

	var b strings.Builder

	key := th.cmd.Width(18)
	section := func(title string, rows [][2]string) {
		b.WriteString(th.session.Render(title))
		b.WriteString("\n")

		for _, r := range rows {
			b.WriteString("  ")
			b.WriteString(key.Render(r[0]))
			b.WriteString(r[1])
			b.WriteString("\n")
		}

		b.WriteString("\n")
	}

	section("Keys", [][2]string{
		{"↑/↓", "move the cursor (also Ctrl-P/N or Ctrl-K/J)"},
		{"type", "fuzzy-filter the list"},
		{"enter", "select the highlighted row"},
		{"esc", "resume the session, or leave tm from the main menu"},
		{"Ctrl-C", "quit"},
		{`Ctrl-\`, "open this menu from a session; detach back to the menu; at the top level: leave tm"},
		{"Ctrl-T", "new session (from the main menu)"},
		{"Ctrl-G", "use namespace (from the main menu)"},
		{"Ctrl-D", "like esc when the filter is empty (EOF)"},
	})

	switchHint := "attach to a session"
	if m.curSession != "" {
		switchHint = "switch this terminal to a session"
	}

	section("Commands", [][2]string{
		{"<session>", switchHint},
		{"[new session]", "create and start a new session"},
		{"[rename session]", "rename a session; it keeps running"},
		{"[kill session]", "end a session's shell and delete it (the current one too)"},
		{"[clear history]", "wipe a session's recorded scrollback (its log file) — e.g. leaked secrets"},
		{"[detach session]", "detach back to the menu; the session keeps running (shown inside a session)"},
		{"[exit]", "leave tm; every session keeps running"},
		{"[use namespace]", "switch namespace, or type a new name to create one (* shows all)"},
		{"[drop namespace]", "delete a namespace"},
		{"[help]", "show this help"},
	})

	version, repo := buildInfo()
	section("About", [][2]string{
		{"version", version},
		{"repo", repo},
	})

	return m.box("tm — help", []string{strings.TrimRight(b.String(), "\n")}) +
		"\n" + th.dim.Render("press any key to go back")
}

// buildInfo reports the module version and GitHub repo URL recorded in the
// binary at build time, via debug.ReadBuildInfo. Go stamps the version from the
// VCS tag for `go install`ed builds; a plain `go build` from a checkout reports
// "(devel)". The repo URL is derived from the main module path.
func buildInfo() (version, repo string) {
	version, repo = "unknown", "https://github.com/ysmood/tm"

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, repo
	}

	if info.Main.Version != "" {
		version = info.Main.Version
	}

	if info.Main.Path != "" {
		repo = "https://" + info.Main.Path
	}

	return version, repo
}

// viewInput frames the active free-text prompt (naming a session, a custom line
// count) in the same box as the menu, its header carried in the top border.
func (m Model) viewInput() string {
	return m.box(m.headerTitle(), []string{m.input.View()})
}

type theme struct {
	title   lipgloss.Style
	dim     lipgloss.Style
	sel     lipgloss.Style
	cmd     lipgloss.Style
	key     lipgloss.Style
	status  lipgloss.Style
	session lipgloss.Style
	item    lipgloss.Style
	box     lipgloss.Style
}

// styles builds the lipgloss styles once, on first render.
var styles = sync.OnceValue(func() theme {
	return theme{
		title: lipgloss.NewStyle().Bold(true),
		dim:   lipgloss.NewStyle().Faint(true),
		sel:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		cmd:   lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		// A dimmer grey than cmd for the shortcut hints, so a key like "Ctrl-\"
		// recedes behind its command label (see pickerDelegate.Render).
		key:     lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
		status:  lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
		session: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10")),
		item:    lipgloss.NewStyle().Foreground(lipgloss.Color("15")),
		box:     lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
	}
})
