//go:build windows

package daemon

import (
	"os"

	"github.com/ysmood/tm/pkg/pty"
)

// killShell forcibly ends the session's shell for a kill. os.Kill terminates
// the ConPTY child outright; Windows has no Unix-style process groups to sweep.
func killShell(p *pty.PTY) { _ = p.Signal(os.Kill) }
