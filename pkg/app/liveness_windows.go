//go:build windows

package app

import "golang.org/x/sys/windows"

// stillActive is the exit code Windows reports for a process that is running.
const stillActive = 259

// processAlive reports whether a process with the given pid is still running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}

	defer func() { _ = windows.CloseHandle(h) }()

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}

	return code == stillActive
}
