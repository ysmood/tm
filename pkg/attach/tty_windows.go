//go:build windows

package attach

import "os"

// openInput returns the relay's input source. On Windows there is no /dev/tty
// equivalent, and the relay launches with CI=1 so lipgloss/termenv never probe
// the console, so the inherited os.Stdin is safe to use directly.
func openInput() (*os.File, func()) {
	return os.Stdin, func() {}
}
