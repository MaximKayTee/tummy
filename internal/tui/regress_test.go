package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Findings of the 2026-11-05 review, kept as regressions.

func TestPasteKeepsLines(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "/")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a\x00b\tc"), Paste: true})
	if got := mm.(Model).filter.Value(); got != "ab c" { // the input turns the tab into a space itself
		t.Fatalf("paste: %q", got)
	}
}

func TestZeroWidthDoesNotPanic(t *testing.T) {
	m := loaded(t, testHome(t))
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 0, Height: 0})
	m = mm.(Model)
	for _, v := range []view{viewList, viewDetail, viewConns} {
		m.view = v
		_ = m.View()
	}
}

func TestBrokenRepIsNotMarkedRunning(t *testing.T) {
	m := loaded(t, testHome(t))
	for i, r := range m.snap.Reps {
		if r.Err != "" || r.ConnErr != "" {
			m.cursor[viewList] = i
			mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
			if mm.(Model).inflight[r.Name] {
				t.Fatalf("%s marked running after a refused run", r.Name)
			}
			return
		}
	}
	t.Skip("no broken replication in the fixture")
}

func TestFilterClearedAcrossViews(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "/")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("alpha")})
	m = press(mm.(Model), "enter", "c")
	if m.filter.Value() != "" || len(m.visibleConns()) == 0 {
		t.Fatalf("filter %q carried into connections", m.filter.Value())
	}
}

func TestHiddenEntraChoiceDoesNotApply(t *testing.T) {
	opts := testHome(t)
	w, _ := newConnWizard(opts, "")
	f := w.form
	f.set("name", "X")
	f.set("type", "mssql")
	f.set("host", "h")
	f.set("fedauth", "ActiveDirectoryDefault")
	f.set("type", "postgres")
	f.set("user", "u")
	if url := w.conn().URL; strings.Contains(url, "fedauth") || !strings.HasPrefix(url, "postgres://u") {
		t.Fatalf("url %s", url)
	}
	if !f.shown("password") {
		t.Fatal("password hidden by a choice that no longer applies")
	}
}
