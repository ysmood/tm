package tui

import (
	"errors"
	"fmt"
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
func (fakeCtrl) KillSession(string) error                      { return nil }
func (fakeCtrl) ClearHistory(string) error                     { return nil }

// killCtrl deletes the killed session from the store, standing in for the real
// controller's daemon shutdown so the menu's rebuilt list reflects the kill.
type killCtrl struct {
	fakeCtrl

	st *store.Store
}

func (c killCtrl) KillSession(id string) error { return c.st.DeleteSession(id) }

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

// [rename session] picks a session, prefills its name, and writes the new one to
// the store. The session keeps running — the menu just returns to the main list,
// which now shows the new name — and the rename is printed above the menu, where
// it stays in the scrollback.
func TestModelRenameSession(t *testing.T) {
	const id, before, after = "r1", "alpha", "alpha-2"

	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: id, Name: before, Namespace: store.DefaultNamespace}))

	m := New(st, fakeCtrl{})

	m = typeStr(m, "rs")
	m = send(m, keyEnterMsg) // [rename session] -> the session chooser
	g.Eq(m.pickFor, pickRenameSession)

	m = send(m, keyEnterMsg) // the only session
	g.Eq(m.mode, modeInput)
	g.Eq(m.inputPurpose, inputRenameSession)
	g.Eq(m.input.Value(), before) // prefilled with the current name

	m = typeStr(m, "-2")

	next, cmd := m.Update(keyEnterMsg)
	m = next.(Model)

	g.Eq(m.mode, modePick)
	g.Eq(m.pickFor, pickMenu)
	g.True(!m.quit) // renaming never attaches or leaves the menu
	g.Eq(mustGet(g, st, id).Name, after)

	// The notice names both sides of the change; tea.Println carries it into the
	// scrollback above the picker.
	g.NotNil(cmd)
	printed := printedLine(cmd)
	g.Has(printed, "renamed session")
	g.Has(printed, before)
	g.Has(printed, after)

	m = send(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	g.Has(m.View().Content, after)
}

// printedLine renders the message a tea.Println command carries, so tests can
// assert on what the menu printed above itself (the message type is unexported by
// Bubble Tea, so it is matched as text).
func printedLine(cmd tea.Cmd) string {
	return fmt.Sprintf("%v", cmd())
}

// Submitting the name a session already has is not a rename: nothing is printed.
func TestModelRenameSessionUnchangedPrintsNothing(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "u1", Name: "epsilon", Namespace: store.DefaultNamespace}))

	m := New(st, fakeCtrl{})
	m = send(typeStr(m, "rs"), keyEnterMsg)
	m = send(m, keyEnterMsg) // the only session -> the prompt, prefilled with its name
	g.Eq(m.mode, modeInput)

	next, cmd := m.Update(keyEnterMsg) // submit it unchanged
	m = next.(Model)

	g.Eq(m.pickFor, pickMenu)
	g.Nil(cmd)
}

// The rename chooser keeps the session this tm is running inside — the main menu
// hides it, but renaming the session you are in is the common case — and the
// header follows the new name.
func TestModelRenameCurrentSession(t *testing.T) {
	const id, before, after = "c1", "beta", "beta!"

	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: id, Name: before, Namespace: store.DefaultNamespace}))

	m := New(st, sessCtrl{id: id, name: before})
	g.Eq(sessionRows(m), 0) // hidden from the attach list

	m = typeStr(m, "rs")
	m = send(m, keyEnterMsg)
	g.Eq(m.pickFor, pickRenameSession)
	g.Len(m.pick.all, 1)                  // but listed here...
	g.Eq(m.pick.all[0].hint, currentHint) // ...marked as the session you are in

	m = send(m, keyEnterMsg)
	m = typeStr(m, "!")
	m = send(m, keyEnterMsg)

	g.Eq(mustGet(g, st, id).Name, after)
	g.Eq(m.curSession, after) // the header hint follows the rename
	g.Has(m.headerTitle(), after)
}

// A name another session in the namespace already holds is rejected: the prompt
// stays open with the typed text intact, the reason shown in the header, and the
// store is untouched.
func TestModelRenameSessionCollision(t *testing.T) {
	const id, keep, taken = "x1", "gamma", "delta"

	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: id, Name: keep, Namespace: store.DefaultNamespace}))
	g.E(st.SaveSession(store.Session{ID: "x2", Name: taken, Namespace: store.DefaultNamespace}))

	m := New(st, fakeCtrl{})
	m = send(typeStr(m, "rs"), keyEnterMsg)
	m = send(typeStr(m, keep), keyEnterMsg) // filter the chooser down to the session to rename

	m.input.SetValue(taken) // the name its sibling x2 holds
	m = send(m, keyEnterMsg)

	g.Eq(m.mode, modeInput) // still on the prompt, ready to be edited
	g.Eq(m.input.Value(), taken)
	g.Has(m.headerTitle(), "already in use")
	g.Eq(mustGet(g, st, id).Name, keep)

	// esc backs out to the main menu without renaming.
	m = send(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	g.Eq(m.pickFor, pickMenu)
	g.Eq(mustGet(g, st, id).Name, keep)
}

