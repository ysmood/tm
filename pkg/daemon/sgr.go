package daemon

import (
	"strconv"
	"strings"
)

// The independently settable text attributes an SGR stream controls. Each slot
// of sgrState holds the parameter that last set one of them; the order here is
// the order render emits them in.
const (
	slotBold = iota
	slotDim
	slotItalic
	slotUnderline
	slotBlink
	slotInverse
	slotHidden
	slotStrike
	slotOverline
	slotFg
	slotBg
	slotUlColor
	numSlots
)

// sgrState is the text style an SGR stream has built up: for each attribute,
// the parameter that set it ("" when it is off or default). Only the latest
// setting of each attribute is kept, so the state — and the one sequence render
// emits for it — stays a few dozen bytes however many SGR sequences a program
// sends. Recording each sequence verbatim instead grows without bound for a
// program that never sends a full reset but turns attributes off one by one
// (22 for bold/dim, 39 for the foreground, ...), as Ink-based TUIs do.
//
// Parameters the log has no use for (alternate fonts, fraktur, frames,
// ideograms, ...) are dropped.
type sgrState [numSlots]string

// sgrSets maps each plain SGR parameter that turns an attribute on to its slot;
// the slot then holds the parameter itself. (21 is double underline.)
var sgrSets = map[int]int{
	1: slotBold, 2: slotDim, 3: slotItalic, 21: slotUnderline,
	5: slotBlink, 6: slotBlink, 7: slotInverse, 8: slotHidden, 9: slotStrike,
	53: slotOverline,
}

// sgrClears maps each SGR parameter that turns attributes off to their slots.
var sgrClears = map[int][]int{
	22: {slotBold, slotDim}, 23: {slotItalic}, 24: {slotUnderline},
	25: {slotBlink}, 27: {slotInverse}, 28: {slotHidden}, 29: {slotStrike},
	55: {slotOverline}, 39: {slotFg}, 49: {slotBg}, 59: {slotUlColor},
}

// apply folds one SGR sequence's parameter list (what sits between "\x1b[" and
// "m") into the state, the way a terminal would: an empty or 0 parameter resets
// everything, and every other parameter sets or clears its own attribute.
func (s *sgrState) apply(params string) {
	fields := strings.Split(params, ";")

	for i := 0; i < len(fields); i++ {
		used, ok := s.applyParam(fields[i:])
		if !ok {
			return // malformed: the parameters after it can't be told apart
		}

		i += used
	}
}

// applyParam applies the parameter at the start of fields. It reports how many
// of the fields after it the parameter consumed too (a semicolon-form extended
// color's), and false when the parameter is malformed.
func (s *sgrState) applyParam(fields []string) (used int, ok bool) {
	f := fields[0]
	code, sub, colon := strings.Cut(f, ":") // "4:3" (curly underline), "38:2::r:g:b"

	n := 0

	if code != "" {
		var err error
		if n, err = strconv.Atoi(code); err != nil {
			return 0, true // not a number: skip it
		}
	}

	switch {
	case n == 0:
		*s = sgrState{}
	case n == 38, n == 48, n == 58:
		color, used, ok := extendedColor(fields, colon)
		if !ok {
			return 0, false
		}

		s[colorSlot(n)] = color

		return used, true
	case n == 4 && sub == "0":
		s[slotUnderline] = ""
	case n == 4:
		s[slotUnderline] = f
	default:
		if slot, ok := setSlot(n); ok {
			s[slot] = code
		}

		for _, slot := range sgrClears[n] {
			s[slot] = ""
		}
	}

	return 0, true
}

// setSlot reports the slot a plain parameter turns on: one of sgrSets, or a
// 16-color foreground (30–37, 90–97) or background (40–47, 100–107).
func setSlot(n int) (int, bool) {
	switch {
	case n >= 30 && n <= 37, n >= 90 && n <= 97:
		return slotFg, true
	case n >= 40 && n <= 47, n >= 100 && n <= 107:
		return slotBg, true
	}

	slot, ok := sgrSets[n]

	return slot, ok
}

// extendedColor reads a 256-color or truecolor parameter (38, 48 or 58) at the
// start of fields, returning the parameter text that sets it and how many fields
// past the first it consumed. The colon form ("38:5:n", "38:2::r:g:b") carries
// the whole color in its one field; the semicolon form spreads it over the next
// two ("5;n") or four ("2;r;g;b").
func extendedColor(fields []string, colon bool) (color string, used int, ok bool) {
	if colon {
		return fields[0], 0, true
	}

	if len(fields) < 2 {
		return "", 0, false
	}

	switch fields[1] {
	case "5":
		used = 2
	case "2":
		used = 4
	default:
		return "", 0, false
	}

	if len(fields) <= used {
		return "", 0, false
	}

	return strings.Join(fields[:used+1], ";"), used, true
}

// colorSlot maps an extended-color parameter to the attribute it sets.
func colorSlot(n int) int {
	switch n {
	case 38:
		return slotFg
	case 48:
		return slotBg
	default:
		return slotUlColor
	}
}

// render returns the state as one SGR sequence that sets it on a terminal in its
// default state, or nil when the state is the default (there is nothing to set).
// Each call returns a fresh slice, so one stored in written cells is never
// mutated underneath them.
func (s *sgrState) render() []byte {
	var b []byte

	for _, p := range s {
		if p == "" {
			continue
		}

		if b == nil {
			b = append(b, "\x1b["...)
		} else {
			b = append(b, ';')
		}

		b = append(b, p...)
	}

	if b == nil {
		return nil
	}

	return append(b, 'm')
}
