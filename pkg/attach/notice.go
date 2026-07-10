package attach

// Status notices are the one-line messages tm prints on the terminal when it
// changes what the terminal is showing: switching to a session, a session's
// shell exiting, detaching back to the menu, or leaving for the launching shell.
// The session name is
// drawn in the tm accent color (azure #00e6cb, matching the menu's prompt) and
// the surrounding text in grey, so the name stands out at a glance.
const (
	noticeGrey   = "\x1b[38;5;245m"       // grey framing text
	noticeAccent = "\x1b[38;2;0;230;203m" // azure #00e6cb, the tm accent
	noticeReset  = "\x1b[0m"
)

// notice renders a status line: before and after in grey, with name (if any) in
// the accent color between them. It ends with CRLF so it prints on its own line
// whether the terminal is in raw or cooked mode.
func notice(before, name, after string) []byte {
	return []byte(noticeLine(before, name, after) + "\r\n")
}

// noticeLine is notice without the trailing CRLF, for callers that own the line
// break themselves (the menu prints through Bubble Tea, which adds it).
func noticeLine(before, name, after string) string {
	if name == "" {
		return noticeGrey + before + after + noticeReset
	}

	return noticeGrey + before + noticeAccent + name + noticeGrey + after + noticeReset
}

// EnteredNotice is shown when the terminal attaches to session name from the
// top-level menu (a fresh attach, as opposed to switching between sessions).
func EnteredNotice(name string) []byte { return notice("[tm entered session ", name, "]") }

// SwitchedNotice is shown after the terminal switches to session name.
func SwitchedNotice(name string) []byte { return notice("[tm switched to session ", name, "]") }

// ExitedSessionNotice is shown when session name's shell exits.
func ExitedSessionNotice(name string) []byte { return notice("[tm exited session ", name, "]") }

// ExitedNotice is shown when tm leaves for the launching shell (the [exit]
// command, or esc/Ctrl-C at the top level).
func ExitedNotice() []byte { return notice("[tm exited]", "", "") }

// DetachedSessionNotice is shown when tm detaches from session name and drops
// back to the top-level menu (rather than leaving tm), with the session still
// running in the background.
func DetachedSessionNotice(name string) []byte { return notice("[tm detached session ", name, "]") }

// CanceledReplayNotice is shown when Ctrl-C aborts session name's history
// replay: the rest of the history is never loaded and tm drops back to the
// top-level menu, with the session still running in the background.
func CanceledReplayNotice(name string) []byte {
	return notice("[tm canceled loading session ", name, "]")
}

// RenamedSessionNotice is shown after [rename session] renames old to name. It
// names both sides of the change, since neither alone identifies what happened.
// Unlike the others it is a string, not bytes: the menu is still on screen when a
// rename lands, so it is printed through Bubble Tea (which appends the line break
// and keeps the line above the picker) rather than written to the terminal.
func RenamedSessionNotice(old, name string) string {
	return noticeLine("[tm renamed session ", old+" → "+name, "]")
}

// KilledSessionNotice is shown after [kill session] ends session name. Like
// RenamedSessionNotice it is a string: the menu is still on screen when the kill
// lands, so it is printed through Bubble Tea rather than written to the terminal.
func KilledSessionNotice(name string) string {
	return noticeLine("[tm killed session ", name, "]")
}

// KilledCurrentSessionNotice is KilledSessionNotice for when [kill session] ends
// the session the menu was opened over: the menu has already quit (the relay is
// torn down around the kill), so the notice is written straight to the terminal
// like the attach/detach notices, before the top-level menu redraws.
func KilledCurrentSessionNotice(name string) []byte {
	return notice("[tm killed session ", name, "]")
}
