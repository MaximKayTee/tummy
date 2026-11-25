package tui

import (
	_ "embed"
	"image/color"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	txt "github.com/MaximKayTee/gotuitools/text"
	"github.com/MaximKayTee/gotuitools/theme"
)

//go:embed themes/tummy.toml
var themeDark []byte

//go:embed themes/tummy-light.toml
var themeLight []byte

func init() {
	theme.MustRegister("tummy", themeDark)
	theme.MustRegister("tummy-light", themeLight)
	th, _ := theme.Find("tummy")
	applyTheme(th, true)
}

var (
	colErr, colWarn color.Color

	sTitle, sDim, sText, sOK, sErr, sWarn, sHeader, sSelected, sRule lipgloss.Style
	sKey, sStatus, sBadgeOn, sBadgeOff, sErrBox                      lipgloss.Style

	// curTheme is the theme the styles come from; darkBG is the terminal background.
	curTheme theme.Theme
	darkBG   bool
)

// applyTheme sets the styles from th. Scopes th lacks fall back to related ones.
func applyTheme(th theme.Theme, dark bool) {
	curTheme, darkBG = th, dark
	style := func(scopes ...string) lipgloss.Style {
		for _, sc := range scopes {
			if st := th.Style(sc); st != (theme.Style{}) {
				return st.Lip()
			}
		}
		return lipgloss.NewStyle()
	}
	sTitle = style("ui.text.focus")
	sDim = style("ui.text.inactive")
	sText = style("ui.text")
	sOK = style("tummy.ok", "markup.list.checked", "diff.plus")
	sErr = style("error")
	sWarn = style("warning")
	sHeader = style("tummy.header", "markup.table.header", "ui.text.inactive")
	sSelected = style("ui.selection")
	sRule = style("ui.window")
	sKey = style("ui.statusline.key", "ui.text.focus")
	sStatus = style("tummy.status", "ui.text")
	sBadgeOn = sOK
	sBadgeOff = sDim
	colErr, colWarn = th.Fg("error"), th.Fg("warning")
	sErrBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colErr).Padding(0, 1)
}

// boxed renders s in the bordered style b, w wide inside the border.
func boxed(b lipgloss.Style, w int, s string) string {
	return b.Width(w + b.GetHorizontalBorderSize()).Render(s)
}

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
		hdr[i] = sHeader.Render(align(c.Title, widths[i], c.Right))
	}
	out = append(out, ansi.Truncate("  "+strings.Join(hdr, "  "), width, "…"))
	for ri, row := range rows {
		cells := make([]string, len(cols))
		for i := range cols {
			v := ""
			if i < len(row) {
				v = row[i]
			}
			cells[i] = align(v, widths[i], cols[i].Right)
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
	if line == "" {
		line = " "
	}
	return sSelected.Render(sKey.Render("›") + txt.Fit(line[1:], max(width-1, 0)))
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

// align fits s to w cells, right-aligned when right.
func align(s string, w int, right bool) string {
	if right {
		return txt.FitLeft(s, w)
	}
	return txt.Fit(s, w)
}

func rule(width int) string { return sRule.Render(strings.Repeat("─", max(width, 0))) }

func shortHome(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if h, err := userHome(); err == nil && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}
