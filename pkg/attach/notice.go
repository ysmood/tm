package attach

// Status notices are the one-line messages tm prints on the terminal when it
// changes what the terminal is showing: switching to a session, a session's
// shell exiting, or detaching back to the launching shell. The session name is
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
	if name == "" {
		return []byte(noticeGrey + before + after + noticeReset + "\r\n")
	}

	return []byte(noticeGrey + before + noticeAccent + name + noticeGrey + after + noticeReset + "\r\n")
}

// EnteredNotice is shown when the terminal attaches to session name from the
// top-level menu (a fresh attach, as opposed to switching between sessions).
func EnteredNotice(name string) []byte { return notice("[tm entered session ", name, "]") }

// SwitchedNotice is shown after the terminal switches to session name.
func SwitchedNotice(name string) []byte { return notice("[tm switched to session ", name, "]") }

// ExitedSessionNotice is shown when session name's shell exits.
func ExitedSessionNotice(name string) []byte { return notice("[tm exited session ", name, "]") }

// DetachedNotice is shown when tm leaves for the launching shell.
func DetachedNotice() []byte { return notice("[tm detached]", "", "") }
