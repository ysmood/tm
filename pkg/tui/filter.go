package tui

import (
	"slices"

	"github.com/sahilm/fuzzy"
)

// rankItems returns the indices of items matching query, best match first. An
// empty query keeps the original order. Every item — the fixed [bracketed]
// commands and the sessions alike — is fuzzy-matched on its text, so typing a
// few characters in any order narrows the list the same way everywhere: "ns"
// surfaces [new session], "ds" surfaces [detach session].
func rankItems(items []pickerItem, query string) []int {
	if query == "" {
		idx := make([]int, len(items))
		for i := range idx {
			idx[i] = i
		}

		return idx
	}

	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.matchText()
	}

	out := make([]int, 0, len(items))
	for _, m := range fuzzy.Find(query, texts) {
		out = append(out, m.Index)
	}

	// A row pinned to exactly this query jumps to the top, keeping the rest in
	// their ranked order.
	for i, idx := range out {
		if items[idx].pin != "" && items[idx].pin == query {
			out = slices.Insert(slices.Delete(out, i, i+1), 0, idx)

			break
		}
	}

	return out
}
