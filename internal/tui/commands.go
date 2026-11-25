package tui

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/MaximKayTee/gotuitools/command"
	"github.com/MaximKayTee/gotuitools/help"
	"github.com/MaximKayTee/gotuitools/palette"
	"github.com/MaximKayTee/gotuitools/settings"
	txt "github.com/MaximKayTee/gotuitools/text"

	"tummy/internal/daemon"
	"tummy/internal/engine"
)

// Key modes: one per screen, and the keys that work over any wizard.
const (
	modeList    = "list"
	modeDetail  = "detail"
	modeHistory = "history"
	modeRun     = "run"
	modeConns   = "conns"
	modeWizard  = "wizard"
)

var (
	screens  = []string{modeList, modeDetail, modeHistory, modeRun, modeConns}
	allModes = append(slices.Clone(screens), modeWizard)
)

// action is a command with its keys by mode and its line on the keys screen.
type action struct {
	name, title, group string
	keys               map[string]string // mode → keys
	help               string            // keys screen; "" is the title
	hidden             bool              // not in the palette
}

// on binds keys in the given modes.
func on(keys string, modes ...string) map[string]string {
	out := map[string]string{}
	for _, m := range modes {
		out[m] = keys
	}
	return out
}

// and merges bindings.
func and(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		maps.Copy(out, m)
	}
	return out
}

const enterKeys = "enter l right"

