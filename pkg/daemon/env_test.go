package daemon

import (
	"slices"
	"testing"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

// sessionEnv sets TM_SESSION and TM_NAMESPACE to the session's own identity,
// replacing any values inherited from an outer session so a nested tm reports the
// session it is actually in rather than a stale inherited one.
func TestSessionEnv(t *testing.T) {
	g := got.T(t)

	// Inherited values from an outer session that must not leak through.
	t.Setenv(config.EnvSession, "outer-id")
	t.Setenv(config.EnvNamespace, "outer-ns")

	env := sessionEnv(store.Session{ID: "inner-id", Namespace: "work"})

	g.True(slices.Contains(env, config.EnvSession+"=inner-id"))
	g.True(slices.Contains(env, config.EnvNamespace+"=work"))
	g.True(!slices.Contains(env, config.EnvSession+"=outer-id"))
	g.True(!slices.Contains(env, config.EnvNamespace+"=outer-ns"))
}

// A session with no namespace (e.g. an older record) sets no TM_NAMESPACE rather
// than an empty one, and still drops any inherited value.
func TestSessionEnvNoNamespace(t *testing.T) {
	g := got.T(t)

	t.Setenv(config.EnvNamespace, "outer-ns")

	env := sessionEnv(store.Session{ID: "inner-id"})

	for _, e := range env {
		g.True(e != config.EnvNamespace+"=")
		g.True(e != config.EnvNamespace+"=outer-ns")
	}
}
