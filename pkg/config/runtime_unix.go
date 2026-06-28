//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// defaultRuntime returns a short per-user directory for transient sockets:
// $XDG_RUNTIME_DIR/tm if set, otherwise /tmp/tm-<uid>. It is kept short so
// socket paths stay under the OS sun_path limit.
func defaultRuntime() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "tm")
	}

	return filepath.Join("/tmp", "tm-"+strconv.Itoa(os.Getuid()))
}