// With no sessions to rename the command says so instead of opening an empty
// chooser.
func TestModelRenameSessionNoSessions(t *testing.T) {
	g := got.T(t)
	m := New(newStore(g, t), fakeCtrl{})

	m = send(typeStr(m, "rs"), keyEnterMsg)
	g.Eq(m.pickFor, pickMenu)
	g.Has(m.headerTitle(), "no sessions to rename")
}

// [kill session] picks a session and ends it: the controller kills it, the menu
// returns to the main list — which no longer shows it — and the kill is printed
// above the menu, where it stays in the scrollback.
func TestModelKillSession(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "k1", Name: "doomed", Namespace: store.DefaultNamespace}))

	m := New(st, killCtrl{st: st})

	m = typeStr(m, "ks")
	m = send(m, keyEnterMsg) // [kill session] -> the session chooser
	g.Eq(m.pickFor, pickKillSession)

	next, cmd := m.Update(keyEnterMsg) // the only session
	m = next.(Model)

	g.Eq(m.mode, modePick)
	g.Eq(m.pickFor, pickMenu)
	g.True(!m.quit) // killing never attaches or leaves the menu

	_, err := st.GetSession("k1")
	g.Is(err, store.ErrNotFound)
	g.Eq(sessionRows(m), 0) // the rebuilt list no longer shows it

	// The notice names the killed session; tea.Println carries it into the
	// scrollback above the picker.
	g.NotNil(cmd)
	printed := printedLine(cmd)
	g.Has(printed, "killed session")
	g.Has(printed, "doomed")

	// And it is recorded, so a menu opened over a session can move it above the
	// shell's prompt where it stays visible (see Model.Notices).
	g.Len(m.Notices(), 1)
	g.Has(m.Notices()[0], "killed session")
}

// The kill chooser keeps the session this tm is running inside — marked
// "current" — and selecting it resolves to ActionKillCurrent instead of killing
// inline: ending the current session takes the screen with it, so app.Run tears
// the relay down around the kill. Other sessions still kill inline (the menu
// stays open).
func TestModelKillCurrentSession(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "cur", Name: "current", Namespace: store.DefaultNamespace}))
	g.E(st.SaveSession(store.Session{ID: "oth", Name: "other", Namespace: store.DefaultNamespace}))

	m := New(st, sessCtrl{id: "cur", name: "current"})
	m = send(typeStr(m, "ks"), keyEnterMsg)

	g.Eq(m.pickFor, pickKillSession)
	g.Len(m.pick.all, 2) // both listed, the current one marked

	var curHint string

	for _, it := range m.pick.all {
		if p, ok := it.payload.(killPayload); ok && p.id == "cur" {
			curHint = it.hint
		}
	}

	g.Eq(curHint, "current")

	// Filter down to the current session and select it: the menu quits with
	// ActionKillCurrent for app.Run to carry out — nothing is killed inline.
	m = typeStr(m, "current")

	next, cmd := m.Update(keyEnterMsg)
	final := next.(Model)

	g.True(final.quit)
	g.Eq(final.Result().Action, ActionKillCurrent)
	g.Eq(final.Result().ID, "cur")
	g.NotNil(cmd)

	_, isQuit := cmd().(tea.QuitMsg)
	g.True(isQuit)

	// The store is untouched: the kill happens after the relay teardown.
	_, err := st.GetSession("cur")
	g.E(err)
}

// With no sessions in the namespace the command says so instead of opening an
// empty chooser; the current session alone is enough to open it (killing the
// session you are inside is supported).
func TestModelKillSessionNoSessions(t *testing.T) {
	g := got.T(t)

	m := send(typeStr(New(newStore(g, t), fakeCtrl{}), "ks"), keyEnterMsg)
	g.Eq(m.pickFor, pickMenu)
	g.Has(m.headerTitle(), "no sessions to kill")

	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "cur", Name: "current", Namespace: store.DefaultNamespace}))

	in := send(typeStr(New(st, sessCtrl{id: "cur", name: "current"}), "ks"), keyEnterMsg)
	g.Eq(in.pickFor, pickKillSession)
	g.Len(in.pick.all, 1)
}

// failKillCtrl simulates a kill the controller could not carry out.
type failKillCtrl struct{ fakeCtrl }

func (failKillCtrl) KillSession(string) error { return errors.New("boom") }

// A failed kill returns to the main menu with the reason in the header; the
// session, still running, stays in the list.
func TestModelKillSessionError(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "k1", Name: "sturdy", Namespace: store.DefaultNamespace}))

	m := New(st, failKillCtrl{})
	m = send(typeStr(m, "ks"), keyEnterMsg)

	next, cmd := m.Update(keyEnterMsg) // the only session
	m = next.(Model)

	g.Nil(cmd) // nothing printed: nothing happened
	g.Eq(m.pickFor, pickMenu)
	g.Has(m.headerTitle(), "failed to kill session: boom")
	g.Eq(sessionRows(m), 1)
}

