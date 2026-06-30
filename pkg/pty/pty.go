// Package pty is a thin cross-platform wrapper around go-pty that starts a
// command attached to a pseudo-terminal (a real PTY on Unix, ConPTY on Windows).
package pty

import (
	"os"

	gopty "github.com/aymanbagabas/go-pty"
)

// PTY is a running command attached to a pseudo-terminal.
type PTY struct {
	pty gopty.Pty
	cmd *gopty.Cmd
}

// Start opens a pseudo-terminal of the given size and starts name+args inside it.
// If env is nil the current environment is inherited; if dir is empty the current
// directory is used.
func Start(name string, args, env []string, dir string, cols, rows int) (*PTY, error) {
	p, err := gopty.New()
	if err != nil {
		return nil, err
	}

	if cols > 0 && rows > 0 {
		_ = p.Resize(cols, rows)
	}

	c := p.Command(name, args...)
	c.Env = env

	c.Dir = dir

	if err := c.Start(); err != nil {
		_ = p.Close()

		return nil, err
	}

	// On Linux a read on the PTY master only reports EOF once the LAST slave fd is
	// closed. go-pty keeps a slave fd open in this (parent) process, so without
	// closing our copy the daemon's read loop blocks forever after the child shell
	// exits — it only works on macOS, whose ptys hang up the line when the
	// controlling process exits regardless of lingering slave fds. The child holds
	// its own dup'd fd (so it is unaffected) and Resize operates on the master, so
	// closing the parent's slave here is safe and lets the master read return once
	// the shell exits. On Windows p is a ConPty, so the assertion is skipped.
	if u, ok := p.(gopty.UnixPty); ok {
		_ = u.Slave().Close()
	}

	return &PTY{pty: p, cmd: c}, nil
}

// Read reads terminal output.
func (t *PTY) Read(b []byte) (int, error) { return t.pty.Read(b) }

// Write writes terminal input.
func (t *PTY) Write(b []byte) (int, error) { return t.pty.Write(b) }

// Resize changes the terminal window size.
func (t *PTY) Resize(cols, rows int) error { return t.pty.Resize(cols, rows) }

// Wait blocks until the command exits.
func (t *PTY) Wait() error { return t.cmd.Wait() }

// Close releases the pseudo-terminal, terminating the command.
func (t *PTY) Close() error { return t.pty.Close() }

// ExitCode returns the command's exit code, or -1 if it has not exited.
func (t *PTY) ExitCode() int {
	if t.cmd.ProcessState == nil {
		return -1
	}

	return t.cmd.ProcessState.ExitCode()
}

// Signal sends sig to the underlying process (best-effort).
func (t *PTY) Signal(sig os.Signal) error {
	if t.cmd.Process == nil {
		return nil
	}

	return t.cmd.Process.Signal(sig)
}
