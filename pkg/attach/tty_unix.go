//go:build !windows

package attach

import "os"

// openInput returns the relay's input source and a cleanup func. It reads the
// controlling terminal via /dev/tty directly — an independent, blocking file
// description — rather than relying on the inherited os.Stdin, which is more
// robust against anything else that may have touched the inherited descriptors.
// Falls back to os.Stdin when there is no controlling terminal (e.g. in tests
// driving the relay over pipes).
func openInput() (*os.File, func()) {
	if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		return f, func() { _ = f.Close() }
	}

	return os.Stdin, func() {}
}
