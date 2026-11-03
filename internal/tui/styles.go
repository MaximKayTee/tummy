package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	colAccent  = lipgloss.AdaptiveColor{Light: "#5B4BDB", Dark: "#9D8CFF"}
	colDim     = lipgloss.AdaptiveColor{Light: "#8A8A8A", Dark: "#6E6E6E"}
	colText    = lipgloss.AdaptiveColor{Light: "#1A1A1A", Dark: "#E6E6E6"}
	colOK      = lipgloss.AdaptiveColor{Light: "#1F8F4E", Dark: "#5BD58A"}
	colErr     = lipgloss.AdaptiveColor{Light: "#C23B3B", Dark: "#FF6B6B"}
	colWarn    = lipgloss.AdaptiveColor{Light: "#B7791F", Dark: "#F2C94C"}
	colSelBg   = lipgloss.AdaptiveColor{Light: "#ECE9FF", Dark: "#2D2A45"}
	colRule    = lipgloss.AdaptiveColor{Light: "#D9D9D9", Dark: "#3A3A3A"}
	colHeaderF = lipgloss.AdaptiveColor{Light: "#5C5C5C", Dark: "#A0A0A0"}

	sTitle    = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	sDim      = lipgloss.NewStyle().Foreground(colDim)
	sText     = lipgloss.NewStyle().Foreground(colText)
	sOK       = lipgloss.NewStyle().Foreground(colOK)
	sErr      = lipgloss.NewStyle().Foreground(colErr)
	sWarn     = lipgloss.NewStyle().Foreground(colWarn)
	sHeader   = lipgloss.NewStyle().Foreground(colHeaderF).Bold(true)
	sSelected = lipgloss.NewStyle().Background(colSelBg).Bold(true)
	sRule     = lipgloss.NewStyle().Foreground(colRule)
	sKey      = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	sStatus   = lipgloss.NewStyle().Foreground(colText).Italic(true)
	sBadgeOn  = lipgloss.NewStyle().Foreground(colOK)
	sBadgeOff = lipgloss.NewStyle().Foreground(colDim)
	sErrBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colErr).Padding(0, 1)
)

const (
	glyphOK     = "✓"
	glyphErr    = "✗"
	glyphNever  = "·"
	glyphQueued = "…"
	glyphSkip   = "–"
)

// column is one table column. Width 0 means fit the content within the remaining space.
type column struct {
	Title string
	Width int
	Right bool
}

// table renders a header and rows fitted to width. Cells are styled strings whose data
// went through clean.
func table(width int, cols []column, rows [][]string, selected int) []string {
	widths := fit(width, cols, rows)
	var out []string
	hdr := make([]string, len(cols))
	for i, c := range cols {
		hdr[i] = sHeader.Render(pad(c.Title, widths[i], c.Right))
	}
	out = append(out, ansi.Truncate("  "+strings.Join(hdr, "  "), width, "…"))
	for ri, row := range rows {
		cells := make([]string, len(cols))
		for i := range cols {
			v := ""
			if i < len(row) {
				v = row[i]
			}
			cells[i] = pad(v, widths[i], cols[i].Right)
		}
		line := ansi.Truncate("  "+strings.Join(cells, "  "), width, "…")
		if ri == selected {
			line = selectedLine(line, width)
		}
		out = append(out, line)
	}
	return out
}

// selectedLine marks a rendered line (starting with two spaces) as the cursor row.
func selectedLine(line string, width int) string {
	return sSelected.Render(sKey.Render("›") + pad(line[1:], max(width-1, 0), false))
}

// fit distributes width over columns. Fixed ones keep theirs; the others get what their
// content needs, narrow ones first, and the widest share what is left.
func fit(width int, cols []column, rows [][]string) []int {
	widths := make([]int, len(cols))
	used := 2 // leading marker and space
	var flex []int
	for i, c := range cols {
		if i > 0 {
			used += 2
		}
		if c.Width > 0 {
			widths[i] = c.Width
			used += c.Width
		} else {
			flex = append(flex, i)
		}
	}
	want := map[int]int{}
	for _, i := range flex {
		w := ansi.StringWidth(cols[i].Title)
		for _, r := range rows {
			if i < len(r) {
				w = max(w, ansi.StringWidth(r[i]))
			}
		}
		want[i] = w
	}
	sort.SliceStable(flex, func(a, b int) bool { return want[flex[a]] < want[flex[b]] })
	rest := max(width-used, 0)
	for n, i := range flex {
		share := rest / (len(flex) - n)
		widths[i] = max(min(want[i], share), min(want[i], 6))
		rest = max(rest-widths[i], 0)
	}
	return widths
}

func pad(s string, w int, right bool) string {
	s = ansi.Truncate(s, w, "…")
	gap := w - ansi.StringWidth(s)
	if gap <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", gap) + s
	}
	return s + strings.Repeat(" ", gap)
}

func rule(width int) string { return sRule.Render(strings.Repeat("─", max(width, 0))) }

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	case d < 48*time.Hour:
		return "yesterday"
	}
	return t.Local().Format("Jan 2")
}

func in(t *time.Time) string {
	if t == nil {
		return ""
	}
	d := time.Until(*t)
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return "in " + strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return "in " + strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return t.Local().Format("15:04")
	}
	return t.Local().Format("Jan 2 15:04")
}

func shortHome(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if h, err := userHome(); err == nil && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
