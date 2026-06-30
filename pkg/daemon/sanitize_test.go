package daemon

import (
	"testing"

	"github.com/ysmood/got"
)

func TestSanitizeReplay(t *testing.T) {
	g := got.T(t)

	check := func(in, want string) {
		g.Helper()
		g.Eq(string(sanitizeReplay([]byte(in))), want)
	}

	// Plain text and visual sequences are untouched.
	check("hello", "hello")
	check("\x1b[31mred\x1b[0m", "\x1b[31mred\x1b[0m") // SGR color
	check("\x1b[!p", "\x1b[!p")                       // DECSTR soft reset (no reply)

	// Display clears are dropped on replay: replaying them would erase the very
	// scrollback the replay is rebuilding. Cursor motion and cursor-relative
	// erases (which shells use to redraw the prompt) are kept.
	check("\x1b[3J", "")                    // ED3 erase scrollback
	check("\x1b[2J", "")                    // ED2 erase whole screen
	check("\x1b[3J\x1b[H\x1b[2J", "\x1b[H") // macOS `clear`: only the cursor-home survives
	check("\x1b[J", "\x1b[J")               // ED0 erase to end (cursor-relative)
	check("\x1b[0J", "\x1b[0J")             // ED0 explicit
	check("\x1b[1J", "\x1b[1J")             // ED1 erase to cursor
	check("\x1b[K", "\x1b[K")               // EL erase line
	check("\x1b[H", "\x1b[H")               // cursor home
	check("\x1b[?2J", "\x1b[?2J")           // DECSED selective erase (private) kept

	// The reported sequences — the probes a Bubble Tea v2 app emits — are dropped.
	check("\x1b[?2026$p", "")                                    // DECRQM, sync output
	check("\x1b[?2027$p", "")                                    // DECRQM, mode 2027
	check("\x1b[?u", "")                                         // Kitty keyboard query
	check("prompt$ \x1b[?2026$p\x1b[?2027$p\x1b[?u", "prompt$ ") // interleaved with text
	check("\x1b[?2026$pa\x1b[?2027$pb\x1b[?uc", "abc")           // query, text, query, …

	// Other reply-soliciting queries are dropped too.
	check("\x1b[6n", "")           // DSR cursor position
	check("\x1b[?6n", "")          // private DSR
	check("\x1b[c", "")            // primary Device Attributes
	check("\x1b[>c", "")           // secondary DA
	check("\x1b[=0c", "")          // tertiary DA
	check("\x1b]11;?\x07", "")     // OSC background-color query (BEL-terminated)
	check("\x1b]11;?\x1b\\", "")   // same, ST-terminated
	check("\x1b]4;1;?\x07", "")    // OSC palette query
	check("\x1bP+q544e\x1b\\", "") // XTGETTCAP request
	check("\x1bP$qm\x1b\\", "")    // DECRQSS request
	check("\x1b[>q", "")           // XTVERSION (CSI > q)
	check("\x1b[>0q", "")          // XTVERSION with explicit param
	check("\x1b[18t", "")          // XTWINOPS report: text-area size in chars
	check("\x1b[14t", "")          // XTWINOPS report: text-area size in pixels
	check("\x1b[14;2t", "")        // XTWINOPS report variant (first param decides)
	check("\x1b[11t", "")          // XTWINOPS report: window state

	// The exact sequences from the bug report: a prompt's startup probes left in
	// the scrollback. Replaying them made the terminal answer with ">|xterm.js(…)"
	// and ";37;152t", which got typed into the session.
	check("\x1b[18t\x1b[>q\x1b[18t", "")
	check("prompt$ \x1b[18t\x1b[>q\x1b[18t", "prompt$ ")

	// Sets and stateful sequences that look similar are kept.
	check("\x1b[>1u", "\x1b[>1u")                                             // Kitty keyboard push
	check("\x1b[<u", "\x1b[<u")                                               // Kitty keyboard pop
	check("\x1b[>4n", "\x1b[>4n")                                             // XTMODKEYS reset (not DSR)
	check("\x1b]0;title\x07", "\x1b]0;title\x07")                             // OSC window title set
	check("\x1b]11;rgb:0000/0000/0000\x07", "\x1b]11;rgb:0000/0000/0000\x07") // OSC color set
	check("\x1b[2 q", "\x1b[2 q")                                             // DECSCUSR set cursor style (not XTVERSION)
	check("\x1b[8;24;80t", "\x1b[8;24;80t")                                   // XTWINOPS resize text area (action, not a query)
	check("\x1b[3;0;0t", "\x1b[3;0;0t")                                       // XTWINOPS move window (action)
	check("\x1b[22;0t", "\x1b[22;0t")                                         // XTWINOPS push title (action)

	// A sequence truncated at the buffer's end is kept verbatim — its query-ness
	// can't be decided until the missing tail arrives, and dropping a partial
	// sequence would corrupt the stream.
	check("\x1b[?2026", "\x1b[?2026")
	check("text\x1b]11;?", "text\x1b]11;?")
}
