//go:build !windows

package app

import (
	"os"
	"testing"

	"github.com/ysmood/got"
)

func TestProcessAlive(t *testing.T) {
	g := got.T(t)

	g.True(processAlive(os.Getpid()))
	g.False(processAlive(0))
	g.False(processAlive(-1))
	// A pid above the typical maximum should not exist.
	g.False(processAlive(1 << 30))
}
