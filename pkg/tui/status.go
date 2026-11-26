package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/ysmood/tm/pkg/store"
)

// stats is the snapshot the /status screen shows. It is taken once, when the
// screen opens (see showStatus), rather than on every render: sizing sessions
// walks their directories, which a redraw has no reason to repeat.
type stats struct {
	// total and namespaces count across every namespace, and disk is everything
	// under the tm home, so the overview describes tm as a whole however the menu
	// is filtered.
	total      int
	namespaces int
	disk       int64
	home       string
	sockets    string // the socket directory; empty where there is none (Windows)

	// ns is the namespace the menu is filtered to, and rows its sessions — the
	// same ones the menu lists, so the table reads like the list it came from.
	ns   string
	rows []statRow
}

// statRow is one session in the /status table.
type statRow struct {
	name    string
	ns      string
	pid     int
	current bool
	created time.Time
	disk    int64
}

// showStatus snapshots the sessions (see stats) and opens the /status screen.
func (m *Model) showStatus() {
	m.mode = modeStatus

	all, _ := m.st.ListSessions()
	names, _ := m.st.ListNamespaces()
	p := m.st.Paths()

	s := stats{
		total:      len(all),
		namespaces: len(names),
		disk:       m.st.DiskUsage(),
		home:       p.Home,
		sockets:    p.Runtime,
		ns:         m.ns,
	}

	for _, sess := range all {
		if m.ns != store.AllNamespaces && sess.Namespace != m.ns {
			continue
		}

		s.rows = append(s.rows, statRow{
			name:    sess.Name,
			ns:      sess.Namespace,
			pid:     sess.PID,
			current: sess.ID == m.curSessionID,
			created: sess.CreatedAt,
			disk:    m.st.SessionDiskUsage(sess.ID),
		})
	}

	m.stats = s
}

// viewStatus renders the /status screen: an overview of tm as a whole, then a
// table of the sessions in the active namespace.
func (m Model) viewStatus(now time.Time) string {
	th := styles()
	s := m.stats

	var b strings.Builder

	running := strconv.Itoa(s.total) + " running"
	if s.ns != store.AllNamespaces {
		running += " · " + strconv.Itoa(len(s.rows)) + " in " + s.ns
	}

	overview := [][2]string{
		{"sessions", running},
		{"namespaces", strconv.Itoa(s.namespaces)},
		{"disk usage", humanBytes(s.disk)},
		{"home", s.home},
	}
	if s.sockets != "" {
		overview = append(overview, [2]string{"sockets", s.sockets})
	}

	writeSection(&b, "Overview", overview)

	title := "Sessions in " + s.ns
	if s.ns == store.AllNamespaces {
		title = "Sessions in all namespaces"
	}

	b.WriteString(th.session.Render(title))
	b.WriteString("\n")

	if len(s.rows) == 0 {
		b.WriteString("  " + th.dim.Render("none"))
	} else {
		b.WriteString(m.statusTable(now))
	}

	return m.box("tm — status", []string{b.String()}) + "\n" + th.dim.Render("press any key to go back")
}

// statusTable lays the session rows out in aligned columns under a dim header:
// name (and namespace, in the "*" view where names from different namespaces
// sit side by side), the daemon's pid, how long the session has been up, and
// what its directory holds on disk. The session this menu is framed inside is
// marked "current", as in the session choosers.
func (m Model) statusTable(now time.Time) string {
	th := styles()
	s := m.stats
	allNS := s.ns == store.AllNamespaces

	head := []string{"name"}
	if allNS {
		head = append(head, "namespace")
	}

	head = append(head, "pid", "up", "disk", "")

	cells := make([][]string, 0, 1+len(s.rows))
	cells = append(cells, head)

	for _, r := range s.rows {
		row := []string{r.name}
		if allNS {
			row = append(row, r.ns)
		}

		mark := ""
		if r.current {
			mark = currentHint
		}

		row = append(row, strconv.Itoa(r.pid), uptime(r.created, now), humanBytes(r.disk), mark)
		cells = append(cells, row)
	}

	// The numbers read best right-aligned, so their units line up.
	right := map[string]bool{"pid": true, "up": true, "disk": true}

	lines := columns(cells, func(col int) bool { return right[head[col]] })
	lines[0] = th.dim.Render(lines[0])

	return "  " + strings.Join(lines, "\n  ")
}

// columns pads a table's cells into aligned lines, two spaces between columns;
// alignRight picks the columns padded on the left. Trailing space is trimmed,
// so a row's empty last cell leaves nothing behind.
func columns(cells [][]string, alignRight func(col int) bool) []string {
	var widths []int

	for _, row := range cells {
		for i, c := range row {
			if i == len(widths) {
				widths = append(widths, 0)
			}

			widths[i] = max(widths[i], lipgloss.Width(c))
		}
	}

	lines := make([]string, len(cells))

	for i, row := range cells {
		var b strings.Builder

		for j, c := range row {
			if j > 0 {
				b.WriteString("  ")
			}

			pad := strings.Repeat(" ", widths[j]-lipgloss.Width(c))
			if alignRight(j) {
				b.WriteString(pad + c)
			} else {
				b.WriteString(c + pad)
			}
		}

		lines[i] = strings.TrimRight(b.String(), " ")
	}

	return lines
}

// writeSection writes a titled block of label/value rows, the layout the /help
// and /status screens share, followed by a blank line.
func writeSection(b *strings.Builder, title string, rows [][2]string) {
	th := styles()
	key := th.cmd.Width(18)

	b.WriteString(th.session.Render(title))
	b.WriteString("\n")

	for _, r := range rows {
		b.WriteString("  ")
		b.WriteString(key.Render(r[0]))
		b.WriteString(r[1])
		b.WriteString("\n")
	}

	b.WriteString("\n")
}

// humanBytes renders a byte count the way the README talks about sizes, in
// binary units: "512 B", "4.0 KiB", "1.2 MiB".
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	div, exp := int64(unit), 0
	for q := n / unit; q >= unit && exp < len("KMGTPE")-1; q /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// uptime renders how long ago created was, to the two largest units: "42s",
// "5m", "2h 3m", "4d 1h". A session with no recorded creation time (one written
// by an older tm) reads "-".
func uptime(created, now time.Time) string {
	if created.IsZero() {
		return "-"
	}

	d := max(0, now.Sub(created))

	const day = 24 * time.Hour

	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < day:
		return fmt.Sprintf("%dh %dm", d/time.Hour, d%time.Hour/time.Minute)
	default:
		return fmt.Sprintf("%dd %dh", d/day, d%day/time.Hour)
	}
}
