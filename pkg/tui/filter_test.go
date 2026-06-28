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
		items[i] = pickerItem{label: c.label, cmd: true, payload: menuPayload{isCmd: true, cmdID: c.id}}
	}

	return items
}

// Commands are found by fuzzy-matching their bracketed labels: typing a
// command's letters in order surfaces it first.
func TestPaletteFuzzyRanksCorrectly(t *testing.T) {
	g := got.T(t)
	items := paletteItems()

	check := func(query string, want cmdID) {
		g.Helper()

		order := rankItems(items, query)
		g.Desc("query %q produced no matches", query).Gt(len(order), 0)
		g.Desc("query %q ranked the wrong command first", query).Eq(items[order[0]].payload.(menuPayload).cmdID, want)
	}

	check("ds", cmdDetachSession)
	check("nn", cmdNewNamespace)
	check("un", cmdUseNamespace)
	check("dn", cmdDropNamespace)
	check("detach", cmdDetachSession)
	check("drop", cmdDropNamespace)
}

// Random in-order characters filter the palette like any other fuzzy field, with
// no per-command aliases: "[n" matches both [new session] and [new namespace].
func TestPaletteFuzzyMatchesMultiple(t *testing.T) {
	g := got.T(t)
	items := paletteItems()

	order := rankItems(items, "[n")
	matched := make([]cmdID, 0, len(order))

	for _, idx := range order {
		matched = append(matched, items[idx].payload.(menuPayload).cmdID)
	}

	g.True(slices.Contains(matched, cmdNewSession))
	g.True(slices.Contains(matched, cmdNewNamespace))
}

func TestFilterEmptyQueryKeepsOrder(t *testing.T) {
	g := got.T(t)
	order := rankItems(paletteItems(), "")
	g.Eq(order, []int{0, 1, 2, 3, 4})
}

// Sessions are found by the same fuzzy name match as everything else.
func TestFilterMatchesSessions(t *testing.T) {
	g := got.T(t)

	items := append(paletteItems(),
		pickerItem{label: "webserver", text: "webserver", payload: menuPayload{sess: store.Session{Name: "webserver"}}},
		pickerItem{label: "api", text: "api", payload: menuPayload{sess: store.Session{Name: "api"}}},
	)

	order := rankItems(items, "web")
	g.Gt(len(order), 0)
	g.Eq(items[order[0]].label, "webserver")
}
