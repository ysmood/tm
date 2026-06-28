package daemon

import "bytes"

const (
	esc = 0x1b
	bel = 0x07
)

// sanitizeReplay strips terminal *query* sequences from recorded output before
// it is replayed to a live terminal on attach.
//
// The scrollback is the raw PTY output, so it contains every escape sequence the
// session's programs emitted — including capability probes: DECRQM (CSI ? Ps $p),
// DSR (CSI 6n), Device Attributes (CSI c), the Kitty keyboard query (CSI ? u),
// OSC color queries (OSC 11 ; ? ST) and XTGETTCAP/DECRQSS (DCS + q / $ q). Those
// solicit a reply. Replaying them makes the attaching terminal answer, and the
// answers arrive on the relay's stdin and get forwarded into the session as if
// typed — surfacing as stray bytes like "2026;2$y2027;0$y1u" at the prompt.
//
// Everything else — text, SGR, cursor motion, OSC title/color *sets*, the soft
// reset — passes through untouched, so the replayed screen still looks right.
func sanitizeReplay(p []byte) []byte {
	out := make([]byte, 0, len(p))

	for i := 0; i < len(p); {
		if p[i] != esc {
			out = append(out, p[i])
			i++

			continue
		}

		n, drop, complete := scanSequence(p[i:])
		if !complete {
			// A sequence truncated by the end of the buffer: keep it verbatim.
			// It can't be acted on as a query until its missing tail arrives,
			// and dropping a partial sequence would corrupt the stream.
			out = append(out, p[i:]...)

			break
		}

		if !drop {
			out = append(out, p[i:i+n]...)
		}

		i += n
	}

	return out
}

// scanSequence parses one ESC-introduced sequence at the start of p (p[0]==esc),
// reporting its length, whether it is a reply-soliciting query to drop, and
// whether it is complete (an incomplete tail reports complete=false).
func scanSequence(p []byte) (n int, drop, complete bool) {
	if len(p) < 2 {
		return 0, false, false
	}

	switch p[1] {
	case '[':
		return scanCSI(p)
	case ']':
		return scanString(p, oscIsQuery)
	case 'P':
		return scanString(p, dcsIsQuery)
	default:
		// Any other escape (ESC 7, ESC M, a bare ESC, …): consume just the ESC
		// and let the next byte be re-evaluated. None are queries, and keeping
		// only one byte avoids accidentally swallowing a following CSI/OSC/DCS.
		return 1, false, true
	}
}

// scanCSI parses a CSI sequence: ESC [ , an optional private-marker byte
// (< = > ?), parameter bytes (0x30–0x3F), intermediate bytes (0x20–0x2F), and a
// final byte (0x40–0x7E).
func scanCSI(p []byte) (n int, drop, complete bool) {
	i := 2 // past ESC [

	var private byte

	if i < len(p) && p[i] >= 0x3c && p[i] <= 0x3f {
		private = p[i]
		i++
	}

	for i < len(p) && p[i] >= 0x30 && p[i] <= 0x3f {
		i++
	}

	intermediateStart := i

	for i < len(p) && p[i] >= 0x20 && p[i] <= 0x2f {
		i++
	}

	intermediates := p[intermediateStart:i]

	if i >= len(p) {
		return 0, false, false // final byte not yet arrived
	}

	final := p[i]
	i++

	if final < 0x40 || final > 0x7e {
		return i, false, true // malformed; not a query
	}

	return i, isCSIQuery(private, intermediates, final), true
}

// isCSIQuery reports whether a parsed CSI sequence solicits a reply.
func isCSIQuery(private byte, intermediates []byte, final byte) bool {
	switch final {
	case 'c': // Device Attributes: primary (CSI c), secondary (CSI > c), tertiary (CSI = c).
		return true
	case 'n': // Device Status Report, incl. cursor position (CSI 6n, CSI ?6n).
		// CSI > Pp n is XTMODKEYS reset — a set, not a query, so keep it.
		return private == 0 || private == '?'
	case 'u': // Kitty keyboard query (CSI ? u). Push/pop/set use < > = and stay.
		return private == '?'
	case 'p': // DECRQM (CSI ? Ps $ p). Keep DECSTR soft reset (CSI ! p) and DECSCUSR.
		return private == '?' && bytes.IndexByte(intermediates, '$') >= 0
	}

	return false
}

// scanString parses a string-terminated sequence (OSC after ESC ], DCS after
// ESC P), which ends at BEL or ST (ESC \). isQuery decides, from the payload
// between the introducer and the terminator, whether to drop it.
func scanString(p []byte, isQuery func([]byte) bool) (n int, drop, complete bool) {
	start := 2 // past ESC ] or ESC P

	for i := start; i < len(p); i++ {
		switch p[i] {
		case bel:
			return i + 1, isQuery(p[start:i]), true
		case esc:
			if i+1 >= len(p) {
				return 0, false, false // possibly a truncated ST
			}

			if p[i+1] == '\\' {
				return i + 2, isQuery(p[start:i]), true
			}

			// A bare ESC ends this (unterminated) string; re-scan it next.
			return i, isQuery(p[start:i]), true
		}
	}

	return 0, false, false // no terminator yet
}

// oscIsQuery reports whether an OSC payload is a value query — any of its
// semicolon-separated parameters is "?" (e.g. OSC 11 ; ? asks for the background
// color). OSC color/title *sets* carry concrete values and are kept.
func oscIsQuery(body []byte) bool {
	for field := range bytes.SplitSeq(body, []byte{';'}) {
		if len(field) == 1 && field[0] == '?' {
			return true
		}
	}

	return false
}

// dcsIsQuery reports whether a DCS payload is a request: XTGETTCAP (+q…) asks for
// terminfo capabilities, DECRQSS ($q…) asks for a setting's value. Both reply.
func dcsIsQuery(body []byte) bool {
	return bytes.HasPrefix(body, []byte("+q")) || bytes.HasPrefix(body, []byte("$q"))
}
