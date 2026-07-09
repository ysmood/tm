//go:build unix

package app_test

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/ysmood/got"
)

// Probe: launch tm from a shell prompt and immediately quit with esc; dump the
// raw bytes so we can see what gets written to the outer terminal.
func TestProbeImmediateQuit(t *testing.T) {
	g := got.T(t)
	g.PanicAfter(60 * time.Second)

	rt, err := os.MkdirTemp("/tmp", "tmprobe")
	g.E(err)
	g.Cleanup(func() { _ = os.RemoveAll(rt) })
	g.Setenv("TM_HOME", t.TempDir())
	g.Setenv("TM_RUNTIME", rt)
	killLeftoverDaemons(g)

	bin := buildTM(g, t)

	pt, err := gopty.New()
	g.E(err)
	g.E(pt.Resize(120, 40))
	g.Cleanup(func() { _ = pt.Close() })

	c := pt.Command("/bin/sh", "-c", "echo OUTER-PROMPT; exec "+bin)
	c.Env = os.Environ()
	g.E(c.Start())

	buf := &safeBuilder{}
	go func() { _, _ = io.Copy(buf, pt) }()

	g.True(waitForText(buf, "new session", 10*time.Second))
	mark := len(buf.String())

	_, err = pt.Write([]byte("\x1b")) // esc to quit immediately
	g.E(err)
	time.Sleep(800 * time.Millisecond)

	_ = c.Wait()

	out := buf.String()[mark:]

	dump := func(needle, label string) {
		fmt.Printf("  %-28s present=%v count=%d\n", label, strings.Contains(out, needle), strings.Count(out, needle))
	}

	fmt.Println("=== bytes after esc (post-menu) ===")
	dump("\x1b[3J", "CSI 3J (erase scrollback)")
	dump("\x1b[2J", "CSI 2J (erase screen)")
	dump("\x1b[1J", "CSI 1J")
	dump("\x1b[J", "CSI J (erase to end)")
	dump("\x1b[?1049h", "enter alt screen")
	dump("\x1b[?1049l", "leave alt screen")
	dump("\x1bc", "RIS full reset")
	dump("\x1b[r", "reset scroll region")
	fmt.Printf("=== raw (quoted) ===\n%q\n", out)
}
