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

// ctrlKey builds a Ctrl-<code> key press, e.g. ctrlKey('t') stringifies to
// "ctrl+t" — the form the main-menu shortcuts match against.
func ctrlKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl}
}

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

// Creating a new session spawns it and attaches in one step: the menu quits
// straight from the name prompt with no intermediate "starting…" frame.
func TestModelNewSessionAttachesImmediately(t *testing.T) {
	g := got.T(t)
	m := New(newStore(g, t), fakeCtrl{})

	m = send(m, keyEnterMsg) // empty store: cursor sits on [new session] -> name prompt
	g.Eq(m.mode, modeInput)

	next, cmd := m.Update(keyEnterMsg) // submit the default name
	final := next.(Model)

	g.True(final.quit)
	g.NotNil(cmd)

	res := final.Result()
	g.Eq(res.Action, ActionAttach)
	g.Eq(res.ID, "id") // fakeCtrl.CreateAndSpawn
	// HistAll, not HistNone: replay history so the shell's first prompt is shown
	// even when the daemon already printed it before the relay attached.
	g.Eq(res.Hist, proto.HistAll)
}

// Selecting [detach session] quits the menu via ActionDetach; app.Run then
// returns to the top-level menu (the session keeps running). The dedicated action
// lets a menu opened mid-session with Ctrl-\ tell "detach to the menu" apart from
// a plain esc, which resumes the session.
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

// [exit] leaves tm from anywhere via ActionExit, selected from the list. Unlike
// ActionNone (esc/Ctrl-C/Ctrl-D) it exits even from a menu opened over a session,
// where those keys resume instead.
func TestModelExitCommand(t *testing.T) {
	g := got.T(t)

	// Fuzzy-select [exit] from the list.
	m := typeStr(New(newStore(g, t), fakeCtrl{}), "exit")
	next, cmd := m.Update(keyEnterMsg)

	final := next.(Model)
	g.True(final.quit)
	g.Eq(final.Result().Action, ActionExit)
	g.NotNil(cmd)
	_, ok := cmd().(tea.QuitMsg)
	g.True(ok)
}

// Ctrl-D is the terminal EOF (VEOF), not a shortcut: on an empty filter it ends
// the menu exactly like esc — quitting from the main menu (app.Run then leaves
// tm) — but with a query typed it does nothing.
func TestModelCtrlDIsEOF(t *testing.T) {
	g := got.T(t)

	// Empty filter, top level: Ctrl-D quits with ActionNone, just like esc.
	top := send(New(newStore(g, t), fakeCtrl{}), ctrlKey('d'))
	g.True(top.quit)
	g.Eq(top.Result().Action, ActionNone)

	// A query is typed: Ctrl-D is a no-op — the menu stays and the text is intact.
	typed := typeStr(New(newStore(g, t), fakeCtrl{}), "ne")
	after := send(typed, ctrlKey('d'))
	g.True(!after.quit)
	g.Eq(after.pick.input.Value(), "ne")

	// In a sub-picker, empty Ctrl-D backs out to the main menu, like esc.
	sub := send(typeStr(New(newStore(g, t), fakeCtrl{}), "un"), keyEnterMsg)
	g.Eq(sub.pickFor, pickUseNamespace)
	sub = send(sub, ctrlKey('d'))
	g.Eq(sub.pickFor, pickMenu)
	g.True(!sub.quit)
}

