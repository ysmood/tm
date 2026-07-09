//go:build !windows

package daemon

import (
	"os"
	"syscall"

	"github.com/ysmood/tm/pkg/pty"
)

// killShell forcibly ends the session's shell for a kill (SIGKILL, which cannot
// be trapped), targeting its whole process group: the shell is started with
// setsid (see go-pty), so its group also covers children that never made their
// own — non-interactive wrapper scripts, background jobs — which inherit an
// ignored SIGHUP and would otherwise outlive the kill. Interactive shells put
// foreground jobs in their own groups; those get SIGHUP when the PTY closes.
// Falls back to signalling just the shell if the group is already gone.
func killShell(p *pty.PTY) {
	if pid := p.PID(); pid > 0 && syscall.Kill(-pid, syscall.SIGKILL) == nil {
		return
	}

	_ = p.Signal(os.Kill)
}
