//go:build unix

package app_test

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

// TestRenameFromSessionKeepsCursor drives the real binary: renaming a session
// from the menu opened over it (Ctrl-\) and then resuming with esc must leave
// the cursor back on the shell's prompt line — the notice printed above the
// picker must not shift where the restored cursor lands. A misplaced cursor
// makes the next command overwrite the line above the prompt.
func TestRenameFromSessionKeepsCursor(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(120 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmrc")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)
	killLeftoverDaemons(g)

	// Pin the prompt: the assertions below compare prompt prefixes, so the
	// session's shell must render a known, non-empty one. (Not g.Setenv — its
	// restore can't tell unset from empty, and an empty-but-set PS1 leaked into
	// later tests makes their shells render no prompt at all.)
	if orig, ok := os.LookupEnv("PS1"); ok {
		g.Cleanup(func() { _ = os.Setenv("PS1", orig) })
	} else {
		g.Cleanup(func() { _ = os.Unsetenv("PS1") })
	}

	g.E(os.Setenv("PS1", "TMCURSOR$ "))

	bin := buildTM(g, t)

	p, err := config.New()
	g.E(err)
	g.E(p.EnsureDirs())
	st := store.New(p)
	sess := store.Session{
		ID: "aaa", Name: "aaa", Namespace: store.DefaultNamespace,
		Shell: "/bin/sh", CreatedAt: time.Unix(1, 0),
	}
	g.E(st.SaveSession(sess))
	g.E(app.SpawnWith(bin, p, sess))

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))
	g.Cleanup(func() { _ = pt.Close() })

	c := pt.Command(bin)
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	// Enter the session (cursor starts on it) with all history.
	g.True(waitForText(buf, "aaa", 10*time.Second))

	_, err = pt.Write([]byte("\r"))
	g.E(err)
	time.Sleep(400 * time.Millisecond)

	// Mark before leaving the menu for the session: the menu's own bytes contain
	// a bare "$" (DECRQM terminal queries like CSI ?2026$p), so the prompt wait
	// below must match fresh output only — and "$ " with the trailing space,
	// which the queries lack.
	promptMark := len(buf.String())

	_, err = pt.Write([]byte("\r")) // "All history"
	g.E(err)

	// Wait for the shell's prompt — the cursor-column assertions below compare
	// prompt prefixes, so the base command must land after a rendered prompt.
	sawPrompt := waitForTextFrom(buf, promptMark, "$ ", 10*time.Second)
	g.Desc("the shell prompt must appear after entering the session; fresh output: %q",
		buf.String()[promptMark:]).True(sawPrompt)

	// A known line above the prompt, so clobbering is detectable.
	_, err = pt.Write([]byte("echo CURSOR-BASE-MARK\r"))
	g.E(err)
	g.True(waitForText(buf, "CURSOR-BASE-MARK", 10*time.Second))

	// Ctrl-\ opens the menu over the session; rename twice — aaa -> aaa-zzz ->
	// aaa-zzz-yyy — so BOTH notice rows must be relocated; esc resumes. The menu
	// wait must match fresh output only: "[rename session]" already sits in the
	// buffer from the first top-level menu, and typing into a menu that has not
	// rendered yet races its startup.
	menuMark := len(buf.String())

	_, err = pt.Write([]byte{0x1c})
	g.E(err)
	g.True(waitForTextFrom(buf, menuMark, "[rename session]", 10*time.Second))
	time.Sleep(300 * time.Millisecond)

	rename := func(suffix, want string) {
		g.Helper()

		// Match only fresh output (waitForTextFrom): the second rename's prompt
		// and chooser text already sit in the buffer from the first one.
		mark := len(buf.String())

		_, werr := pt.Write([]byte("rs\r"))
		g.E(werr)
		time.Sleep(400 * time.Millisecond)

		_, werr = pt.Write([]byte("\r"))
		g.E(werr)
		g.True(waitForTextFrom(buf, mark, "Rename session:", 10*time.Second))

		_, werr = pt.Write([]byte(suffix + "\r"))
		g.E(werr)
		g.True(waitForTextFrom(buf, mark, want, 10*time.Second))
	}

	// The waits stop short of the notice's closing "]": a color-reset escape sits
	// between the name and the bracket in the raw stream.
	rename("-zzz", "aaa → aaa-zzz")
	rename("-yyy", "aaa-zzz → aaa-zzz-yyy")

	_, err = pt.Write([]byte{0x1b}) // esc: resume the session
	g.E(err)
	time.Sleep(800 * time.Millisecond)

	// Type another command; with the cursor restored correctly it lands on the
	// prompt line, below everything already on screen.
	_, err = pt.Write([]byte("echo CURSOR-AFTER-MARK\r"))
	g.E(err)
	g.True(waitForText(buf, "CURSOR-AFTER-MARK", 10*time.Second))

	v := newVT(40, 120)
	v.feed([]byte(buf.String()))
	screen := v.visible()

	// Nothing that was on screen before the renames may be overwritten by the
	// resumed session's next command.
	g.Desc("the pre-rename output must survive: %q", screen).
		True(strings.Contains(screen, "CURSOR-BASE-MARK"))
	g.Desc("the first rename notice must survive the resumed session's output: %q", screen).
		True(strings.Contains(screen, "[tm renamed session aaa → aaa-zzz]"))
	g.Desc("the second rename notice must survive too: %q", screen).
		True(strings.Contains(screen, "[tm renamed session aaa-zzz → aaa-zzz-yyy]"))

	// The new command must land below everything, in order: base output, the two
	// notices, then the new echo — i.e. the notices sit in the scrollback above
	// the prompt, and the cursor was back on the prompt line.
	base := strings.Index(screen, "echo CURSOR-BASE-MARK")
	first := strings.Index(screen, "[tm renamed session aaa →")
	second := strings.Index(screen, "[tm renamed session aaa-zzz →")
	after := strings.Index(screen, "echo CURSOR-AFTER-MARK")
	g.Desc("expected base < first < second < after, got %d %d %d %d in %q", base, first, second, after, screen).
		True(base >= 0 && first > base && second > first && after > second)

	// The cursor must also be restored to the right COLUMN — right after the
	// prompt, not at the start of the line — so the resumed command carries the
	// same shell prompt prefix as the one typed before the menu opened.
	prefixOf := func(marker string) (string, bool) {
		for l := range strings.SplitSeq(screen, "\n") {
			if i := strings.Index(l, "echo "+marker); i >= 0 {
				return l[:i], true
			}
		}

		return "", false
	}

	basePrefix, ok := prefixOf("CURSOR-BASE-MARK")
	g.True(ok)
	g.Desc("the shell must have printed a prompt before the typed command: %q", screen).
		NotZero(len(basePrefix))

	afterPrefix, ok := prefixOf("CURSOR-AFTER-MARK")
	g.True(ok)
	g.Desc("the resumed command must start right after the prompt, not at column 0: %q", screen).
		Eq(afterPrefix, basePrefix)
}
