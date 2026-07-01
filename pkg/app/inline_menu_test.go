//go:build unix

package app_test

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

// TestInlineMenuClearsLikeFzf drives the real binary: while attached to a
// session, running tm shows the picker inline — without the alternate screen —
// so the session's existing output stays on screen. On a switch the picker is
// then erased (like fzf clearing its prompt) and the target session's history
// replays in its place, rather than the screen being blanked or the picker left
// stranded above the output.
func TestInlineMenuClearsLikeFzf(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(150 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tminline")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)

	p, err := config.New()
	g.E(err)
	g.E(p.EnsureDirs())
	st := store.New(p)

	for _, id := range []string{"aaa", "bbb"} {
		sess := store.Session{
			ID: id, Name: id, Namespace: store.DefaultNamespace,
			Shell: "/bin/sh", CreatedAt: time.Unix(1, 0),
		}
		g.E(st.SaveSession(sess))
		g.E(app.SpawnWith(bin, p, sess))
	}

	// Give bbb a distinctive history line, then leave it.
	bp, err := gopty.New()
	g.E(err)
	g.E(bp.Resize(120, 40))

	bc := bp.Command(bin, "__attach", "--id", "bbb")
	bc.Env = os.Environ()
	g.E(bc.Start())

	bBuf := &safeBuilder{}
	go func() { _, _ = io.Copy(bBuf, bp) }()

	time.Sleep(600 * time.Millisecond)

	_, err = bp.Write([]byte("echo BBB-HISTORY-MARK\r"))
	g.E(err)
	g.True(waitForText(bBuf, "BBB-HISTORY-MARK", 10*time.Second))

	_, err = bp.Write([]byte{0x1c})
	g.E(err)
	g.E(bc.Wait())
	_ = bp.Close()

	// Attach to aaa and print a marker we expect to stay on screen.
	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))
	g.Cleanup(func() { _ = pt.Close() })

	c := pt.Command(bin, "__attach", "--id", "aaa")
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	time.Sleep(800 * time.Millisecond)

	_, err = pt.Write([]byte("echo AAA-VISIBLE-MARK\r"))
	g.E(err)
	g.True(waitForText(buf, "AAA-VISIBLE-MARK", 10*time.Second))

	mark := len(buf.String())

	// Run tm inside aaa: the menu must appear without switching to the alt screen.
	_, err = pt.Write([]byte(bin + "\r"))
	g.E(err)
	g.True(waitForText(buf, "[new session]", 10*time.Second))

	menuOut := buf.String()[mark:]
	g.Desc("inner menu must not enter the alternate screen: %q", menuOut).
		False(strings.Contains(menuOut, "\x1b[?1049h"))

	// Pick bbb with all history -> in-place switch.
	_, err = pt.Write([]byte("bbb\r"))
	g.E(err)
	time.Sleep(400 * time.Millisecond)

	_, err = pt.Write([]byte("\r")) // "All history"
	g.E(err)
	time.Sleep(1500 * time.Millisecond)

	// Render the whole stream through a terminal model and inspect what the user
	// would actually see (visible screen plus scrollback).
	v := newVT(40, 120)
	v.feed([]byte(buf.String()))
	screen := v.visible()

	g.Desc("the pre-menu output must still be on screen: %q", screen).
		True(strings.Contains(screen, "AAA-VISIBLE-MARK"))
	g.Desc("the target session history must replay: %q", screen).
		True(strings.Contains(screen, "BBB-HISTORY-MARK"))
	g.Desc("the picker must be erased, not left on screen: %q", screen).
		False(strings.Contains(screen, "[new session]"))

	_, err = pt.Write([]byte{0x1c})
	g.E(err)
	_ = c.Wait()
}

// vt is a minimal terminal model: enough to track the main-screen scrollback and
// visible grid so a test can assert what the user can actually see.
type vt struct {
	rows, cols     int
	cur            [][]rune
	scroll         []string
	cr, cc         int
	savedR, savedC int // DECSC/DECRC saved cursor (ESC 7 / ESC 8)
}

func newVT(rows, cols int) *vt {
	v := &vt{rows: rows, cols: cols}
	v.cur = v.blank()

	return v
}

func (v *vt) blank() [][]rune {
	g := make([][]rune, v.rows)
	for i := range g {
		g[i] = make([]rune, v.cols)
		for j := range g[i] {
			g[i][j] = ' '
		}
	}

	return g
}

func (v *vt) line(r []rune) string { return strings.TrimRight(string(r), " ") }

