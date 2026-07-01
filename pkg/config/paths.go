// Package config resolves the on-disk locations tm uses to store its state.
package config

import (
	"os"
	"path/filepath"
)

// EnvHome overrides the default storage root when set.
const EnvHome = "TM_HOME"

// EnvSession names the session a shell belongs to. The daemon sets it (to the
// session id) in every session shell's environment, so a tm launched from inside
// a session can tell which session it is running in.
const EnvSession = "TM_SESSION"

// EnvNamespace sets the namespace tm opens in. When set, the menu starts filtered
// to this namespace and new sessions land in it, instead of the default; unset or
// empty falls back to the default namespace.
const EnvNamespace = "TM_NAMESPACE"

// EnvRuntime overrides the directory holding tm's per-session unix sockets (unix
// only). It defaults to /tmp/tm-<uid>; set it to relocate the sockets, e.g. onto a
// per-user runtime tmpfs. It must resolve to the same path for every tm that talks
// to a session — the daemon that spawns listens there and later attaches dial it —
// so pin it in a shared profile, not per-connection. tm intentionally does not fall
// back to $XDG_RUNTIME_DIR; see defaultRuntime.
const EnvRuntime = "TM_RUNTIME"

// Paths holds the resolved storage locations. Home holds persistent data
// (session metadata, logs); Runtime holds transient unix sockets and is kept
// short on purpose, since socket paths have a ~104-byte OS limit. Runtime is
// unused on Windows, which addresses sessions by named pipe instead.
type Paths struct {
	Home    string
	Runtime string
}

// New resolves Paths from the environment: Home is $TM_HOME if set, else ~/.tm;
// Runtime is the platform's per-user runtime directory.
func New() (Paths, error) {
	var home string
	if h := os.Getenv(EnvHome); h != "" {
		home = h
	} else {
		hd, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}

		home = filepath.Join(hd, ".tm")
	}

	return Paths{Home: home, Runtime: defaultRuntime()}, nil
}

// Sessions is the directory holding per-session metadata files.
func (p Paths) Sessions() string { return filepath.Join(p.Home, "sessions") }

// Namespaces is the directory holding namespace marker files.
func (p Paths) Namespaces() string { return filepath.Join(p.Home, "namespaces") }

// Logs is the directory holding per-session scrollback logs.
func (p Paths) Logs() string { return filepath.Join(p.Home, "logs") }

// Sock is the directory holding per-session unix sockets. It is Runtime when
// set, otherwise a "sock" subdirectory of Home as a fallback.
func (p Paths) Sock() string {
	if p.Runtime != "" {
		return p.Runtime
	}

	return filepath.Join(p.Home, "sock")
}

// SessionFile is the metadata file path for a session id.
func (p Paths) SessionFile(id string) string {
	return filepath.Join(p.Sessions(), id+".json")
}

// LogFile is the scrollback log path for a session id.
func (p Paths) LogFile(id string) string {
	return filepath.Join(p.Logs(), id+".log")
}

// SockFile is the unix socket path for a session id (unix only).
func (p Paths) SockFile(id string) string {
	return filepath.Join(p.Sock(), id+".sock")
}

// NamespaceFile is the marker file path for a namespace.
func (p Paths) NamespaceFile(name string) string {
	return filepath.Join(p.Namespaces(), name)
}

// ReadyFile is the marker a daemon writes once it is serving; the spawner polls
// for it as a readiness handshake.
func (p Paths) ReadyFile(id string) string {
	return filepath.Join(p.Sock(), id+".ready")
}

// DaemonLogFile captures a daemon process's own stdout/stderr for diagnostics
// (distinct from the session's scrollback log).
func (p Paths) DaemonLogFile(id string) string {
	return filepath.Join(p.Logs(), id+".daemon.log")
}

// EnsureDirs creates Home and all subdirectories.
func (p Paths) EnsureDirs() error {
	for _, d := range []string{p.Sessions(), p.Namespaces(), p.Logs(), p.Sock()} {
		err := os.MkdirAll(d, 0o700)
		if err != nil {
			return err
		}
	}

	return nil
}