// actions are every command of the screens, in keys screen order. One grammar in every
// view: E edit, e on/off, d delete (y/n), ctrl+s save, esc back, enter open or next.
// Backspace only goes back.
var actions = []action{
	{name: "rep.open", title: "Open replication", group: "Replications", keys: on(enterKeys, modeList), help: "streams of the replication"},
	{name: "rep.new", title: "New replication", group: "Replications", keys: on("n", modeList)},
	{name: "rep.edit", title: "Edit replication", group: "Replications", keys: on("E", modeList)},
	{name: "rep.duplicate", title: "Duplicate replication", group: "Replications", keys: on("D", modeList), help: "duplicate: a new replication with its streams and defaults"},
	{name: "rep.toggle", title: "Replication on/off", group: "Replications", keys: on("e", modeList), help: "on/off for the daemon"},
	{name: "rep.delete", title: "Delete replication", group: "Replications", keys: on("d", modeList), help: "delete the file (y/n)"},
	{name: "rep.run", title: "Run", group: "Replications", keys: on("r", modeList, modeDetail), help: "run now, every stream"},
	{name: "rep.run.detached", title: "Run detached", group: "Replications", keys: on("ctrl+r", modeList, modeDetail), help: "run now as its own process; survives quitting the TUI"},
	{name: "rep.plan", title: "Dry run", group: "Replications", keys: on("t", modeList, modeDetail), help: "dry run: target table, action, source query and columns per stream, nothing written"},
	{name: "rep.cancel", title: "Cancel run", group: "Replications", keys: on("x", modeList, modeDetail), help: "cancel a running run (y/n)"},
	{name: "rep.commit", title: "Commit run", group: "Replications", keys: on("C", modeList, modeDetail), help: "commit a running run: stop reading, write what is loaded, end as ok (y/n)"},

	{name: "stream.open", title: "Open stream", group: "Streams", keys: on(enterKeys, modeDetail), help: "loaded data in the target; a failed stream: its run; a bookmark: its query"},
	{name: "stream.preview", title: "Preview source", group: "Streams", keys: on("p", modeDetail), help: "source preview; steps there are not saved to the stream"},
	{name: "stream.edit", title: "Edit stream", group: "Streams", keys: on("E", modeDetail), help: "edit the stream, else the replication; the stream's preview saves with ctrl+s"},
	{name: "stream.add", title: "Add streams", group: "Streams", keys: on("a", modeDetail), help: "add tables or an endpoint"},
	{name: "stream.toggle", title: "Stream on/off", group: "Streams", keys: on("e", modeDetail), help: "stream on/off; (replication): the replication"},
	{name: "stream.delete", title: "Delete stream", group: "Streams", keys: on("d", modeDetail), help: "delete the stream or bookmark (y/n); from a table pattern: exclude it; run history only: forget the runs"},
	{name: "stream.run", title: "Run stream", group: "Streams", keys: on("s", modeDetail)},
	{name: "stream.run.detached", title: "Run stream detached", group: "Streams", keys: on("S", modeDetail), help: "run the stream as its own process"},
	{name: "stream.history", title: "Stream history", group: "Streams", keys: on("H", modeDetail)},

	{name: "run.open", title: "Run details", group: "History", keys: on(enterKeys, modeHistory)},
	{name: "run.forget", title: "Forget run", group: "History", keys: on("d", modeHistory), help: "forget the run (y/n); the one before it becomes the last"},

	{name: "run.again", title: "Run again", group: "Run", keys: on("r", modeRun), help: "run again; a dry run: run it"},
	{name: "run.testload", title: "Test load", group: "Run", keys: on("t", modeRun), help: fmt.Sprintf("dry run: test load, the first %d rows per stream into a copy of the run's temp table, rolled back", engine.SampleRows)},
	{name: "run.edit", title: "Edit", group: "Run", keys: on("E", modeRun), help: "edit the stream, else the connection in the error"},

	{name: "conn.explore", title: "Explore connection", group: "Connections", keys: on(enterKeys, modeConns), help: "explore tables"},
	{name: "conn.new", title: "New connection", group: "Connections", keys: on("n", modeConns)},
	{name: "conn.edit", title: "Edit connection", group: "Connections", keys: on("E", modeConns)},
	{name: "conn.test", title: "Test connection", group: "Connections", keys: on("t", modeConns)},
	{name: "conn.delete", title: "Delete connection", group: "Connections", keys: on("d", modeConns), help: "delete (y/n)"},
	{name: "conn.create", title: "Create file", group: "Connections", keys: on("c", modeConns), help: "create a missing sqlite file"},

	{name: "view.conns", title: "Connections", group: "Screens", keys: on("c", modeList, modeDetail, modeHistory, modeRun)},
	{name: "view.history", title: "History", group: "Screens", keys: on("H", modeList), help: "run history"},

	{name: "up", title: "Up", group: "Move", keys: on("up k", screens...), hidden: true},
	{name: "down", title: "Down", group: "Move", keys: on("down j", screens...), hidden: true},
	{name: "page.up", title: "Page up", group: "Move", keys: on("pgup", screens...), hidden: true},
	{name: "page.down", title: "Page down", group: "Move", keys: on("pgdown", screens...), hidden: true},
	{name: "top", title: "Top", group: "Move", keys: on("g home", modeList, modeDetail, modeHistory, modeConns), hidden: true},
	{name: "bottom", title: "Bottom", group: "Move", keys: on("G end", modeList, modeDetail, modeHistory, modeConns), hidden: true},
	{name: "back", title: "Back", group: "Move", keys: and(on("esc backspace h left", modeList), on("esc backspace h left q", modeDetail, modeHistory, modeRun, modeConns)), help: "back; on the list: clear the filter"},
	{name: "filter", title: "Filter", group: "Move", keys: on("/", modeList, modeHistory, modeConns)},
	{name: "refresh", title: "Refresh", group: "Move", keys: on("R", screens...)},

	{name: "palette", title: "Commands", group: "Anywhere", keys: on("ctrl+p", screens...), hidden: true},
	{name: "help", title: "Keys", group: "Anywhere", keys: and(on("? f1", screens...), on("f1 ?", modeWizard)), help: "keys; ? types in a text field"},
	{name: "daemon.install", title: "Install daemon", group: "Replications", keys: on("I", modeList), help: "systemd user service running tummy serve for this config, enabled at boot"},
	{name: "settings", title: "Settings", group: "Anywhere", keys: on("ctrl+,", screens...)},
	{name: "quit", title: "Quit", group: "Anywhere", keys: and(on("q ctrl+c", modeList), on("ctrl+c", modeDetail, modeHistory, modeRun, modeConns, modeWizard)), help: "quit; asks when there are changes or runs"},
	{name: "suspend", title: "Suspend", group: "Anywhere", keys: on("ctrl+z", allModes...), help: "suspend; fg resumes"},
}

var actionsByName = func() map[string]action {
	out := map[string]action{}
	for _, a := range actions {
		out[a.name] = a
	}
	return out
}()

// actionMsg runs a command chosen in the palette.
type actionMsg string

// newRegistry holds every action.
func newRegistry() *command.Registry {
	var reg command.Registry
	for _, a := range actions {
		name := a.name
		reg.MustAdd(command.Command{Name: name, Title: a.title, Group: a.group, Hidden: a.hidden,
			Run: func([]string) tea.Cmd { return func() tea.Msg { return actionMsg(name) } }})
	}
	return &reg
}

// keyModes are each mode's commands with their default keys.
func keyModes() []settings.KeyMode {
	out := make([]settings.KeyMode, len(allModes))
	for i, mode := range allModes {
		out[i].Mode = mode
		for _, a := range actions {
			if k, ok := a.keys[mode]; ok {
				out[i].Commands = append(out[i].Commands, command.Command{Name: a.name, Title: a.title, Group: a.group, Key: k})
			}
		}
	}
	return out
}

