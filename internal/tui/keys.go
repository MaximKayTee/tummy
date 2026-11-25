package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// pasted is pasted text routed like a key. Its name matches no binding.
type pasted string

func (p pasted) String() string { return "[" + string(p) + "]" }
func (p pasted) Key() tea.Key   { return tea.Key{Code: tea.KeyExtended, Text: string(p)} }

// teaMsg is msg as the bubbles components take it.
func teaMsg(msg tea.KeyMsg) tea.Msg {
	if p, ok := msg.(pasted); ok {
		return tea.PasteMsg{Content: string(p)}
	}
	return msg
}

// dropControls removes control characters; pasted text keeps line breaks and tabs.
func dropControls(s string, paste bool) string {
	return strings.Map(func(r rune) rune {
		if paste && (r == '\n' || r == '\r' || r == '\t') {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// typedRunes is the text of a key that types something other than a space.
func typedRunes(msg tea.KeyMsg) string {
	k := msg.Key()
	if k.Mod.Contains(tea.ModAlt) || k.Mod.Contains(tea.ModCtrl) || strings.TrimSpace(k.Text) == "" {
		return ""
	}
	return k.Text
}
