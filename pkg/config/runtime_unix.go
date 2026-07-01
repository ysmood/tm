//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// defaultRuntime returns a short per-user directory for transient sockets:
// $TM_RUNTIME if set, otherwise /tmp/tm-<uid>.
//
// It deliberately does NOT use $XDG_RUNTIME_DIR. That directory's lifetime is
// bound to the login session — systemd-logind destroys /run/user/<uid> when your
// last session logs out (absent enable-linger), and non-login SSH command exec
// (ssh host 'cmd') often doesn't populate it at all. A tm session daemon is
// detached and outlives the connection that spawned it, so a socket under
// XDG_RUNTIME_DIR gets stranded: the daemon keeps running while a later
// connection computes a different (or now-empty) path and can't reach it. A
// stable /tmp/tm-<uid> stays put across connect/disconnect, like tmux's
// /tmp/tmux-<uid>. The path is kept short so it stays under the OS sun_path limit.
func defaultRuntime() string {
	if d := os.Getenv(EnvRuntime); d != "" {
		return d
	}

	return filepath.Join("/tmp", "tm-"+strconv.Itoa(os.Getuid()))
}