// loadKeys binds every mode's default keys, then own (keys → command) in the modes that
// have the command. An unknown command or a refused binding is an error; the defaults stay.
func loadKeys(own map[string]string) (*command.Keys, error) {
	keys := &command.Keys{}
	var errs []string
	for keysText, name := range own {
		if _, ok := actionsByName[name]; name != "" && !ok {
			errs = append(errs, fmt.Sprintf("keys: %q: no command %s", keysText, name))
		}
	}
	for _, km := range keyModes() {
		mode, cmds := km.Mode, km.Commands
		mine := map[string]string{}
		for k, name := range own {
			if _, ok := actionsByName[name].keys[mode]; ok || name == "" {
				mine[k] = name
			}
		}
		if err := keys.Load(mode, cmds, mine); err != nil {
			errs = append(errs, "keys: "+err.Error())
			keys.Load(mode, cmds, nil)
		}
	}
	slices.Sort(errs)
	if len(errs) > 0 {
		return keys, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return keys, nil
}

// mode is the key mode of what is on screen.
func (m Model) mode() string {
	switch m.view {
	case viewWizard:
		return modeWizard
	case viewDetail:
		return modeDetail
	case viewHistory:
		return modeHistory
	case viewRun:
		return modeRun
	case viewConns:
		return modeConns
	}
	return modeList
}

// keyCommand is the command msg runs in mode, "" none. In a wizard taking text, keys
// that type are the wizard's.
func (m Model) keyCommand(mode string, msg tea.KeyMsg) string {
	if mode == modeWizard && typedRunes(msg) != "" && wizTyping(m.wiz) {
		return ""
	}
	name, _ := m.ui.keys.Match(mode, []string{msg.String()})
	return name
}

// keyFor is the key shown for a command in mode: its first default key while bound to it,
// else its shortest key; "" when unbound.
func (m Model) keyFor(mode, name string) string {
	if def := strings.Fields(actionsByName[name].keys[mode]); len(def) > 0 {
		if got, _ := m.ui.keys.Match(mode, []string{def[0]}); got == name {
			return def[0]
		}
	}
	return m.ui.keys.For(mode, name)
}

// hints are key pairs for the footer: command name, then label, for each; unbound
// commands are left out.
func (m Model) hints(mode string, pairs ...string) string {
	var out []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if k := m.keyFor(mode, pairs[i]); k != "" {
			out = append(out, k, pairs[i+1])
		}
	}
	return helpLine(out...)
}

// openPalette lists the commands of the screen that can run now.
func (m Model) openPalette() tea.Cmd {
	mode := m.mode()
	var items []palette.Item
	for _, c := range m.ui.reg.All() {
		if _, ok := actionsByName[c.Name].keys[mode]; !ok || c.Hidden || !m.can(c.Name) {
			continue
		}
		items = append(items, palette.Item{Title: c.Label(), Note: m.keyFor(mode, c.Name), Group: c.Group, Search: []string{c.Name}, Value: c.Name})
	}
	p := palette.New(curTheme, "Commands", items, func(it palette.Item) tea.Cmd {
		return m.ui.reg.Run(it.Value.(string))
	})
	return m.ui.modals.Open(p)
}

// can reports whether a command of the current screen does anything now.
func (m Model) can(name string) bool {
	if name == "daemon.install" {
		return m.opts.ServeCommand != nil && m.loaded && !m.daemonServes() && daemon.CanInstall()
	}
	switch m.view {
	case viewList:
		cur, ok := at(m.visibleReps(), m.cursor[viewList])
		switch name {
		case "rep.new":
			return true
		case "rep.duplicate":
			return ok && cur.Err == ""
		case "rep.cancel", "rep.commit":
			return ok && cur.Running > 0
		case "rep.open", "rep.edit", "rep.toggle", "rep.delete", "rep.run", "rep.run.detached", "rep.plan":
			return ok
		}
	case viewDetail:
		rep, ok := m.rep(m.repName)
		if !ok {
			return false
		}
		row, onRow := at(rep.detailRows(), m.cursor[viewDetail])
		stream := onRow && row.mark == nil
		switch name {
		case "rep.cancel", "rep.commit":
			return rep.Running > 0
		case "stream.edit", "stream.add":
			return rep.Err == ""
		case "stream.open", "stream.delete":
			return onRow
		case "stream.preview", "stream.history":
			return stream
		case "stream.run", "stream.run.detached":
			return stream && row.stream != "*"
		}
	case viewHistory:
		if name == "run.open" || name == "run.forget" {
			_, ok := at(m.visibleRuns(), m.cursor[viewHistory])
			return ok
		}
	case viewRun:
		switch name {
		case "run.testload":
			return m.planName != "" && !m.planning
		case "run.edit":
			return m.planName == ""
		}
	case viewConns:
		cur, ok := at(m.visibleConns(), m.cursor[viewConns])
		switch name {
		case "conn.create":
			return ok && m.missingFile(cur.Name) != ""
		case "conn.explore", "conn.edit", "conn.test", "conn.delete":
			return ok
		}
	}
	return true
}

