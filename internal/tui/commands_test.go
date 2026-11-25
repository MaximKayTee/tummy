package tui

import (
	"cmp"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/MaximKayTee/gotuitools/palette"
	"github.com/MaximKayTee/gotuitools/settingsui"
	"github.com/MaximKayTee/gotuitools/theme"

	"tummy/internal/daemon"
)

// drain runs cmd and feeds what it sends back into the model, as Bubble Tea would.
// Cmds that do not answer quickly (ticks, blinks) are dropped.
func drain(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for n := 0; len(queue) > 0 && n < 200; n++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		got := make(chan tea.Msg, 1)
		go func() { got <- c() }()
		var msg tea.Msg
		select {
		case msg = <-got:
		case <-time.After(100 * time.Millisecond):
			continue
		}
		switch msg := msg.(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			if reflect.TypeOf(msg).Kind() == reflect.Slice { // a sequence
				v := reflect.ValueOf(msg)
				for i := range v.Len() {
					c, _ := v.Index(i).Interface().(tea.Cmd)
					queue = append(queue, c)
				}
				continue
			}
			mm, next := m.Update(msg)
			m = mm.(Model)
			queue = append(queue, next)
		}
	}
	return m
}

func sendAll(t *testing.T, m Model, msgs ...tea.Msg) Model {
	t.Helper()
	for _, msg := range msgs {
		mm, cmd := m.Update(msg)
		m = drain(t, mm.(Model), cmd)
	}
	return m
}

var (
	keyCtrlP     = tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}
	keyCtrlComma = tea.KeyPressMsg{Code: ',', Mod: tea.ModCtrl}
)

func TestEveryActionHasKeysAndHelp(t *testing.T) {
	m := loaded(t, testHome(t))
	for _, a := range actions {
		if _, ok := m.ui.reg.Get(a.name); !ok {
			t.Errorf("%s not registered", a.name)
		}
		if len(a.keys) == 0 {
			t.Errorf("%s has no keys", a.name)
		}
	}
	for _, mode := range allModes {
		listed := map[string]bool{}
		for _, g := range m.commandGroups(mode) {
			for _, r := range g.Rows {
				listed[r.Action] = true
			}
		}
		for _, a := range actions {
			if _, ok := a.keys[mode]; !ok {
				continue
			}
			if k := m.keyFor(mode, a.name); k == "" {
				t.Errorf("%s: %s unbound", mode, a.name)
			}
			if !listed[cmp.Or(a.help, a.title)] {
				t.Errorf("%s: %s missing from the keys screen", mode, a.name)
			}
		}
	}
}

func TestKeysScreenShowsKeyNames(t *testing.T) {
	m := loaded(t, testHome(t))
	rows := map[string]string{}
	for _, g := range m.commandGroups(modeList) {
		for _, r := range g.Rows {
			rows[r.Action] = r.Keys
		}
	}
	if got := rows["streams of the replication"]; got != "Enter, l, Right" {
		t.Errorf("open: %q", got)
	}
	if got := rows["quit; asks when there are changes or runs"]; got != "q, Ctrl+C" {
		t.Errorf("quit: %q", got)
	}
}

func TestPaletteRunsCommand(t *testing.T) {
	m := loaded(t, testHome(t))
	m = sendAll(t, m, keyCtrlP)
	p, ok := m.ui.modals.Top().(*palette.Model)
	if !ok {
		t.Fatal("ctrl+p: no palette")
	}
	titles := map[string]string{}
	for _, it := range p.Shown() {
		titles[it.Title] = it.Note
	}
	if titles["Run"] != "r" || titles["Connections"] != "c" || titles["Settings"] != "ctrl+," {
		t.Fatalf("palette items: %v", titles)
	}
	if _, ok := titles["Stream history"]; ok {
		t.Error("detail commands on the list")
	}
	m = sendAll(t, m, keyRunes("q"), tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.ui.modals.Len() != 1 {
		t.Fatal("q types in the palette")
	}
	for _, r := range "connections" {
		m = sendAll(t, m, keyRunes(string(r)))
	}
	m = sendAll(t, m, keyEnter)
	if m.view != viewConns || m.ui.modals.Len() != 0 {
		t.Fatalf("palette choice: view %d, modals %d", m.view, m.ui.modals.Len())
	}
}

func TestPaletteLeavesOutWhatCannotRun(t *testing.T) {
	m := loaded(t, testHome(t))
	m = sendAll(t, m, keyCtrlP)
	for _, it := range m.ui.modals.Top().(*palette.Model).Shown() {
		if it.Title == "Cancel run" {
			t.Error("cancel offered with nothing running")
		}
	}
}

func TestHelpCtrlCQuits(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "?")
	if !helpOpen(m) {
		t.Fatal("? opens keys")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+c over the keys screen")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c quits")
	}
}

func withSettings(t *testing.T, body string) Options {
	t.Helper()
	opts := testHome(t)
	opts.Settings = filepath.Join(t.TempDir(), "config.toml")
	if body != "" {
		os.WriteFile(opts.Settings, []byte(body), 0o644)
	}
	return opts
}

