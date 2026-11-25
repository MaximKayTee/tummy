package tui

import (
	"github.com/charmbracelet/x/ansi"

	"github.com/MaximKayTee/gotuitools/help"
)

func widthOf(s string) int { return ansi.StringWidth(s) }

// plain drops the styling.
func plain(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.Strip(l)
	}
	return out
}

func plainScreen(m Model) string { return ansi.Strip(m.screen()) }

// helpOpen reports the keys screen on top.
func helpOpen(m Model) bool {
	_, ok := m.ui.modals.Top().(*help.Screen)
	return ok
}
