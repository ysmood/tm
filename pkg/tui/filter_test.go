package tui

import (
	"testing"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/store"
)

func paletteItems() []pickerItem {
	items := make([]pickerItem, len(palette))
	for i, c := range palette {
		items[i] = pickerItem{label: c.label, aliases: c.aliases, payload: menuPayload{isCmd: true, cmdID: c.id}}
	}

	return items
}

// Each mnemonic must select its intended command first.
func TestPaletteAliasesRankCorrectly(t *testing.T) {
	g := got.T(t)
	items := paletteItems()

	check := func(query string, want cmdID) {
		g.Helper()

		order := rankItems(items, query)
		g.Desc("query %q produced no matches", query).Gt(len(order), 0)
		g.Desc("query %q ranked the wrong command first", query).Eq(items[order[0]].payload.(menuPayload).cmdID, want)
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
	order := rankItems(paletteItems(), "")
	g.Eq(order, []int{0, 1, 2, 3, 4})
}

// Sessions are found by fuzzy name match and rank below exact command mnemonics.
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
