package tui

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/MaximKayTee/gotuitools/command"
	"github.com/MaximKayTee/gotuitools/modal"
	"github.com/MaximKayTee/gotuitools/settings"
	"github.com/MaximKayTee/gotuitools/settingsui"
	"github.com/MaximKayTee/gotuitools/theme"
)

// Settings are the program's preferences: config.toml in the user config directory.
type Settings struct {
	Theme string `toml:"theme" options:"themes" help:"a light terminal takes <theme>-light when there is one"`
	// Keys binds keys to commands: "ctrl+r" = "rep.run"; "" frees the keys.
	Keys map[string]string `toml:"keys"`
}

var defaultSettings = Settings{Theme: "tummy"}

// SettingsPath is <UserConfigDir>/tummy/config.toml.
func SettingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "tummy", "config.toml")
}

// themeNames are the built-in themes and those in the theme directories.
func themeNames() []string {
	names := theme.BuiltIn()
	for _, dir := range theme.Dirs("tummy") {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if n, ok := strings.CutSuffix(e.Name(), ".toml"); ok && !e.IsDir() {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// settingsHeader is the file's first comment.
func settingsHeader() []string {
	names := make([]string, len(actions))
	for i, a := range actions {
		names[i] = a.name
	}
	return []string{
		"tummy settings",
		`[keys]: "key" = "command"; "" frees the key`,
		"commands: " + strings.Join(names, " "),
	}
}

// ui is the state shared by every copy of the model.
type ui struct {
	reg    *command.Registry
	keys   *command.Keys
	modals *modal.Stack
	store  *settings.Store[Settings] // nil without a settings file
	dark   bool                      // terminal background
	theme  string                    // applied theme name
	own    map[string]string         // applied [keys]
}

// newUI opens the settings file at path ("" none), returning a problem to show.
func newUI(path string) (*ui, error) {
	u := &ui{reg: newRegistry(), modals: modal.NewStack(), dark: true}
	var problem error
	if path != "" {
		store, err := settings.Open(path, defaultSettings, func(s *settings.Store[Settings]) {
			s.Options("themes", themeNames)
			s.Header(settingsHeader()...)
		})
		var le *settings.LoadError
		if err == nil || errors.As(err, &le) {
			u.store = store
			store.Keys("keys", keyModes)
			if ferr := store.Fill(); err == nil {
				err = ferr
			}
		}
		problem = err
	}
	if err := u.sync(true); err != nil && problem == nil {
		problem = err
	}
	return u, problem
}

// prefs are the settings now.
func (u *ui) prefs() Settings {
	if u.store == nil {
		return defaultSettings
	}
	return u.store.Get()
}

// sync applies the theme and keys of the settings when they changed, or always with force.
func (u *ui) sync(force bool) error {
	p := u.prefs()
	var errs []error
	if force || p.Theme != u.theme {
		th, err := theme.FindFor(p.Theme, u.dark, theme.Dirs("tummy")...)
		if err != nil {
			errs = append(errs, err)
			th, _ = theme.FindFor("tummy", u.dark)
		}
		applyTheme(th, u.dark)
		u.theme = p.Theme
	}
	if force || u.keys == nil || !maps.Equal(p.Keys, u.own) {
		keys, err := loadKeys(p.Keys)
		u.keys, u.own = keys, maps.Clone(p.Keys)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// openSettings shows the settings screen.
func (m Model) openSettings() (tea.Model, tea.Cmd) {
	if m.ui.store == nil {
		return m, status("no settings file", true)
	}
	return m, m.ui.modals.Open(settingsui.New(curTheme, m.ui.store, "Settings"))
}
