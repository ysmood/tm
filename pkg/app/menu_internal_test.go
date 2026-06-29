package app

import (
	"errors"
	"testing"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

// reapNoun pluralizes the reaped-session count for the menu's status line.
func TestReapNoun(t *testing.T) {
	g := got.T(t)
	g.Eq(reapNoun(1), "1 unreachable session")
	g.Eq(reapNoun(3), "3 unreachable sessions")
}

// With nothing to reap, a failed attach reports the relay error so the reopened
// menu explains why the session went away. (The reap-and-remove path is covered
// end to end by TestMenuReapsUnreachableSession.)
func TestAfterAttachErrorWithNothingToReap(t *testing.T) {
	g := got.T(t)
	p := config.Paths{Home: t.TempDir(), Runtime: t.TempDir()}
	g.E(p.EnsureDirs())

	ctrl := &controller{st: store.New(p)}
	g.Eq(afterAttachError(ctrl, errors.New("connection refused")), "session ended: connection refused")
}
