package tui

import (
	"sort"
	"strings"

	"github.com/sahilm/fuzzy"
)

// Score bands keep command mnemonics deterministic and above fuzzy session
// matches, so typing "ns" always selects [new session] rather than a session
// whose name happens to contain those letters.
const (
	scoreExact       = 1000
	scorePrefixBase  = 800
	scoreSubseq      = 400
	scoreSessionBase = 300
)

// filterItems returns the indices of items matching query, best match first.
// An empty query keeps the original order. Commands are matched against their
// alias tokens; sessions are fuzzy-matched on their name.
func filterItems(items []listItem, query string) []int {
	if query == "" {
		idx := make([]int, len(items))
		for i := range idx {
			idx[i] = i
		}

		return idx
	}

	q := strings.ToLower(query)

	type scored struct{ idx, score int }

	var matched []scored

	for i, it := range items {
		var (
			score int
			ok    bool
		)

		if it.isCmd {
			score, ok = scoreCommand(it.aliases, q)
		} else {
			score, ok = scoreSession(strings.ToLower(it.name), q)
		}

		if ok {
			matched = append(matched, scored{i, score})
		}
	}

	sort.SliceStable(matched, func(a, b int) bool { return matched[a].score > matched[b].score })

	out := make([]int, len(matched))
	for i, m := range matched {
		out[i] = m.idx
	}

	return out
}

// scoreCommand ranks q against a command's alias tokens: an exact alias beats a
// prefix (shorter alias preferred), which beats a subsequence match.
func scoreCommand(aliases []string, q string) (int, bool) {
	best := -1

	for _, a := range aliases {
		switch {
		case a == q:
			best = max(best, scoreExact)
		case strings.HasPrefix(a, q):
			best = max(best, scorePrefixBase-len(a))
		case subsequence(a, q):
			best = max(best, scoreSubseq)
		}
	}

	if best < 0 {
		return 0, false
	}

	return best, true
}

func scoreSession(name, q string) (int, bool) {
	m := fuzzy.Find(q, []string{name})
	if len(m) == 0 {
		return 0, false
	}

	return scoreSessionBase + m[0].Score, true
}

// subsequence reports whether sub appears in s in order (not necessarily
// contiguously). Inputs are expected to be lowercase ASCII mnemonics.
func subsequence(s, sub string) bool {
	i := 0
	for j := 0; i < len(sub) && j < len(s); j++ {
		if s[j] == sub[i] {
			i++
		}
	}

	return i == len(sub)
}