// clearCtrl records which session ClearHistory was asked to wipe.
type clearCtrl struct {
	fakeCtrl

	cleared *[]string
}

func (c clearCtrl) ClearHistory(id string) error {
	*c.cleared = append(*c.cleared, id)

	return nil
}

// sessClearCtrl is clearCtrl for a tm framed inside a session: it reports the
// current session and records what ClearHistory was asked to wipe.
type sessClearCtrl struct {
	sessCtrl

	cleared *[]string
}

func (c sessClearCtrl) ClearHistory(id string) error {
	*c.cleared = append(*c.cleared, id)

	return nil
}

// [clear history] picks a session and wipes its recorded scrollback: the
// controller clears it, the session stays running (and listed), and the wipe is
// printed above the menu, where it stays in the scrollback.
func TestModelClearHistory(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "h1", Name: "leaky", Namespace: store.DefaultNamespace}))

	var cleared []string

	m := New(st, clearCtrl{cleared: &cleared})

	m = typeStr(m, "ch")
	m = send(m, keyEnterMsg) // [clear history] -> the session chooser
	g.Eq(m.pickFor, pickClearHistory)

	next, cmd := m.Update(keyEnterMsg) // the only session
	m = next.(Model)

	g.Eq(m.mode, modePick)
	g.Eq(m.pickFor, pickMenu)
	g.True(!m.quit) // clearing never attaches or leaves the menu
	g.Eq(cleared, []string{"h1"})

	// The session survives the clear and stays in the list.
	_, err := st.GetSession("h1")
	g.E(err)
	g.Eq(sessionRows(m), 1)

	// The notice names the cleared session; tea.Println carries it into the
	// scrollback above the picker.
	g.NotNil(cmd)
	printed := printedLine(cmd)
	g.Has(printed, "cleared history of session")
	g.Has(printed, "leaky")

	// And it is recorded, so a menu opened over a session can move it above the
	// shell's prompt where it stays visible (see Model.Notices).
	g.Len(m.Notices(), 1)
	g.Has(m.Notices()[0], "cleared history of session")
}

// The clear chooser keeps the session this tm is running inside — marked
// "current" — and clearing it works inline like any other session: unlike a
// kill, a clear disturbs nothing on screen, so the menu just stays open.
func TestModelClearHistoryCurrentSession(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "cur", Name: "current", Namespace: store.DefaultNamespace}))

	var cleared []string

	m := New(st, sessClearCtrl{sessCtrl: sessCtrl{id: "cur", name: "current"}, cleared: &cleared})
	m = send(typeStr(m, "ch"), keyEnterMsg)

	g.Eq(m.pickFor, pickClearHistory)
	g.Len(m.pick.all, 1) // the current session is listed, marked

	var curHint string

	for _, it := range m.pick.all {
		if p, ok := it.payload.(clearPayload); ok && p.id == "cur" {
			curHint = it.hint
		}
	}

	g.Eq(curHint, currentHint)

	next, cmd := m.Update(keyEnterMsg)
	m = next.(Model)

	g.True(!m.quit) // handled inline: nothing is torn down
	g.Eq(m.pickFor, pickMenu)
	g.Eq(cleared, []string{"cur"})
	g.NotNil(cmd)
	g.Has(printedLine(cmd), "cleared history of session")
}

// failClearCtrl simulates a clear the controller could not carry out.
type failClearCtrl struct{ fakeCtrl }

func (failClearCtrl) ClearHistory(string) error { return errors.New("boom") }

// A failed clear returns to the main menu with the reason in the header.
func TestModelClearHistoryError(t *testing.T) {
	g := got.T(t)
	st := newStore(g, t)
	g.E(st.SaveSession(store.Session{ID: "h1", Name: "sturdy", Namespace: store.DefaultNamespace}))

	m := New(st, failClearCtrl{})
	m = send(typeStr(m, "ch"), keyEnterMsg)

	next, cmd := m.Update(keyEnterMsg) // the only session
	m = next.(Model)

	g.Nil(cmd) // nothing printed: nothing happened
	g.Eq(m.pickFor, pickMenu)
	g.Has(m.headerTitle(), "failed to clear history: boom")
}

// With no sessions in the namespace the command says so instead of opening an
// empty chooser.
func TestModelClearHistoryNoSessions(t *testing.T) {
	g := got.T(t)

	m := send(typeStr(New(newStore(g, t), fakeCtrl{}), "ch"), keyEnterMsg)
	g.Eq(m.pickFor, pickMenu)
	g.Has(m.headerTitle(), "no sessions to clear")
}

func mustGet(g got.G, st *store.Store, id string) store.Session {
	s, err := st.GetSession(id)
	g.E(err)

	return s
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

	// Down past [rename session], [kill session] and [clear history] onto
	// [detach session]: now its Ctrl-\ is highlighted and Ctrl-T is dim.
	v = send(send(send(send(m, keyDownMsg), keyDownMsg), keyDownMsg), keyDownMsg).View().Content
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
