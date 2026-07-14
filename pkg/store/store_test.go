package store_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/store"
)

func setup(t *testing.T) (got.G, *store.Store) {
	g := got.T(t)
	p := config.Paths{Home: t.TempDir()}
	g.E(p.EnsureDirs())

	return g, store.New(p)
}

func sess(id, name, ns string, created int64) store.Session {
	return store.Session{
		ID:        id,
		Name:      name,
		Namespace: ns,
		CreatedAt: time.Unix(created, 0),
	}
}

func TestSaveAndGet(t *testing.T) {
	g, st := setup(t)

	in := sess("a1", "web", store.DefaultNamespace, 1)
	in.PID = 4242
	in.Shell = "/bin/zsh"
	g.E(st.SaveSession(in))

	out, err := st.GetSession("a1")
	g.E(err)
	g.Eq(out.Name, "web")
	g.Eq(out.PID, 4242)
	g.Eq(out.Shell, "/bin/zsh")
}

func TestGetMissing(t *testing.T) {
	g, st := setup(t)
	_, err := st.GetSession("nope")
	g.Is(err, store.ErrNotFound)
}

func TestListSortedByCreation(t *testing.T) {
	g, st := setup(t)
	g.E(st.SaveSession(sess("b", "b", store.DefaultNamespace, 2)))
	g.E(st.SaveSession(sess("a", "a", store.DefaultNamespace, 1)))

	list, err := st.ListSessions()
	g.E(err)
	g.Len(list, 2)
	g.Eq(list[0].ID, "a")
	g.Eq(list[1].ID, "b")
}

func TestListEmpty(t *testing.T) {
	g, st := setup(t)
	list, err := st.ListSessions()
	g.E(err)
	g.Len(list, 0)
}

func TestNamespaceFilter(t *testing.T) {
	g, st := setup(t)
	g.E(st.SaveSession(sess("1", "x", store.DefaultNamespace, 1)))
	g.E(st.SaveSession(sess("2", "y", "work", 2)))

	def, err := st.ListByNamespace(store.DefaultNamespace)
	g.E(err)
	g.Len(def, 1)
	g.Eq(def[0].ID, "1")

	all, err := st.ListByNamespace(store.AllNamespaces)
	g.E(err)
	g.Len(all, 2)
}

func TestDeleteSession(t *testing.T) {
	g, st := setup(t)
	g.E(st.SaveSession(sess("z", "z", store.DefaultNamespace, 1)))

	g.E(st.DeleteSession("z"))
	_, err := st.GetSession("z")
	g.Is(err, store.ErrNotFound)

	// Deleting a missing session is a no-op.
	g.E(st.DeleteSession("z"))
}

// Renaming keeps the session's id — the socket, log and every derived path hang
// off it — so a running session survives being renamed.
func TestRenameSession(t *testing.T) {
	g, st := setup(t)
	g.E(st.SaveSession(sess("a1", "web", store.DefaultNamespace, 1)))

	g.E(st.RenameSession("a1", "  api  ")) // surrounding space is trimmed

	out, err := st.GetSession("a1")
	g.E(err)
	g.Eq(out.ID, "a1")
	g.Eq(out.Name, "api")
}

func TestRenameSessionRejectsEmptyName(t *testing.T) {
	g, st := setup(t)
	g.E(st.SaveSession(sess("a1", "web", store.DefaultNamespace, 1)))

	g.Err(st.RenameSession("a1", "   "))

	out, err := st.GetSession("a1")
	g.E(err)
	g.Eq(out.Name, "web")
}

func TestRenameMissingSession(t *testing.T) {
	g, st := setup(t)
	g.Is(st.RenameSession("nope", "x"), store.ErrNotFound)
}

// Names are unique within a namespace, so a name a sibling holds is rejected —
// but the same name in another namespace is free, and renaming a session to the
// name it already has is a no-op rather than a self-collision.
func TestRenameSessionNameCollision(t *testing.T) {
	g, st := setup(t)
	g.E(st.SaveSession(sess("a1", "web", store.DefaultNamespace, 1)))
	g.E(st.SaveSession(sess("a2", "api", store.DefaultNamespace, 2)))
	g.E(st.SaveSession(sess("w1", "other", "work", 3)))

	g.Err(st.RenameSession("a1", "api")) // taken by a2

	out, err := st.GetSession("a1")
	g.E(err)
	g.Eq(out.Name, "web") // left untouched

	g.E(st.RenameSession("a1", "web")) // same name: nothing to do
	g.E(st.RenameSession("w1", "api")) // another namespace: free
	g.Eq(mustGet(g, st, "w1").Name, "api")
}

