//go:build windows

package attach

import (
	"time"

	"github.com/ysmood/tm/pkg/proto"
)

// resizePollInterval is how often the relay checks for a console size change on
// Windows, which has no SIGWINCH.
const resizePollInterval = 250 * time.Millisecond

// watchResize polls the terminal size and forwards changes to the daemon. It
// returns a function that stops watching.
func watchResize(c *proto.Conn) func() {
	stop := make(chan struct{})

	go func() {
		lastW, lastH := terminalSize()

		ticker := time.NewTicker(resizePollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				w, h := terminalSize()
				if w != lastW || h != lastH {
					lastW, lastH = w, h
					_ = c.Write(proto.MsgResize, proto.Resize{Cols: uint16(w), Rows: uint16(h)}.Encode())
				}
			case <-stop:
				return
			}
		}
	}()

	return func() { close(stop) }
}
