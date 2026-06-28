//go:build !windows

package app

import "os"

// shellPath returns the user's login shell, falling back to /bin/sh.
func shellPath() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}

	return "/bin/sh"
}
