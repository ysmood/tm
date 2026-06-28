//go:build !windows

package app

import (
	"os/exec"
	"syscall"
)

// configureDaemonSysProc detaches the daemon from the launching terminal by
// putting it in a new session (Setsid), so closing that terminal won't send it
// SIGHUP. The daemon has no controlling terminal of its own; only the shell it
// runs (via go-pty) gets one, on the PTY slave.
func configureDaemonSysProc(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Setsid = true
}