func mustGet(g got.G, st *store.Store, id string) store.Session {
	s, err := st.GetSession(id)
	g.E(err)

	return s
}

func TestNamespaceCreateAndList(t *testing.T) {
	g, st := setup(t)
	g.E(st.CreateNamespace("work"))

	ns, err := st.ListNamespaces()
	g.E(err)
	g.Has(ns, store.DefaultNamespace)
	g.Has(ns, "work")
}

func TestDropNamespaceReassignsSessions(t *testing.T) {
	g, st := setup(t)
	g.E(st.CreateNamespace("work"))
	g.E(st.SaveSession(sess("s1", "s1", "work", 1)))

	g.E(st.DeleteNamespace("work"))

	s1, err := st.GetSession("s1")
	g.E(err)
	g.Eq(s1.Namespace, store.DefaultNamespace)

	ns, err := st.ListNamespaces()
	g.E(err)
	g.False(sliceHas(ns, "work"))
}

func TestCannotDropReservedNamespaces(t *testing.T) {
	g, st := setup(t)
	g.Err(st.DeleteNamespace(store.DefaultNamespace))
	g.Err(st.DeleteNamespace(store.AllNamespaces))
}

func TestPrune(t *testing.T) {
	g, st := setup(t)
	g.E(st.SaveSession(sess("live", "live", store.DefaultNamespace, 1)))
	g.E(st.SaveSession(sess("dead", "dead", store.DefaultNamespace, 2)))

	g.E(st.Prune(func(s store.Session) bool { return s.ID == "live" }))

	list, err := st.ListSessions()
	g.E(err)
	g.Len(list, 1)
	g.Eq(list[0].ID, "live")
}

// Everything persistent a session owns lives in one directory named after its
// id — its metadata, its scrollback log, its daemon's log — so a session is a
// single self-contained thing on disk, and deleting it removes the lot.
func TestSessionDirLayout(t *testing.T) {
	g := got.T(t)

	p := config.Paths{Home: t.TempDir(), Runtime: t.TempDir()}
	g.E(p.EnsureDirs())
	st := store.New(p)

	g.E(st.SaveSession(sess("s1", "one", store.DefaultNamespace, 1)))

	dir := filepath.Join(p.Home, "sessions", "s1")
	g.Eq(p.SessionDir("s1"), dir)
	g.Eq(p.SessionFile("s1"), filepath.Join(dir, "meta.json"))
	g.Eq(p.LogFile("s1"), filepath.Join(dir, "std.log"))
	g.Eq(p.DaemonLogFile("s1"), filepath.Join(dir, "daemon.log"))

	// Saving the session created the directory and wrote the metadata into it.
	g.E(os.WriteFile(p.LogFile("s1"), []byte("scrollback\n"), 0o600))
	g.E(os.WriteFile(p.DaemonLogFile("s1"), []byte("diagnostics\n"), 0o600))

	names := readDirNames(g, dir)
	g.True(sliceHas(names, "meta.json"))
	g.True(sliceHas(names, "std.log"))
	g.True(sliceHas(names, "daemon.log"))

	// Deleting the session takes the whole directory with it.
	g.E(st.DeleteSession("s1"))

	_, err := os.Stat(dir)
	g.True(os.IsNotExist(err))
}

// A directory under sessions/ with no readable metadata — one half-written by a
// concurrent create, or left behind by hand — is skipped, not listed as a broken
// session and not an error for every other reader.
func TestListSkipsDirWithoutMetadata(t *testing.T) {
	g := got.T(t)

	p := config.Paths{Home: t.TempDir(), Runtime: t.TempDir()}
	g.E(p.EnsureDirs())
	st := store.New(p)

	g.E(st.SaveSession(sess("good", "good", store.DefaultNamespace, 1)))
	g.E(os.MkdirAll(p.SessionDir("orphan"), 0o700))

	list, err := st.ListSessions()
	g.E(err)
	g.Len(list, 1)
	g.Eq(list[0].ID, "good")
}

func readDirNames(g got.G, dir string) []string {
	g.Helper()

	entries, err := os.ReadDir(dir)
	g.E(err)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// sliceHas reports whether v is present in list.
func sliceHas(list []string, v string) bool {
	return slices.Contains(list, v)
}
