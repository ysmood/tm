//go:build windows

package config

// defaultRuntime is empty on Windows: sessions are addressed by named pipe
// rather than by a socket file in a directory.
func defaultRuntime() string { return "" }
