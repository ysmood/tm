package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/store"
)

// statusStore holds three sessions: the current one and sessWeb in the default
// namespace, sessAPI in nsWork. sessWeb's history log is 3 MiB, so its disk
// usage reads the same whatever its small metadata file adds.
func statusStore(g got.G, t *testing.T) *store.Store {
	st := newStore(g, t)
	now := time.Now()

	g.E(st.SaveSession(store.Session{
		ID: curID, Name: curName, Namespace: store.DefaultNamespace, PID: 111, CreatedAt: now.Add(-90 * time.Minute),
	}))
	g.E(st.SaveSession(store.Session{
		ID: "web", Name: sessWeb, Namespace: store.DefaultNamespace, PID: 222, CreatedAt: now.Add(-time.Minute),
	}))
	g.E(st.SaveSession(store.Session{
		ID: "api", Name: sessAPI, Namespace: nsWork, PID: 333, CreatedAt: now,
	}))
	g.E(os.WriteFile(st.Paths().LogFile("web"), make([]byte, 3<<20), 0o600))

	return st
}

// wide sizes the menu so the status table — and the long temp-dir home path
// these tests store under — fits without truncation.
var wide = tea.WindowSizeMsg{Width: 160, Height: 40}

// openStatus runs /status from the main menu.
func openStatus(m Model) Model {
	return send(typeStr(m, "status"), keyEnterMsg)
}

// rowOf returns the line of view that mentions name, so a test can check what
// one session's row says.
func rowOf(g got.G, view, name string) string {
	g.Helper()

	for ln := range strings.SplitSeq(view, "\n") {
		if strings.Contains(ln, name) {
			return ln
		}
	}

	g.Desc("no row for %q in:\n%s", name, view).Fail()

	return ""
}

// /status opens an overview of tm as a whole — how many sessions run, across
// how many namespaces, and what they hold on disk — above a table of the
// sessions in the active namespace, each with its pid, uptime and disk usage.
// The session the menu sits over is marked current. Any key goes back.
func TestModelStatusScreen(t *testing.T) {
	g := got.T(t)
	st := statusStore(g, t)

	m := openStatus(send(New(st, sessCtrl{id: curID, name: curName}), wide))
	g.Eq(m.mode, modeStatus)

	v := m.View().Content
	g.Has(v, "tm — status")
	g.Has(v, "3 running · 2 in default")
	g.Has(v, humanBytes(st.DiskUsage()))
	g.Has(v, st.Paths().Home)
	g.Has(v, "Sessions in default")

	web := rowOf(g, v, sessWeb)
	g.Has(web, "222")
	g.Has(web, "1m")
	g.Has(web, "3.0 MiB")
	g.True(!strings.Contains(web, currentHint))

	cur := rowOf(g, v, curName)
	g.Has(cur, "111")
	g.Has(cur, "1h 30m")
	g.Has(cur, currentHint)

	g.True(!strings.Contains(v, sessAPI)) // another namespace's session

	m = send(m, keyEnterMsg) // any key returns to the menu
	g.Eq(m.mode, modePick)
	g.Eq(m.pickFor, pickMenu)
}

// In the "*" view the table lists every namespace's sessions, with a namespace
// column to tell same-named sessions apart.
func TestModelStatusAllNamespaces(t *testing.T) {
	g := got.T(t)

	m := openStatus(send(New(statusStore(g, t), fakeCtrl{}).WithNamespace(store.AllNamespaces), wide))

	v := m.View().Content
	g.Has(v, "Sessions in all namespaces")
	g.True(!strings.Contains(v, " in *"))
	g.Has(rowOf(g, v, "pid"), "namespace")
	g.Has(rowOf(g, v, sessAPI), nsWork)
	g.Has(rowOf(g, v, sessAPI), "333")
}

// With no sessions the table gives way to a plain "none".
func TestModelStatusEmpty(t *testing.T) {
	g := got.T(t)

	v := openStatus(New(newStore(g, t), fakeCtrl{})).View().Content
	g.Has(v, "0 running · 0 in default")
	g.Has(v, "none")
}

func TestHumanBytes(t *testing.T) {
	g := got.T(t)

	for n, want := range map[int64]string{
		0:         "0 B",
		1023:      "1023 B",
		1024:      "1.0 KiB",
		1536:      "1.5 KiB",
		64 << 20:  "64.0 MiB",
		5 << 30:   "5.0 GiB",
		1<<62 + 1: "4.0 EiB",
	} {
		g.Desc("%d bytes", n).Eq(humanBytes(n), want)
	}
}

func TestUptime(t *testing.T) {
	g := got.T(t)
	now := time.Unix(1_000_000, 0)

	for d, want := range map[time.Duration]string{
		-time.Second:                   "0s", // a clock that stepped back
		42 * time.Second:               "42s",
		5*time.Minute + time.Second:    "5m",
		2*time.Hour + 3*time.Minute:    "2h 3m",
		4*24*time.Hour + time.Hour:     "4d 1h",
		24*time.Hour - time.Nanosecond: "23h 59m",
	} {
		g.Desc("%s", d).Eq(uptime(now.Add(-d), now), want)
	}

	g.Eq(uptime(time.Time{}, now), "-")
}

// columns pads every column to its widest cell, right-aligning the ones asked
// for, and leaves no trailing space behind an empty last cell.
func TestColumns(t *testing.T) {
	g := got.T(t)

	lines := columns([][]string{
		{"name", "pid", ""},
		{"a", "12345", "current"},
		{"longer", "7", ""},
	}, func(col int) bool { return col == 1 })

	g.Eq(lines, []string{
		"name      pid",
		"a       12345  current",
		"longer      7",
	})
}
