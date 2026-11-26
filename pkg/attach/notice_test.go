package attach

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/ysmood/got"
)

// The exit notice counts the sessions left running, in the right number.
func TestExitedNotice(t *testing.T) {
	g := got.T(t)

	for n, want := range map[int]string{
		0: "[tm exited, no sessions running]\r\n",
		1: "[tm exited, 1 session still running]\r\n",
		3: "[tm exited, 3 sessions still running]\r\n",
	} {
		g.Eq(ansi.Strip(string(ExitedNotice(n))), want)
	}
}
