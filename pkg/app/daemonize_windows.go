//go:build windows

package app

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// configureDaemonSysProc detaches the daemon from the launching console so it
// survives the parent and the console closing: DETACHED_PROCESS gives it no
// console, CREATE_NEW_PROCESS_GROUP isolates it from Ctrl-C/Ctrl-Break, and
// HideWindow avoids flashing a window. The shell's own ConPTY is set up later by
// go-pty.
func configureDaemonSysProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