// [exit]'s hint reacts to the filter: on an empty query both esc and Ctrl-D (EOF)
// end the menu, so both are shown; once a query is typed Ctrl-D is a no-op, so
// only esc remains. Opened over a session, esc/Ctrl-D resume, so no hint at all.
func TestModelExitHintReactive(t *testing.T) {
	g := got.T(t)

	// Empty top-level filter: both keys advertised.
	m := send(New(newStore(g, t), fakeCtrl{}), tea.WindowSizeMsg{Width: 80, Height: 24})
	g.Has(m.View().Content, "esc or Ctrl-D")

	// Typing hides Ctrl-D's EOF, leaving esc alone on the [exit] row.
	m = typeStr(m, "exit")
	v := m.View().Content
	g.Has(v, "[exit]")
	g.Has(v, "esc")
	g.True(!strings.Contains(v, "Ctrl-D"))

	// Deleting back to an empty filter restores both.
	m = send(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = send(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = send(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = send(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	g.Has(m.View().Content, "esc or Ctrl-D")

	// Opened over a session, esc and Ctrl-D resume, so [exit] shows no key hint.
	inSess := send(New(newStore(g, t), sessCtrl{id: "cur", name: "work"}), tea.WindowSizeMsg{Width: 80, Height: 24}).View().Content
	g.Has(inSess, "[exit]")
	g.True(!strings.Contains(inSess, "esc"))
	g.True(!strings.Contains(inSess, "Ctrl-D"))
}

// The main menu carries direct shortcuts: Ctrl-\ runs [detach session], Ctrl-T
// runs [new session] and Ctrl-G runs [use namespace], so the common commands are
// one keystroke away without moving the cursor onto them.
func TestModelCommandShortcuts(t *testing.T) {
	g := got.T(t)

	// Ctrl-\ detaches (like selecting [detach session]).
	detach := send(New(newStore(g, t), fakeCtrl{}), ctrlKey('\\'))
	g.True(detach.quit)
	g.Eq(detach.Result().Action, ActionDetach)

	// Ctrl-T opens the new-session name prompt.
	newSess := send(New(newStore(g, t), fakeCtrl{}), ctrlKey('t'))
	g.Eq(newSess.mode, modeInput)
	g.Eq(newSess.inputPurpose, inputNewSession)

	// Ctrl-G opens the namespace picker in "use" mode.
	useNs := send(New(newStore(g, t), fakeCtrl{}), ctrlKey('g'))
	g.Eq(useNs.mode, modePick)
	g.Eq(useNs.pickFor, pickUseNamespace)
}

// The shortcuts are scoped to the main menu: inside a sub-picker (here the
// namespace chooser) Ctrl-\ is not a detach — it falls through to the picker, so
// the menu stays put instead of quitting tm.
func TestModelShortcutsMainMenuOnly(t *testing.T) {
	g := got.T(t)
	m := New(newStore(g, t), fakeCtrl{})

	m = typeStr(m, "un")
	m = send(m, keyEnterMsg) // into the [use namespace] sub-picker
	g.Eq(m.pickFor, pickUseNamespace)

	m = send(m, ctrlKey('\\')) // Ctrl-\ here must not detach
	g.True(!m.quit)
	g.Eq(m.pickFor, pickUseNamespace)
}

// Each shortcut command shows its key hint beside the label on the main menu.
func TestModelShortcutHintsRendered(t *testing.T) {
	g := got.T(t)
	m := send(New(newStore(g, t), fakeCtrl{}), tea.WindowSizeMsg{Width: 80, Height: 24})

	v := m.View().Content
	g.Has(v, "Ctrl-T")  // [new session]
	g.Has(v, "Ctrl-\\") // [detach session]
	g.Has(v, "Ctrl-G")  // [use namespace]
}

// The focused row's shortcut highlights with the row: its hint renders in the
// selection style, while other rows' hints stay in the dim key style. Moving the
// cursor flips which hint is highlighted.
func TestModelShortcutHintHighlightsOnFocus(t *testing.T) {
	g := got.T(t)
	th := styles()
	m := send(New(newStore(g, t), fakeCtrl{}), tea.WindowSizeMsg{Width: 80, Height: 24})

	// The cursor starts on [new session], so its Ctrl-T is highlighted while
	// [detach session]'s Ctrl-\ stays dim.
	v := m.View().Content
	g.Has(v, th.sel.Render("Ctrl-T"))
	g.Has(v, th.key.Render(`Ctrl-\`))

	// Down to [detach session]: now its Ctrl-\ is highlighted and Ctrl-T is dim.
	v = send(m, keyDownMsg).View().Content
	g.Has(v, th.sel.Render(`Ctrl-\`))
	g.Has(v, th.key.Render("Ctrl-T"))
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

// WithNamespace (backing the TM_NAMESPACE env var) opens the menu filtered to the
// given namespace: only its sessions are listed, not the default's.
func TestModelWithNamespace(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "w1", Name: "work-sess", Namespace: "work"}))
	g.E(st.SaveSession(store.Session{ID: "d1", Name: "def-sess", Namespace: store.DefaultNamespace}))

	m := New(st, fakeCtrl{}).WithNamespace("work")
	g.Eq(m.ns, "work")

	m = send(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	v := m.View().Content
	g.Has(v, "work-sess")
	g.True(!strings.Contains(v, "def-sess"))
}

// Reopening the menu framed inside a session (Ctrl-\, via WithCurrentSession)
// adopts that session's namespace, so the header keeps showing it instead of
// reverting to default — the top-level tm process has no TM_NAMESPACE of its own.
func TestModelWithCurrentSessionAdoptsNamespace(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "w1", Name: "work-sess", Namespace: "work"}))

	m := New(st, fakeCtrl{})
	g.Eq(m.ns, store.DefaultNamespace) // a plain top-level menu starts in default

	m = m.WithCurrentSession("w1", "work-sess")
	g.Eq(m.ns, "work")

	m = send(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	g.Has(m.headerTitle(), "work") // the namespace shown in the header follows the session
}
