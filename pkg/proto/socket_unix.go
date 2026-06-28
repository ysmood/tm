//go:build !windows

package proto

import (
	"net"
	"os"

	"github.com/ysmood/tm/pkg/config"
)

// SockAddr returns the unix-domain socket path for a session id.
func SockAddr(p config.Paths, id string) string { return p.SockFile(id) }

// Listen creates a listener at addr, removing any stale socket file first.
func Listen(addr string) (net.Listener, error) {
	_ = os.Remove(addr)

	return net.Listen("unix", addr)
}

// Dial connects to the listener at addr.
func Dial(addr string) (net.Conn, error) {
	return net.Dial("unix", addr)
}
