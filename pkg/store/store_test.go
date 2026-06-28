package store_test

import (
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

// sliceHas reports whether v is present in list.
func sliceHas(list []string, v string) bool {
	return slices.Contains(list, v)
}
