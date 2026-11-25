package daemon

const (
	esc = 0x1b
	bel = 0x07
)

// scanEscape finds the end of the ESC-introduced sequence at the start of p
// (p[0] == esc), reporting its length and whether it is complete. An incomplete
// sequence — cut off at the end of a chunk — reports complete=false, so the
// caller can hold it until the rest arrives.
func scanEscape(p []byte) (n int, complete bool) {
	if len(p) < 2 {
		return 0, false
	}

	switch p[1] {
	case '[':
		return scanCSI(p)
	case ']':
		return scanString(p) // OSC
	case 'P', 'X', '^', '_':
		return scanString(p) // DCS / SOS / PM / APC — all string-terminated
	default:
		// A two- or three-byte escape (ESC 7, ESC =, ESC ( B, ...): its
		// intermediate bytes and one final byte.
		i := 1
		for i < len(p) && p[i] >= 0x20 && p[i] <= 0x2f {
			i++
		}

		if i >= len(p) {
			return 0, false
		}

		return i + 1, true
	}
}

// scanCSI finds the end of a CSI sequence (ESC [): an optional private marker,
// parameter bytes, intermediate bytes, and one final byte.
func scanCSI(p []byte) (n int, complete bool) {
	i := 2 // past ESC [

	if i < len(p) && p[i] >= 0x3c && p[i] <= 0x3f {
		i++ // private marker
	}

	for i < len(p) && p[i] >= 0x30 && p[i] <= 0x3f {
		i++ // parameter bytes
	}

	for i < len(p) && p[i] >= 0x20 && p[i] <= 0x2f {
		i++ // intermediate bytes
	}

	if i >= len(p) {
		return 0, false // final byte not yet arrived
	}

	return i + 1, true // a malformed final byte still ends it
}

// scanString finds the end of a string-terminated sequence (OSC after ESC ],
// and DCS / SOS / PM / APC after ESC P/X/^/_), which ends at BEL or ST (ESC \).
func scanString(p []byte) (n int, complete bool) {
	for i := 2; i < len(p); i++ {
		switch p[i] {
		case bel:
			return i + 1, true
		case esc:
			if i+1 >= len(p) {
				return 0, false // possibly a truncated ST
			}

			if p[i+1] == '\\' {
				return i + 2, true
			}

			return i, true // a bare ESC ends it; rescan from there
		}
	}

	return 0, false // no terminator yet
}
