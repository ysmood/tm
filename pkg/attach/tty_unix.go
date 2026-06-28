//go:build !windows

package attach

import "os"

// openInput returns the relay's input source and a cleanup func. It opens
// /dev/tty directly rather than using the inherited os.Stdin: other imported
// packages (lipgloss/termenv) probe the terminal at startup and can leave the
// inherited stdin/stdout file description in a non-blocking state, which would
// break the relay's reads. A freshly opened /dev/tty is an independent,
// blocking file description immune to that. Falls back to os.Stdin if there is
// no controlling terminal (e.g. in tests driving the relay over pipes).
func openInput() (*os.File, func()) {
	if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		return f, func() { _ = f.Close() }
	}

	return os.Stdin, func() {}
}
