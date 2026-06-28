package tui

import (
	"testing"

	"github.com/ysmood/got"
)

func paletteItems() []listItem {
	items := make([]listItem, len(palette))
	for i, c := range palette {
		items[i] = listItem{label: c.label, isCmd: true, cmdID: c.id, aliases: c.aliases}
	}

	return items
}

// Each mnemonic must select its intended command first.
func TestPaletteAliasesRankCorrectly(t *testing.T) {
	g := got.T(t)
	items := paletteItems()

	check := func(query string, want cmdID) {
		g.Helper()

		order := filterItems(items, query)
		g.Desc("query %q produced no matches", query).Gt(len(order), 0)
		g.Desc("query %q ranked the wrong command first", query).Eq(items[order[0]].cmdID, want)
	}

	check("ns", cmdNewSession)
	check("ds", cmdDetachSession)
	check("nn", cmdNewNamespace)
	check("un", cmdUseNamespace)
	check("dn", cmdDropNamespace)
	check("detach", cmdDetachSession)
	check("drop", cmdDropNamespace)
	check("new", cmdNewSession)
}

func TestFilterEmptyQueryKeepsOrder(t *testing.T) {
	g := got.T(t)
	order := filterItems(paletteItems(), "")
	g.Eq(order, []int{0, 1, 2, 3, 4})
}

// Sessions are found by fuzzy name match and rank below exact command mnemonics.
func TestFilterMatchesSessions(t *testing.T) {
	g := got.T(t)

	items := append(paletteItems(),
		listItem{label: "webserver", name: "webserver"},
		listItem{label: "api", name: "api"},
	)

	order := filterItems(items, "web")
	g.Gt(len(order), 0)
	g.Eq(items[order[0]].label, "webserver")
}
