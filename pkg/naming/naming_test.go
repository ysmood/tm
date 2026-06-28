package naming_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/naming"
)

func TestGenerateFromGitRepo(t *testing.T) {
	g := got.T(t)
	root := t.TempDir()
	g.E(os.MkdirAll(filepath.Join(root, "myrepo", ".git"), 0o700))
	deep := filepath.Join(root, "myrepo", "pkg", "deep")
	g.E(os.MkdirAll(deep, 0o700))

	g.Eq(naming.Generate(deep, time.Unix(0, 0)), "myrepo")
}

func TestGenerateFromCwd(t *testing.T) {
	g := got.T(t)
	dir := filepath.Join(t.TempDir(), "project-x")
	g.E(os.MkdirAll(dir, 0o700))

	g.Eq(naming.Generate(dir, time.Unix(0, 0)), "project-x")
}

func TestGenerateFromTime(t *testing.T) {
	g := got.T(t)
	when := time.Date(2026, 6, 29, 1, 2, 3, 0, time.UTC)
	g.Eq(naming.Generate("", when), "2026-06-29-010203")
}

func TestUnique(t *testing.T) {
	g := got.T(t)
	taken := map[string]bool{"web": true, "web-2": true}

	g.Eq(naming.Unique("web", taken), "web-3")
	g.Eq(naming.Unique("api", taken), "api")
}
