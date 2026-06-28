//go:build !windows

package attach

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/ysmood/tm/pkg/proto"
)

// watchResize forwards terminal size changes to the daemon on SIGWINCH and
// returns a function that stops watching.
func watchResize(c *proto.Conn) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)

	stop := make(chan struct{})

	go func() {
		for {
			select {
			case <-ch:
				w, h := terminalSize()
				_ = c.Write(proto.MsgResize, proto.Resize{Cols: uint16(w), Rows: uint16(h)}.Encode())
			case <-stop:
				return
			}
		}
	}()

	return func() {
		signal.Stop(ch)
		close(stop)
	}
}
