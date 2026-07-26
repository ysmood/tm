//go:build unix

package app_test

// This file is the harness the end-to-end tests in this package are built on.
// They drive the real tm binary under a pseudo-terminal — the only place a
// terminal multiplexer's behavior is observable — so each needs the same things:
// an isolated tm installation, the built binary, a PTY whose output is captured,
// and a way to wait for what tm drew on it.
//
// Two rules keep the suite fast and parallel:
//
//   - The binary is built once for the whole package (see tmBin), not per test.
//   - Nothing mutates this process's environment. Each test gets its own TM_HOME
//     and TM_RUNTIME and hands them to the tm processes it starts through their
//     own cmd.Env (see childEnv), so tests can run concurrently — t.Setenv is
//     process-global and forbids parallel tests outright.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

const (
	// ptyCols and ptyRows size every test terminal. Bubble Tea's v2 cell renderer
	// needs a non-zero screen, and the vt model a test replays the captured stream
	// into (see vt_test.go) is built to the same size.
	ptyCols, ptyRows = 120, 40

	// testShell is the shell every test session runs: POSIX sh, so prompts and
	// `echo` behave the same on macOS and Linux.
	testShell = "/bin/sh"

	// testPrompt is the prompt those shells render (PS1). It is pinned because
	// several tests assert on prompt lines — where the cursor lands relative to
	// one, that one is on screen at all — which a developer's own PS1 (or an empty
	// one) would make untestable. It ends in "$ " like a default sh prompt.
	testPrompt = "TMPROMPT$ "

	// waitTimeout bounds how long a test waits for tm to draw something expected.
	// Generous: these are real processes on a possibly loaded machine, and the wait
	// returns as soon as the text shows up, so a high ceiling costs nothing when
	// things work.
	waitTimeout = 15 * time.Second

	// exitTimeout bounds waiting for tm to leave. Shorter than waitTimeout so a
	// regression that fails to exit fails the test instead of hanging it.
	exitTimeout = 10 * time.Second

	// pollInterval is how often a wait re-reads the captured output.
	pollInterval = 50 * time.Millisecond

	// settle is the pause after a keystroke that has nothing observable to wait
	// for — a filter character, a picker row whose text already sits on screen. Key
	// presses that do redraw something are awaited instead (see term.mustFrom), so
	// this is the exception, not the rhythm of these tests.
	settle = 200 * time.Millisecond

	// grace is how long a negative assertion allows before concluding something did
	// NOT happen — a replay that must not arrive, a reset that must not be sent.
	// There is nothing to poll for in that case, so it is a plain pause.
	grace = time.Second
)

// Keys the tests press, named so a test reads as what the user does.
const (
	keyMenu  = "\x1c" // Ctrl-\ : open the menu from a session, or leave from the top level
	keyEsc   = "\x1b" // esc: resume the session, or leave tm from the top-level menu
	keyCtrlC = "\x03" // Ctrl-C: quit
	keyEnter = "\r"
)

// Fragments of tm's own output the tests synchronize on. They are the notices and
// menu rows tm prints, so a wait for one is a wait for tm to have actually reached
// that state.
const (
	menuRow      = "[new session]"        // the top-level menu is up
	renameRow    = "[rename session]"     // ...with its full command list rendered
	sessionHint  = "session:"             // the menu is framed inside a session
	currentHint  = "current"              // a chooser row for the session tm is inside
	namePrompt   = "New session name"     // the [new session] name prompt
	renamePrompt = "Rename session:"      // the [rename session] name prompt
	enteredMsg   = "[tm entered session"  // a relay attached from the top-level menu
	switchedMsg  = "[tm switched to sess" // the relay moved to another session
	exitedSess   = "[tm exited session"   // a session's shell exited
	detachedMsg  = "[tm detached session" // [detach session] dropped to the top level
	killedMsg    = "[tm killed session"   // [kill session] ended one
	exitedMsg    = "[tm exited]"          // tm left for the launching shell
)

// tmBin builds the tm binary once per test run and returns its path. Every
// end-to-end test needs it, and a `go build` — even a fully cached one — is worth
// paying for once rather than twenty-odd times. It is lazy so tests that never
// launch tm don't pay for it at all; TestMain removes what it built.
var tmBin = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "tm-e2e-bin")
	if err != nil {
		return "", err
	}

	binDir.Store(&dir)

	bin := filepath.Join(dir, "tm")

	out, err := exec.Command("go", "build", "-o", bin, "github.com/ysmood/tm").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build tm: %w: %s", err, out)
	}

	return bin, nil
})

