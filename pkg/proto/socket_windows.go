//go:build windows

package proto

import (
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"

	"github.com/ysmood/tm/pkg/config"
)

// dialTimeout bounds how long Dial waits for the pipe to accept a connection.
const dialTimeout = 5 * time.Second

// SockAddr returns the named-pipe path that addresses a session on Windows.
func SockAddr(_ config.Paths, id string) string { return `\\.\pipe\tm-` + id }

// Listen creates a named-pipe listener at addr.
func Listen(addr string) (net.Listener, error) {
	return winio.ListenPipe(addr, nil)
}

// Dial connects to the named pipe at addr.
func Dial(addr string) (net.Conn, error) {
	timeout := dialTimeout

	return winio.DialPipe(addr, &timeout)
}
