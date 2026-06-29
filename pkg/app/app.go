// Package app wires the store, daemon, relay, and menu together and implements
// process orchestration: spawning detached session daemons and the entrypoints
// for the hidden __daemon and __attach subcommands.
package app

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/daemon"
	"github.com/ysmood/tm/pkg/store"
)

// readyTimeout bounds how long a spawner waits for a daemon to start serving.
const readyTimeout = 10 * time.Second

// Spawn launches a detached daemon for sess using the current executable, and
// returns once the daemon is serving (or an error if it fails to start).
func Spawn(p config.Paths, sess store.Session) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}

	return SpawnWith(self, p, sess)
}

// SpawnWith is Spawn with an explicit executable path (used by tests).
func SpawnWith(exe string, p config.Paths, sess store.Session) error {
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	// Clear any stale readiness marker before launching.
	_ = os.Remove(p.ReadyFile(sess.ID))

	logf, err := os.OpenFile(p.DaemonLogFile(sess.ID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	defer func() { _ = logf.Close() }()

	cmd := exec.Command(exe, "__daemon", "--id", sess.ID)

	cmd.Env = os.Environ() // inherit TM_HOME / XDG_RUNTIME_DIR so paths agree

	if dirExists(sess.Cwd) {
		cmd.Dir = sess.Cwd
	}

	cmd.Stdin = nil
	cmd.Stdout = logf
	cmd.Stderr = logf
	configureDaemonSysProc(cmd) // detach from the controlling terminal

	if err := cmd.Start(); err != nil {
		return err
	}

	pid := cmd.Process.Pid
	// Reap the child in the background so it doesn't linger as a zombie while
	// this process lives; it reparents to init if this process exits first.
	go func() { _ = cmd.Wait() }()

	if err := waitReady(p, sess.ID, pid, readyTimeout); err != nil {
		return err
	}

	return nil
}

// RunDaemon is the entrypoint for the hidden `tm __daemon --id=<id>` subcommand.
// It opens the session, starts serving, records its PID, signals readiness, and
// blocks until the shell exits.
func RunDaemon(id string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}

	sess, err := st.GetSession(id)
	if err != nil {
		return err
	}

	d, err := daemon.Start(st.Paths(), sess)
	if err != nil {
		return err
	}

	sess.PID = os.Getpid()

	_ = st.SaveSession(sess)

	wErr := os.WriteFile(st.Paths().ReadyFile(id), nil, 0o600)
	if wErr != nil {
		// Non-fatal: the spawner will time out, but the session still runs.
		fmt.Fprintln(os.Stderr, "tm daemon: write ready marker:", wErr)
	}

	return d.Wait()
}

// waitReady polls for the daemon's readiness marker, failing early if the
// process dies first or the timeout elapses.
func waitReady(p config.Paths, id string, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	ready := p.ReadyFile(id)

	for {
		if fileExists(ready) {
			return nil
		}

		if !processAlive(pid) {
			return fmt.Errorf("session %s daemon exited before it was ready (see %s)", id, p.DaemonLogFile(id))
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for session %s daemon", id)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}

	info, err := os.Stat(path)

	return err == nil && info.IsDir()
}
