//go:build windows

package app

import "os"

// shellPath returns the user's command shell, falling back to cmd.exe.
func shellPath() string {
	if s := os.Getenv("COMSPEC"); s != "" {
		return s
	}

	return "cmd.exe"
}