// binDir holds the directory tmBin built into, so TestMain can clean it up. Nil
// means no test ever needed the binary.
var binDir atomic.Pointer[string]

func TestMain(m *testing.M) {
	pinEnv()

	code := m.Run()

	if dir := binDir.Load(); dir != nil {
		_ = os.RemoveAll(*dir)
	}

	os.Exit(code)
}

// pinEnv fixes the parts of the environment every test shares, once, before any of
// them runs: a POSIX shell and a known prompt — several tests assert on prompt
// lines, which a developer's own PS1 would make untestable — and no TM_* inherited
// from the developer's shell, where a stray TM_SESSION would make every tm under
// test believe it was nested inside a session.
//
// Doing it here rather than per test is what leaves the tests parallel: only the
// per-test paths vary (see childEnv), so nothing writes to this environment again
// once the tests start. It also reaches the daemons a test spawns in-process, whose
// shells inherit it.
func pinEnv() {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "TM_") {
			_ = os.Unsetenv(name)
		}
	}

	_ = os.Setenv("SHELL", testShell)
	_ = os.Setenv("PS1", testPrompt)
}

// tmEnv is one test's private tm installation: an isolated TM_HOME holding its
// session metadata and logs, a TM_RUNTIME for its sockets, the binary, and the
// environment the tm processes it starts inherit.
type tmEnv struct {
	got.G

	paths config.Paths
	st    *store.Store
	bin   string
	env   []string
}

// setupTM prepares an isolated tm installation for an end-to-end test and runs
// the test in parallel with the others. Any session still running when the test
// ends is killed (see killLeftoverDaemons), so a detached session never outlives
// the test that made it.
func setupTM(t *testing.T) *tmEnv {
	g := got.T(t)
	g.Parallel()
	g.PanicAfter(3 * time.Minute)

	// Sockets live under /tmp: their paths have a ~104-byte OS limit, which the
	// long path t.TempDir hands a parallel subtest would blow past.
	rt, err := os.MkdirTemp("/tmp", "tmrt")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })

	paths := config.Paths{Home: t.TempDir(), Runtime: rt}
	g.E(paths.EnsureDirs())

	bin, err := tmBin()
	g.E(err)

	e := &tmEnv{G: g, paths: paths, st: store.New(paths), bin: bin, env: childEnv(paths)}
	e.killLeftoverDaemons()

	return e
}

// childEnv is the environment every tm this test starts inherits: the pinned one
// (see pinEnv), pointed at this test's own directories. That is the only per-test
// part, and it travels with the child instead of being written into this process,
// so tests can run concurrently.
func childEnv(p config.Paths) []string {
	return append(os.Environ(),
		config.EnvHome+"="+p.Home,
		config.EnvRuntime+"="+p.Runtime,
	)
}

// killLeftoverDaemons registers a cleanup that kills the daemons of any sessions
// still running when the test ends. It only ever sees this test's own sessions:
// the store it reads is the test's private TM_HOME.
func (e *tmEnv) killLeftoverDaemons() {
	e.Cleanup(func() {
		sessions, _ := e.st.ListSessions()
		for _, s := range sessions {
			if s.PID > 0 {
				_ = syscall.Kill(s.PID, syscall.SIGKILL)
			}
		}
	})
}

// spawn records a session per id (its name too) and starts each one's daemon
// through the real binary, so a test can begin from live sessions instead of
// clicking through the menu to create them. It returns the last session's daemon
// pid, which the daemon records before signalling ready.
func (e *tmEnv) spawn(ids ...string) int {
	e.Helper()

	pid := 0

	for _, id := range ids {
		sess := store.Session{
			ID: id, Name: id, Namespace: store.DefaultNamespace,
			Shell: testShell, CreatedAt: time.Unix(1, 0),
		}

		e.E(e.st.SaveSession(sess))
		e.E(app.Spawn(e.bin, e.paths, sess))

		spawned, err := e.st.GetSession(id)
		e.E(err)
		e.Gt(spawned.PID, 0)

		pid = spawned.PID
	}

	return pid
}

// term is one test terminal: a PTY, the process attached to it, and everything
// that process has written so far. Each term stands for one "the user launched
// this in their terminal", so a test that relaunches tm makes a new one.
type term struct {
	*tmEnv

	pty gopty.Pty
	cmd *gopty.Cmd
	out *safeBuilder
}