func (v *vt) scrollUp() {
	v.scroll = append(v.scroll, v.line(v.cur[0]))
	copy(v.cur, v.cur[1:])

	last := make([]rune, v.cols)
	for j := range last {
		last[j] = ' '
	}

	v.cur[v.rows-1] = last
}

func (v *vt) newline() {
	v.cr++
	if v.cr >= v.rows {
		v.cr = v.rows - 1
		v.scrollUp()
	}
}

func (v *vt) put(ch rune) {
	if v.cc >= v.cols {
		v.cc = 0
		v.newline()
	}

	v.cur[v.cr][v.cc] = ch
	v.cc++
}

func (v *vt) feed(p []byte) {
	for i := 0; i < len(p); {
		switch b := p[i]; {
		case b == 0x1b && i+1 < len(p) && p[i+1] == '7': // DECSC: save cursor
			v.savedR, v.savedC = v.cr, v.cc
			i += 2
		case b == 0x1b && i+1 < len(p) && p[i+1] == '8': // DECRC: restore cursor
			v.cr, v.cc = v.savedR, v.savedC
			i += 2
		case b == 0x1b:
			i += v.esc(p[i:])
		case b == '\n':
			v.newline()
			i++
		case b == '\r':
			v.cc = 0
			i++
		case b == '\b':
			if v.cc > 0 {
				v.cc--
			}
			i++
		case b < 0x20:
			i++
		default:
			r, size := utf8.DecodeRune(p[i:])
			v.put(r) // each rune is one column (no wide chars in this UI)
			i += size
		}
	}
}

// esc handles one CSI sequence (cursor motion and erases are all this needs).
func (v *vt) esc(p []byte) int {
	if len(p) < 2 || p[1] != '[' {
		return 2
	}

	i := 2
	for i < len(p) && (p[i] == '?' || p[i] == '>' || p[i] == '=' || p[i] == '<') {
		i++
	}

	ps := i

	for i < len(p) && ((p[i] >= '0' && p[i] <= '9') || p[i] == ';') {
		i++
	}

	params := string(p[ps:i])

	for i < len(p) && p[i] >= 0x20 && p[i] <= 0x2f {
		i++
	}

	if i >= len(p) {
		return len(p)
	}

	final := p[i]
	i++

	v.apply(params, final)

	return i
}

func (v *vt) apply(params string, final byte) {
	n := func(idx, def int) int {
		fields := strings.Split(params, ";")
		if idx < len(fields) && fields[idx] != "" {
			x := 0
			_, _ = fmt.Sscanf(fields[idx], "%d", &x)

			if x > 0 {
				return x
			}
		}

		return def
	}

	switch final {
	case 'A':
		v.cr = max(0, v.cr-n(0, 1))
	case 'B':
		v.cr = min(v.rows-1, v.cr+n(0, 1))
	case 'H', 'f':
		v.cr, v.cc = max(0, n(0, 1)-1), max(0, n(1, 1)-1)
	case 'J':
		v.eraseDisplay(n(0, 0))
	case 'K':
		for j := v.cc; j < v.cols; j++ {
			v.cur[v.cr][j] = ' '
		}
	case 'r':
		// DECSTBM (set scroll region) homes the cursor to the top margin as a side
		// effect — the very behavior that made a bare \e[r wipe the screen when the
		// menu reopened. Model it so tests can catch a regression.
		v.cr, v.cc = max(0, n(0, 1)-1), 0
	}
}

func (v *vt) eraseDisplay(mode int) {
	switch mode {
	case 0: // cursor to end of screen
		for j := v.cc; j < v.cols; j++ {
			v.cur[v.cr][j] = ' '
		}

		for r := v.cr + 1; r < v.rows; r++ {
			for j := range v.cur[r] {
				v.cur[r][j] = ' '
			}
		}
	case 2: // whole screen
		v.cur = v.blank()
	case 3: // scrollback
		v.scroll = nil
	}
}

func (v *vt) visible() string {
	var b strings.Builder
	for _, l := range v.scroll {
		b.WriteString(l)
		b.WriteString("\n")
	}

	for _, row := range v.cur {
		b.WriteString(v.line(row))
		b.WriteString("\n")
	}

	return b.String()
}

// screen returns only the on-screen rows (no scrollback), i.e. what is actually
// visible right now — used to catch content being erased off the current screen.
func (v *vt) screen() string {
	var b strings.Builder
	for _, row := range v.cur {
		b.WriteString(v.line(row))
		b.WriteString("\n")
	}

	return b.String()
}
