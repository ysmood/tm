//go:build unix

package app_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestStatusShowsSessions opens /status in the real binary over a live session:
// the overview counts it as running, and its row carries the pid its daemon
// recorded. Any key then returns to the menu.
func TestStatusShowsSessions(t *testing.T) {
	e := setupTM(t)
	pid := strconv.Itoa(e.spawn("alpha"))

	tm := e.start()
	tm.must("alpha")

	mark := tm.mark()
	tm.write("status" + keyEnter) // fuzzy-filter to /status and run it
	tm.mustFrom(mark, "tm — status")

	row := func(v *vt) string {
		for ln := range strings.SplitSeq(v.visible(), "\n") {
			if strings.Contains(ln, "alpha") && strings.Contains(ln, pid) {
				return ln
			}
		}

		return ""
	}

	screen := tm.waitScreen(func(v *vt) bool {
		return strings.Contains(v.visible(), "1 running · 1 in default") && row(v) != ""
	})
	e.Desc("status screen: %s", screen.visible()).Has(screen.visible(), "1 running · 1 in default")
	e.Desc("status screen: %s", screen.visible()).Has(row(screen), " B") // a disk usage column

	mark = tm.mark()
	tm.write(keyEnter) // any key goes back
	tm.mustFrom(mark, menuRow)

	tm.write(keyCtrlC)
	tm.waitExit()
}