// start runs the tm binary with args on a fresh PTY and begins draining its
// output. The PTY closes when the test ends.
func (e *tmEnv) start(args ...string) *term {
	e.Helper()

	return e.startCmd(e.bin, args...)
}

// attachRelay starts the bare `tm __attach` relay for a session on its own
// terminal — no menu loop, so the menu key just ends it — and waits for the
// session's shell prompt, which the attach's replay redraws (or, if the shell has
// yet to print it, the live stream delivers). Either way its arrival means the
// relay is attached and forwarding.
func (e *tmEnv) attachRelay(id string) *term {
	e.Helper()

	tm := e.start("__attach", "--id", id)
	tm.must(testPrompt)

	return tm
}

// startShell runs tm from a shell prompt instead of directly, echoing banner
// first: the tests that check what happens to the line above the inline menu need
// a prompt on screen to check. The shell exec's tm in place, so the terminal has
// exactly one process on it, as when a user types `tm`.
func (e *tmEnv) startShell(banner string) *term {
	e.Helper()

	return e.startCmd(testShell, "-c", "echo "+banner+"; exec "+e.bin)
}

func (e *tmEnv) startCmd(name string, args ...string) *term {
	e.Helper()

	pty, err := gopty.New()
	e.E(err)
	e.E(pty.Resize(ptyCols, ptyRows))
	e.Cleanup(func() { _ = pty.Close() })

	cmd := pty.Command(name, args...)
	cmd.Env = e.env
	e.E(cmd.Start())

	out := &safeBuilder{}
	go func() { _, _ = io.Copy(out, pty) }()

	return &term{tmEnv: e, pty: pty, cmd: cmd, out: out}
}

// write types s on the terminal, with no pause: the caller waits for whatever it
// should have drawn.
func (tm *term) write(s string) {
	tm.Helper()

	_, err := tm.pty.Write([]byte(s))
	tm.E(err)
}

// send types s and pauses for the keystroke to land. It is for the keys with
// nothing observable to wait for — a filter character, a picker row whose text is
// already on screen (see settle).
func (tm *term) send(s string) {
	tm.Helper()
	tm.write(s)
	time.Sleep(settle)
}

// mark records how much output has arrived, so a later wait can require a fresh
// render past this point (see waitFor).
func (tm *term) mark() int { return len(tm.out.String()) }

// quiet waits until the terminal stops changing — no new bytes for one settle
// interval — so a test can type into a screen that has finished redrawing.
//
// Not every redraw is announced by text a test could wait for. Re-attaching a
// relay resizes the session's PTY, and a shell repaints its prompt on the SIGWINCH
// that delivers; a command typed into the middle of that repaint is echoed wherever
// the cursor momentarily is, not after the prompt. Quiescence is the only signal
// that such a repaint is done.
func (tm *term) quiet() {
	deadline := time.Now().Add(waitTimeout)

	for last := -1; ; {
		n := tm.mark()
		if n == last {
			return
		}

		last = n

		if time.Now().After(deadline) {
			return
		}

		time.Sleep(settle)
	}
}

// since returns the output produced after mark, for failure messages.
func (tm *term) since(mark int) string {
	s := tm.out.String()
	if len(s) < mark {
		return s
	}

	return s[mark:]
}

// must waits for want to appear anywhere in the terminal's output, failing the
// test — quoting everything captured — if it never does.
func (tm *term) must(want string) {
	tm.Helper()
	tm.Desc("never saw %q on the terminal; output: %q", want, tm.out.String()).
		True(waitFor(tm.out, 0, want, waitTimeout))
}

// mustFrom is must for output produced after mark, which is what a repeated
// screen needs (see waitFor).
func (tm *term) mustFrom(mark int, want string) {
	tm.Helper()
	tm.Desc("never saw a fresh %q; output since the mark: %q", want, tm.since(mark)).
		True(waitFor(tm.out, mark, want, waitTimeout))
}

// run types a command into the attached session's shell and waits for the
// session's own shell to print the marked result. The marker is computed by the
// shell ("$((6*7))" -> 42), so it can only appear if the keystrokes really
// reached a live session rather than being echoed by something else.
func (tm *term) run(marker string) {
	tm.Helper()
	tm.write("echo " + marker + "-$((6*7))" + keyEnter)
	tm.must(marker + "-42")
}

