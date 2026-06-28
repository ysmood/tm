//go:build windows

package daemon

import "os"

// defaultShell is the fallback shell when a session record has none set.
func defaultShell() string {
	if s := os.Getenv("COMSPEC"); s != "" {
		return s
	}

	return "cmd.exe"
}
