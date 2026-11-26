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
	check("un", cmdUseNamespace)
	check("dn", cmdDropNamespace)
	check("detach", cmdDetachSession)
	check("drop", cmdDropNamespace)
}

// Random in-order characters filter the palette like any other fuzzy field, with
// no per-command aliases: "session" matches both [new session] and [detach
// session].
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

// A pinned row jumps to the top only for its exact query; any other query
// leaves the fuzzy ranking alone.
func TestFilterPinOnlyOnExactQuery(t *testing.T) {
	g := got.T(t)

	plain := paletteItems()
	items := paletteItems()
	help := slices.IndexFunc(items, func(it pickerItem) bool { return it.payload.(menuPayload).cmdID == cmdHelp })
	items[help].pin = helpPin

	g.Neq(rankItems(plain, helpPin)[0], help) // fuzzy alone favors [clear history]
	g.Eq(rankItems(items, helpPin)[0], help)
	g.Eq(rankItems(items, "e"), rankItems(plain, "e")) // matches [help], but isn't its pin
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