// menu opens the menu over the attached session with Ctrl-\ and waits for it to
// render, returning the mark it waited from so a caller can inspect exactly what
// opening the menu drew. The wait is mark-based: the same menu rows are already in
// the buffer from every earlier menu, so matching the whole buffer would return on
// a stale copy and the next keystroke would be typed into the session instead.
func (tm *term) menu() int {
	tm.Helper()

	mark := tm.mark()
	tm.write(keyMenu)
	tm.mustFrom(mark, sessionHint)
	time.Sleep(settle)

	return mark
}

// innerTM runs tm from inside the attached session's own shell, the way a user
// types `tm` there. Its menu is framed inside that session, and picking another
// session asks this session's daemon to hand the outer relay over rather than
// nesting a second relay. Returns the mark the menu was awaited from.
func (tm *term) innerTM() int {
	tm.Helper()

	mark := tm.mark()
	tm.write(tm.bin + keyEnter)
	tm.mustFrom(mark, menuRow)
	time.Sleep(settle)

	return mark
}

// detach leaves tm the way a user does from inside a session: Ctrl-\ opens the
// menu, [detach session] drops back to the top-level menu with the session still
// running, and esc there leaves tm for the launching shell.
func (tm *term) detach() {
	tm.Helper()
	tm.menu()
	tm.detachFromMenu()
}

// detachToMenu runs [detach session] from a menu already open over a session,
// dropping back to the top-level menu with the session still running. It returns
// the mark it waited from, so a caller can assert on what the detach printed.
func (tm *term) detachToMenu() int {
	tm.Helper()

	mark := tm.mark()
	tm.write("detach" + keyEnter) // fuzzy-filter to [detach session] and run it
	tm.mustFrom(mark, menuRow)
	time.Sleep(settle)

	return mark
}

// detachFromMenu is detachToMenu followed by esc, which leaves tm from the
// top-level menu. It is detach for when the menu is already open.
func (tm *term) detachFromMenu() {
	tm.Helper()
	tm.detachToMenu()
	tm.write(keyEsc)
}

// rename runs [rename session] from the open menu, picks the row for the session
// called name, appends suffix to the prefilled name, and waits for the notice
// quoting both sides of the change. The wait stops short of the notice's closing
// "]": a color-reset escape sits between the new name and the bracket in the raw
// stream.
func (tm *term) rename(name, suffix string) {
	tm.Helper()

	chooser := tm.mark()
	tm.write("rs" + keyEnter)  // fuzzy-filter to [rename session] and select it
	tm.mustFrom(chooser, name) // the chooser redraws the session's row

	prompt := tm.mark()
	tm.write(keyEnter) // pick it -> the name prompt, prefilled with the name
	tm.mustFrom(prompt, renamePrompt)

	tm.write(suffix + keyEnter)
	tm.mustFrom(prompt, name+" → "+name+suffix)
}

// killCurrent runs [kill session] from a menu open over the current session and
// picks the row it offers for that session — marked "current", since ending it also
// ends what is on this terminal. It returns the mark taken just before the kill, so
// the notices that follow can be required to be freshly drawn.
func (tm *term) killCurrent() int {
	tm.Helper()

	chooser := tm.mark()
	tm.write("ks" + keyEnter) // fuzzy-filter to [kill session] and select it
	tm.mustFrom(chooser, currentHint)

	mark := tm.mark()
	tm.write(keyEnter)

	return mark
}

// switchTo picks another session from a menu opened over the current one, which
// moves this terminal there in place, and waits for the switch notice.
func (tm *term) switchTo(name string) {
	tm.Helper()

	mark := tm.mark()
	tm.write(name + keyEnter)
	tm.mustFrom(mark, switchedMsg)
}

// resume presses esc in a menu opened over a session, dropping back into it.
// Deliberately unobservable — a resume replays nothing, since that screen never
// went away — so it pauses instead of waiting for output.
func (tm *term) resume() {
	tm.Helper()
	tm.write(keyEsc)
	time.Sleep(grace)
}

// resumeAfterNotice is resume for a menu that printed notices over a live session
// (a rename, a kill). Leaving such a menu is not instantaneous: the prompt guard
// moves those notice rows above the shell's prompt and puts the cursor back on it,
// rewriting the notices in their new place as it goes (see promptGuard.restorePrompt).
// Waiting for that rewrite — rather than pausing and hoping — is what makes it safe
// to type next: keystrokes sent mid-surgery land wherever the cursor happens to be.
func (tm *term) resumeAfterNotice(notice string) {
	tm.Helper()

	mark := tm.mark()
	tm.write(keyEsc)
	tm.mustFrom(mark, notice)
}