func TestOwnKeys(t *testing.T) {
	m := loaded(t, withSettings(t, "theme = \"tummy\"\n[keys]\n\"ctrl+t\" = \"view.history\"\n\"H\" = \"\"\n"))
	if m.status.IsErr {
		t.Fatalf("status: %s", m.status.Text)
	}
	m = press(m, "H")
	if m.view != viewList {
		t.Fatal("H freed")
	}
	m = sendAll(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if m.view != viewHistory {
		t.Fatal("ctrl+t: history")
	}
	m = press(m, "esc")
	if v := plainScreen(m); !strings.Contains(v, "ctrl+t history") {
		t.Errorf("footer shows the own key:\n%s", v)
	}
}

func TestOwnKeysUnknownCommand(t *testing.T) {
	m := loaded(t, withSettings(t, "[keys]\n\"ctrl+t\" = \"nope\"\n"))
	if !m.status.IsErr || !strings.Contains(m.status.Text, "no command nope") {
		t.Fatalf("status: %+v", m.status)
	}
}

func TestSettingsFileWritten(t *testing.T) {
	opts := withSettings(t, "")
	loaded(t, opts)
	b, err := os.ReadFile(opts.Settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`theme = "tummy"`, "rep.run", "commands:"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("settings file missing %q:\n%s", want, b)
		}
	}
}

func TestSettingsScreenChangesTheme(t *testing.T) {
	t.Cleanup(func() { applyTheme(mustTheme(t, "tummy", true), true) })
	m := loaded(t, withSettings(t, ""))
	m = sendAll(t, m, keyCtrlComma)
	if _, ok := m.ui.modals.Top().(*settingsui.Model); !ok {
		t.Fatal("ctrl+, opens settings")
	}
	if _, err := m.ui.store.Set("theme", "charmtone"); err != nil {
		t.Fatal(err)
	}
	m = sendAll(t, m, settingsui.ChangedMsg{})
	if curTheme.Name() != "charmtone" {
		t.Fatalf("theme %q", curTheme.Name())
	}
	m = sendAll(t, m, tea.BackgroundColorMsg{Color: lipgloss.Color("#ffffff")})
	if curTheme.Name() != "charmtone-light" {
		t.Fatalf("light theme %q", curTheme.Name())
	}
}

func TestNoSettingsFile(t *testing.T) {
	m := loaded(t, testHome(t))
	m = sendAll(t, m, keyCtrlComma)
	if m.ui.modals.Len() != 0 || !m.status.IsErr {
		t.Fatalf("no settings: %+v", m.status)
	}
}

func mustTheme(t *testing.T, name string, dark bool) theme.Theme {
	t.Helper()
	th, err := theme.FindFor(name, dark)
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func TestThemeKeepsColours(t *testing.T) {
	t.Cleanup(func() { applyTheme(mustTheme(t, "tummy", true), true) })
	for _, c := range []struct {
		dark                     bool
		name                     string
		accent, ok, err, sel, hd string
	}{
		{true, "tummy", "#9D8CFF", "#5BD58A", "#FF6B6B", "#2D2A45", "#A0A0A0"},
		{false, "tummy-light", "#5B4BDB", "#1F8F4E", "#C23B3B", "#ECE9FF", "#5C5C5C"},
	} {
		th := mustTheme(t, "tummy", c.dark)
		if th.Name() != c.name {
			t.Fatalf("FindFor dark=%v: %s", c.dark, th.Name())
		}
		applyTheme(th, c.dark)
		same := func(what string, got any, hex string) {
			if !reflect.DeepEqual(got, lipgloss.Color(hex)) {
				t.Errorf("%s %s: %v, want %s", c.name, what, got, hex)
			}
		}
		same("title", sTitle.GetForeground(), c.accent)
		same("key", sKey.GetForeground(), c.accent)
		same("ok", sOK.GetForeground(), c.ok)
		same("error", sErr.GetForeground(), c.err)
		same("selected", sSelected.GetBackground(), c.sel)
		same("header", sHeader.GetForeground(), c.hd)
		if !sTitle.GetBold() || !sSelected.GetBold() || !sHeader.GetBold() || !sStatus.GetItalic() {
			t.Errorf("%s: attributes", c.name)
		}
	}
}

func TestKeysScreenGroupsInWizard(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "c", "enter") // explore SRC
	var names []string
	for _, g := range m.helpGroups() {
		names = append(names, g.Name)
	}
	got := strings.Join(names, ",")
	if !strings.HasPrefix(got, "tables,Anywhere,Files") {
		t.Fatalf("groups: %s", got)
	}
}

func TestPaletteInstallDaemon(t *testing.T) {
	if !daemon.CanInstall() {
		t.Skip("no systemctl")
	}
	shown := func(m Model) bool {
		m = sendAll(t, m, keyCtrlP)
		for _, r := range "install" {
			m = sendAll(t, m, keyRunes(string(r)))
		}
		for _, it := range m.ui.modals.Top().(*palette.Model).Shown() {
			if it.Title == "Install daemon" {
				return true
			}
		}
		return false
	}
	if shown(loaded(t, testHome(t))) {
		t.Error("offered with no serve command")
	}
	m := loaded(t, testHome(t))
	m.opts.ServeCommand = []string{"tummy", "serve"}
	if !shown(m) {
		t.Fatal("not offered with no daemon")
	}
	m = drain(t, m, func() tea.Msg { return actionMsg("daemon.install") })
	if m.ask == nil || !strings.Contains(m.ask.text, daemon.UnitName) {
		t.Fatalf("no confirm: %+v", m.ask)
	}
}
