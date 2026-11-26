package tui

import (
	"slices"
	"testing"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/store"
)

func paletteItems() []pickerItem {
	items := make([]pickerItem, len(palette))
	for i, c := range palette {
		items[i] = pickerItem{label: c.label, payload: menuPayload{isCmd: true, cmdID: c.id}}
	}

	return items
}

// Commands are found by fuzzy-matching their /labels: typing a command's
// letters in order surfaces it first. The queries are the ones the README
// documents for each command, with and without the leading "/".
func TestPaletteFuzzyRanksCorrectly(t *testing.T) {
	g := got.T(t)
	items := paletteItems()

	check := func(query string, want cmdID) {
		g.Helper()

		order := rankItems(items, query)
		g.Desc("query %q produced no matches", query).Gt(len(order), 0)
		g.Desc("query %q ranked the wrong command first", query).Eq(items[order[0]].payload.(menuPayload).cmdID, want)
	}

	for want, queries := range map[cmdID][]string{
		cmdNewSession:    {"new", "n", "ns", "/n", "/ns"},
		cmdRenameSession: {"rename", "rs", "/r"},
		cmdKillSession:   {"kill", "ks", "/k"},
		cmdClearHistory:  {"clear", "ch", "/c"},
		cmdHistory:       {"history", "hi", "/hi"},
		cmdDetachSession: {"detach", "ds", "/d", "/ds"},
		cmdUseNamespace:  {"namespace", "un", "/u"},
		cmdDropNamespace: {"drop", "dn", "/dn"},
		cmdStatus:        {"status", "st", "/st", "/s"},
		cmdHelp:          {"help", "h", "/h"},
	} {
		for _, q := range queries {
			check(q, want)
		}
	}
}

// Random in-order characters filter the palette like any other fuzzy field, with
// no per-command aliases: "session" matches both /new-session and /detach-session.
func TestPaletteFuzzyMatchesMultiple(t *testing.T) {
	g := got.T(t)
	items := paletteItems()

	order := rankItems(items, "session")
	matched := make([]cmdID, 0, len(order))

	for _, idx := range order {
		matched = append(matched, items[idx].payload.(menuPayload).cmdID)
	}

	g.True(slices.Contains(matched, cmdNewSession))
	g.True(slices.Contains(matched, cmdDetachSession))
}

func TestFilterEmptyQueryKeepsOrder(t *testing.T) {
	g := got.T(t)
	items := paletteItems()

	want := make([]int, len(items))
	for i := range want {
		want[i] = i
	}

	g.Eq(rankItems(items, ""), want)
}

// Sessions are found by the same fuzzy name match as everything else.
func TestFilterMatchesSessions(t *testing.T) {
	g := got.T(t)

	items := append(paletteItems(),
		pickerItem{label: sessWeb, text: sessWeb, payload: menuPayload{sess: store.Session{Name: sessWeb}}},
		pickerItem{label: sessAPI, text: sessAPI, payload: menuPayload{sess: store.Session{Name: sessAPI}}},
	)

	order := rankItems(items, "web")
	g.Gt(len(order), 0)
	g.Eq(items[order[0]].label, sessWeb)
}