// enter picks the named session from the top-level menu and waits for the relay
// to report it attached, so the caller knows the session is up without guessing
// at a sleep.
func (tm *term) enter(name string) {
	tm.Helper()
	tm.must(name)

	mark := tm.mark()
	tm.write(name + keyEnter)
	tm.mustFrom(mark, enteredMsg)
}

// newSession runs [new session], accepts the default name, and waits for the
// relay to report the created session attached.
func (tm *term) newSession() {
	tm.Helper()
	tm.must(menuRow)
	tm.send("ns" + keyEnter) // fuzzy-filter to [new session] and select it
	tm.must(namePrompt)

	mark := tm.mark()
	tm.write(keyEnter) // accept the default name -> spawn + attach
	tm.mustFrom(mark, enteredMsg)
}

// screen replays everything the terminal has been sent through the vt model and
// returns it, so a test can assert on what the user would actually see rather
// than on the raw byte stream.
func (tm *term) screen() *vt {
	v := newVT(ptyRows, ptyCols)
	v.feed([]byte(tm.out.String()))

	return v
}

// waitScreen re-renders the screen until settled reports it looks the way the test
// expects, then returns the last model it built — whether or not it settled, so the
// caller's own assertions report what was actually on screen.
//
// Screens are drawn by another process, and the things these tests care about are
// often the *absence* of something being redrawn (a picker erased, a prompt moved),
// which no single byte announces. Polling the rendered screen waits for exactly
// that, and returns as soon as it happens instead of pausing for a fixed guess.
func (tm *term) waitScreen(settled func(*vt) bool) *vt {
	deadline := time.Now().Add(waitTimeout)

	for {
		v := tm.screen()
		if settled(v) || time.Now().After(deadline) {
			return v
		}

		time.Sleep(pollInterval)
	}
}

// waitExit waits for the process to leave, killing it if it overstays so a
// regression that fails to exit can't hang the test.
func (tm *term) waitExit() {
	tm.Helper()

	done := make(chan struct{})

	go func() { _ = tm.cmd.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(exitTimeout):
		if tm.cmd.Process != nil {
			_ = tm.cmd.Process.Kill()
		}

		tm.Desc("tm did not exit within %s; output: %q", exitTimeout, tm.out.String()).Fail()
	}
}

// waitFor polls b for want in the output past byte offset from, reporting whether
// it arrived before the timeout. Polling is how these tests synchronize: tm
// renders asynchronously in another process, so the only signal is bytes landing
// on the terminal.
//
// The from offset is what makes a repeated screen testable. A token like the
// menu's "session:" header, or a notice printed on every attach, is drawn again
// each time that screen reopens — so scanning the whole buffer matches the stale
// copy and returns instantly, and the next keystroke is sent before the new
// screen is really up, where a loaded machine drops it. Marking the length before
// the action and matching only past it waits for the fresh render instead.
func waitFor(b *safeBuilder, from int, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for {
		if s := b.String(); len(s) >= from && strings.Contains(s[from:], want) {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(pollInterval)
	}
}

// safeBuilder accumulates a terminal's output. The draining goroutine writes to
// it while the test reads, so both go through a mutex.
type safeBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *safeBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// gone polls until check reports the thing it watches has disappeared, or the
// timeout elapses. Teardown is asynchronous — a daemon deletes its session's
// record and is reaped by init on its own schedule — so its effects are polled
// for rather than assumed.
func gone(check func() bool) bool {
	deadline := time.Now().Add(waitTimeout)

	for {
		if check() {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		time.Sleep(pollInterval)
	}
}

// recordGone reports whether the session's store record disappears in time; the
// daemon deletes it during teardown.
func (e *tmEnv) recordGone(id string) bool {
	return gone(func() bool {
		_, err := e.st.GetSession(id)

		return err != nil
	})
}

// pidGone reports whether pid stops existing in time.
func pidGone(pid int) bool {
	return gone(func() bool { return syscall.Kill(pid, 0) != nil })
}

// lastNonEmptyLine is the bottom-most line with anything on it — the line the
// user's eye lands on, which for a resumed session must be its shell prompt.
func lastNonEmptyLine(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, " \n"), "\n")

	return lines[len(lines)-1]
}