// openHelp shows the keys of the current screen, then forms and schedules.
func (m Model) openHelp() tea.Cmd {
	return m.ui.modals.Open(help.New(curTheme, "Keys", m.helpGroups(), ""))
}

// helpGroups are the keys screen: the mode's commands by group, a wizard's own keys, then
// files, forms and schedules.
func (m Model) helpGroups() []help.Group {
	var groups []help.Group
	if m.wiz != nil {
		if kh, ok := m.wiz.(keyHelper); ok {
			title, rows := kh.keyHelp()
			g := help.Group{Name: title}
			for _, r := range rows {
				g.Rows = append(g.Rows, help.Row{Keys: r[0], Action: r[1]})
			}
			groups = append(groups, g)
		}
	}
	groups = append(groups, m.commandGroups(m.mode())...)
	groups = append(groups, help.Group{Name: "Files", Rows: []help.Row{
		{Keys: "config", Action: txt.Clean(shortHome(m.opts.Home))},
		{Keys: "connections", Action: txt.Clean(shortHome(m.opts.Connections))},
		{Keys: "state", Action: txt.Clean(shortHome(m.opts.State.Path))},
	}})
	if m.opts.Settings != "" {
		files := &groups[len(groups)-1]
		files.Rows = append(files.Rows, help.Row{Keys: "settings", Action: txt.Clean(shortHome(m.opts.Settings))})
	}
	return append(groups, formGroups...)
}

// commandGroups are the commands bound in mode, by group, each with every key.
func (m Model) commandGroups(mode string) []help.Group {
	var groups []help.Group
	at := map[string]int{}
	for _, c := range m.ui.reg.All() {
		bound := m.ui.keys.All(mode, c.Name)
		if len(bound) == 0 {
			continue
		}
		if first := m.keyFor(mode, c.Name); first != "" {
			bound = append([]string{first}, slices.DeleteFunc(bound, func(k string) bool { return k == first })...)
		}
		names := make([]string, len(bound))
		for i, k := range bound {
			names[i] = help.KeyName(k)
		}
		i, ok := at[c.Group]
		if !ok {
			i = len(groups)
			at[c.Group] = i
			groups = append(groups, help.Group{Name: c.Group})
		}
		groups[i].Rows = append(groups[i].Rows, help.Row{Keys: strings.Join(names, ", "), Action: cmp.Or(actionsByName[c.Name].help, c.Title)})
	}
	return groups
}

// formGroups are the keys of forms and lists, and the schedule syntax.
var formGroups = []help.Group{
	{Name: "Fields", Rows: []help.Row{
		{Keys: "tab  ↓", Action: "next field"}, {Keys: "shift+tab  ↑", Action: "previous field"},
		{Keys: "enter", Action: "next field; open a list; on the last field: apply"},
		{Keys: "pgup/pgdn", Action: "five fields"}, {Keys: "←/→", Action: "cycle a list's value"}, {Keys: "space", Action: "on/off; open a list"},
		{Keys: "type", Action: "on a list: open it filtered"}, {Keys: "ctrl+s", Action: "save"}, {Keys: "esc", Action: "back"},
	}},
	{Name: "Open list", Rows: []help.Row{
		{Keys: "type", Action: "filter"}, {Keys: "↑/↓  pgup/pgdn", Action: "move"}, {Keys: "home/end", Action: "first / last"}, {Keys: "space", Action: "tick"},
		{Keys: "ctrl+a / ctrl+n", Action: "tick all / none shown"}, {Keys: "enter", Action: "pick; done"}, {Keys: "esc", Action: "close, no change"},
		{Keys: "new", Action: "typed value"},
	}},
	{Name: "Schedule", Rows: []help.Row{
		{Keys: "m h dom mon dow", Action: "minute  hour  day  month  weekday, UTC"},
		{Keys: "*/15 * * * *", Action: "every 15 min"}, {Keys: "0 * * * *", Action: "hourly"}, {Keys: "30 3 * * *", Action: "03:30 UTC daily"},
		{Keys: "0 6 * * 1-5", Action: "06:00 UTC Mon-Fri"}, {Keys: "0 0 1 * *", Action: "00:00 UTC on the 1st"}, {Keys: "@every 10m", Action: "every 10 min"},
		{Keys: "@hourly  @daily", Action: "0 * * * *   0 0 * * *"}, {Keys: "CRON_TZ=Area/City", Action: "time zone first: CRON_TZ=Europe/Berlin 0 6 * * * is 06:00 Berlin time"},
		{Keys: "*  */n", Action: "any, every n"}, {Keys: "a-b  a,b", Action: "range, list"}, {Keys: "weekday", Action: "0-6, 0 Sunday"},
	}},
}
