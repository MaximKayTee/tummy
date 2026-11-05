package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"tummy/internal/txt"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/api"
	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/engine"
)

var schedulePresets = []struct{ label, expr string }{
	{"none", ""},
	{"every 5 min", "*/5 * * * *"},
	{"every 15 min", "*/15 * * * *"},
	{"hourly", "0 * * * *"},
	{"daily 03:00 UTC", "0 3 * * *"},
	{"weekdays 06:00 UTC", "0 6 * * 1-5"},
	{"custom", "custom"},
}

// newConnRow is the row of a connection list that opens the connection editor.
const newConnRow = "new connection…"

// discoverMsg carries the tables of a connection. For tells the replication wizard
// whether they are the source's or the target's.
type discoverMsg struct {
	Conn   string
	For    string
	Tables []string
	Err    error
}

// columnsMsg carries a table's columns for the stream editor.
type columnsMsg struct {
	Stream string
	Cols   []string
	Kinds  map[string]db.Kind
	PK     []string // the table's primary key in the source
	Err    error
}

// tableColsMsg carries the columns of the draft's tables, for the defaults' key lists.
type tableColsMsg struct {
	Cols map[string][]db.Column
}

// sampleMsg carries the columns of an API stream's first page.
type sampleMsg struct {
	Cols    []db.Column
	Records int
	Err     error
}

// discoverTables lists every table of every schema of a connection.
func discoverTables(c *config.Conn, role string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if path := missingFile(c); path != "" { // opening would create it
			if role == "target" {
				return discoverMsg{Conn: c.Name, For: role, Tables: []string{}}
			}
			return discoverMsg{Conn: c.Name, For: role, Err: fmt.Errorf("conn %s: no such file: %s", c.Name, path)}
		}
		d, err := db.Open(ctx, c.DB())
		if err != nil {
			return discoverMsg{Conn: c.Name, For: role, Err: err}
		}
		defer d.Close()
		all, err := d.ListAllTables(ctx)
		tables := []string{}
		for _, t := range all {
			tables = append(tables, t.String())
		}
		return discoverMsg{Conn: c.Name, For: role, Tables: tables, Err: err}
	}
}

type repStep int

const (
	stepBasics repStep = iota
	stepDefaults
	stepStreams
	stepReview
)

var stepNames = []string{"Basics", "Defaults", "Streams", "Review"}

// repEntry is how the wizard was opened.
type repEntry int

const (
	entryFull repEntry = iota // n or E on the list: every step
	entryAdd                  // a on the detail view: add streams, review, save
	entryEdit                 // E on a stream: its form, save
)

type repWizard struct {
	opts     Options
	conns    *config.Connections
	original string
	orig     config.ReplicationDraft // as loaded
	draft    config.ReplicationDraft
	step     repStep
	entry    repEntry
	touched  bool // streams were added, removed or changed

	basics   *form
	defaults *form

	cur        int
	stream     *form // stream editor
	editing    int   // stream being edited, -1 for new
	pick       *picker
	lastPick   *picker // add: the picker behind the review
	beforePick *config.ReplicationDraft

	srcTables  []string
	srcErr     string
	srcLoading bool
	wantPick   bool
	tgtTables  []string
	tgtLoading bool
	colsFor    string // stream whose columns are being read
	colsErr    string
	colsSeen   map[string][]db.Column // columns of source tables read so far
	colsAsked  bool
	sampling   bool // fetching an API stream's first page

	child      *connWizard // new connection from the source or target list
	explore    *explorer   // a draft stream opened on its data: sql, columns, steps
	childField string

	ask        *wizAsk
	review     viewport.Model
	reviewErr  string
	reviewWarn string
	changes    int
	err        string
	done       statusMsg
	savedName  string
}

func newRepWizard(opts Options, conns *config.Connections, editing string) (*repWizard, error) {
	w := &repWizard{opts: opts, conns: conns, original: editing, editing: -1}
	if editing != "" {
		path, err := config.ReplicationPath(opts.Home, editing)
		if err != nil {
			return nil, err
		}
		r, err := config.LoadReplicationRaw(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("replication %s not found", editing)
		}
		if err != nil {
			return nil, err
		}
		w.draft, w.orig = config.DraftOf(r), config.DraftOf(r)
	} else {
		w.draft = config.ReplicationDraft{Enabled: true, Defaults: config.Stream{Mode: "full-refresh"}}
		w.orig = w.draft
	}
	w.buildBasics()
	return w, nil
}

// newAddStreamWizard adds streams to an existing replication: the table list or the
// endpoint form, then saves.
func newAddStreamWizard(opts Options, conns *config.Connections, name string) (*repWizard, error) {
	w, err := newRepWizard(opts, conns, name)
	if err != nil {
		return nil, err
	}
	w.entry = entryAdd
	return w, nil
}

// newEditStreamWizard opens one stream's form and saves on submit.
func newEditStreamWizard(opts Options, conns *config.Connections, name, stream string) (*repWizard, error) {
	w, err := newRepWizard(opts, conns, name)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(w.draft.Streams, func(s config.StreamDraft) bool { return s.Name == stream })
	if i < 0 {
		return nil, fmt.Errorf("stream %s not in %s.toml", stream, name)
	}
	w.entry, w.cur = entryEdit, i
	return w, nil
}

func (w *repWizard) connNames(apiToo bool) []string {
	var out []string
	for _, n := range w.conns.Names() {
		if apiToo || w.conns.Conn[n].Type != "api" {
			out = append(out, n)
		}
	}
	return out
}

// connNotes labels each connection with its type and where it points.
func connNotes(conns *config.Connections) map[string]string {
	out := map[string]string{}
	for n, c := range conns.Conn {
		out[n] = strings.TrimSpace(fmt.Sprintf("%-8s  %s", c.Type, c.Target()))
	}
	return out
}

func (w *repWizard) isAPI() bool {
	c, ok := w.conns.Conn[w.draft.Source]
	return ok && c.Type == "api"
}

func (w *repWizard) buildBasics() {
	d := &w.draft
	sources, targets := w.connNames(true), w.connNames(false)
	src := cmp.Or(d.Source, first(sources))
	tgt := d.Target
	if tgt == "" {
		tgt = first(targets)
		for _, t := range targets {
			if t != src {
				tgt = t
				break
			}
		}
	}
	preset, custom := "none", ""
	for _, p := range schedulePresets {
		if p.expr == d.Schedule && d.Schedule != "custom" {
			preset = p.label
		}
	}
	if d.Schedule != "" && preset == "none" {
		preset, custom = "custom", d.Schedule
	}
	labels := make([]string, len(schedulePresets))
	cronNotes := map[string]string{}
	for i, p := range schedulePresets {
		labels[i] = p.label
		if p.expr != "" && p.expr != "custom" {
			cronNotes[p.label] = p.expr
		}
	}
	title := "New replication"
	if w.original != "" {
		title = "Edit replication " + w.original
	}
	notes := connNotes(w.conns)
	w.basics = newForm(title,
		text("name", "Name", "").eg("sales").helpFn(func(f *form) string {
			return "replications/" + cmp.Or(f.get("name"), "NAME") + ".toml"
		}).def(d.Name).required().check(func(v string) error {
			if !config.ValidName(v) {
				return errors.New("letters, digits, _ -")
			}
			if path, err := config.ReplicationPath(w.opts.Home, v); err == nil && v != w.original {
				if _, err := os.Stat(path); err == nil {
					return errors.New("exists")
				}
			}
			return nil
		}),
		choice("source", "Source", "", sources...).def(src).notes(notes).actions(newConnRow).required(),
		choice("target", "Target", "", targets...).def(tgt).notes(notes).actions(newConnRow).required(),
		choice("mode", "Mode", "default of every stream", config.Modes...).def(cmp.Or(d.Defaults.Mode, "full-refresh")),
		choice("schedule", "Schedule", "", labels...).def(preset).notes(cronNotes).helpFn(func(f *form) string {
			return nextRunHelp(scheduleOf(f))
		}),
		text("cron", "Cron", "").def(custom).when(func(f *form) bool { return f.get("schedule") == "custom" }).required().check(func(v string) error {
			_, err := config.ParseSchedule(v)
			return err
		}).helpFn(func(f *form) string {
			if _, err := config.ParseSchedule(f.get("cron")); err == nil {
				return nextRunHelp(f.get("cron"))
			}
			return "minute hour day month weekday, UTC   */15 * * * *   @every 10m   CRON_TZ=Europe/Berlin 0 6 * * *"
		}),
		toggle("enabled", "Enabled", "off: manual runs only", d.Enabled),
	)
	w.basics.done = "next"
}

// scheduleOf is the cron expression the basics form describes.
func scheduleOf(f *form) string {
	for _, p := range schedulePresets {
		if p.label == f.get("schedule") {
			if p.expr == "custom" {
				return f.get("cron")
			}
			return p.expr
		}
	}
	return ""
}

func nextRunHelp(expr string) string {
	if expr == "" {
		return "manual runs only"
	}
	s, err := config.ParseSchedule(expr)
	if err != nil {
		return ""
	}
	return "next " + config.NextRun(s, time.Now()).Local().Format("Mon Jan 2 15:04")
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// basicsInto copies the basics form into d.
func (w *repWizard) basicsInto(d *config.ReplicationDraft) {
	f := w.basics
	d.Name, d.Source, d.Target = f.get("name"), f.get("source"), f.get("target")
	d.Defaults.Mode = f.get("mode")
	d.Enabled = f.on("enabled")
	d.Schedule = scheduleOf(f)
}

func (w *repWizard) applyBasics() {
	old := w.draft.Defaults
	w.basicsInto(&w.draft)
	w.rebase(old)
}

// defaultsFields are the replication-wide keys the defaults step edits.
var defaultsKeys = []string{"primary_key", "update_key", "object", "records", "paginate", "page_size", "page_param", "size_param", "cursor_field", "cursor_param"}

func (w *repWizard) buildDefaults() {
	api := w.isAPI()
	def := w.draft.Defaults
	fields := fieldSubset(streamFields(def, nil, api), defaultsKeys...)
	if !api {
		for i, fl := range fields {
			switch fl.Key {
			case "primary_key":
				fields[i] = multi("primary_key", "Primary key", "incremental: upsert on it", nil, def.PrimaryKey).optsFn(w.seenCols(false)).notes(w.seenNotes())
			case "update_key":
				fields[i] = combo("update_key", "Update key", "incremental: rows above the target's max", "").empty("none").def(def.UpdateKey).optsFn(w.seenCols(true)).notes(w.seenNotes())
			}
		}
		fields = append(fields, multi("exclude", "Exclude", "left out of table patterns; type a pattern: audit.*", nil, w.draft.Exclude).optsFn(func(*form) []string {
			return w.srcTables
		}))
	}
	w.defaults = newForm("Defaults", fields...)
	w.defaults.done = "next"
}

// seenCols lists the columns of the source tables read so far, for the defaults' keys.
func (w *repWizard) seenCols(update bool) func(*form) []string {
	return func(*form) []string {
		seen := map[string]bool{}
		var out []string
		if update {
			out = append(out, "")
		}
		for _, cols := range w.colsSeen {
			for _, c := range cols {
				ok := c.Kind != db.JSON && c.Kind != db.Bytes
				if update {
					ok = ok && c.Kind != db.Bool
				}
				if ok && !seen[c.Name] {
					seen[c.Name] = true
					out = append(out, c.Name)
				}
			}
		}
		slices.Sort(out)
		return out
	}
}

func (w *repWizard) seenNotes() map[string]string {
	out := map[string]string{}
	for _, cols := range w.colsSeen {
		for _, c := range cols {
			out[c.Name] = c.Kind.String()
		}
	}
	return out
}

// discoverCols reads the columns of the draft's table streams once, for the defaults.
func (w *repWizard) discoverCols() tea.Cmd {
	if w.colsAsked || w.isAPI() {
		return nil
	}
	c, err := w.conns.Get(w.draft.Source)
	if err != nil || missingFile(c) != "" {
		return nil
	}
	var tables []string
	for _, sd := range w.draft.Streams {
		if sd.Stream.SQL == "" && !strings.ContainsAny(sd.Name, "*?[") && w.colsSeen[sd.Name] == nil && len(tables) < 30 {
			tables = append(tables, sd.Name)
		}
	}
	if len(tables) == 0 {
		return nil
	}
	w.colsAsked = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out := map[string][]db.Column{}
		d, err := db.Open(ctx, c.DB())
		if err != nil {
			return tableColsMsg{Cols: out}
		}
		defer d.Close()
		for _, t := range tables {
			if cols, err := d.Columns(ctx, db.ParseTable(t)); err == nil && len(cols) > 0 {
				out[t] = cols
			}
		}
		return tableColsMsg{Cols: out}
	}
}

func (w *repWizard) defaultsInto(d *config.ReplicationDraft) {
	if w.defaults == nil {
		return
	}
	d.Defaults = streamFromFields(w.defaults, d.Defaults)
	if fl := w.defaults.field("exclude"); fl != nil {
		if fl.Kind == kindMulti {
			d.Exclude = w.defaults.picked("exclude")
		} else {
			d.Exclude = splitList(w.defaults.get("exclude"))
		}
	}
}

func (w *repWizard) applyDefaults() {
	old := w.draft.Defaults
	w.defaultsInto(&w.draft)
	w.rebase(old)
}

// rebase moves the streams made in this session along with changed defaults: a value
// equal to the old default takes the new one.
func (w *repWizard) rebase(old config.Stream) {
	nd := w.draft.Defaults
	for i := range w.draft.Streams {
		sd := &w.draft.Streams[i]
		if sd.Keys != nil {
			continue // from the file: the writer compares with the defaults itself
		}
		s := &sd.Stream
		if s.Mode == old.Mode {
			s.Mode = nd.Mode
		}
		if slices.Equal(s.PrimaryKey, old.PrimaryKey) {
			s.PrimaryKey = nd.PrimaryKey
		}
		if s.UpdateKey == old.UpdateKey {
			s.UpdateKey = nd.UpdateKey
		}
		if s.Object == old.Object {
			s.Object = nd.Object
		}
		if s.Records == old.Records {
			s.Records = nd.Records
		}
		if s.Paginate == old.Paginate {
			s.Paginate = nd.Paginate
		}
		if s.PageSize == old.PageSize {
			s.PageSize = nd.PageSize
		}
		if s.CursorField == old.CursorField {
			s.CursorField = nd.CursorField
		}
	}
}

func (w *repWizard) title() string {
	switch w.entry {
	case entryAdd:
		return "add to " + w.original
	case entryEdit:
		if sd, ok := at(w.draft.Streams, w.cur); ok {
			return "edit " + w.original + " › " + sd.Name
		}
	}
	if w.original != "" {
		return "edit " + w.original
	}
	return "new replication"
}

func (w *repWizard) help() string {
	l, r := w.footer()
	return l + "  " + r
}

func (w *repWizard) footer() (string, string) {
	save := "save"
	switch {
	case w.ask != nil:
		return sWarn.Render(txt.Clean(w.ask.text)), helpLine("esc", "back")
	case w.child != nil:
		return w.child.footer()
	case w.explore != nil:
		return w.explore.footer()
	case w.pick != nil:
		return helpLine(w.pick.hints()...), helpLine("esc", "cancel")
	case w.stream != nil:
		keys := append(w.stream.hints(), "ctrl+s", save)
		if w.isAPI() {
			keys = append(keys, "ctrl+g", "sample")
		}
		return helpLine(keys...), helpLine("esc", escWord(w.stream, "cancel"))
	case w.colsFor != "":
		return "", helpLine("esc", "cancel")
	}
	jump := []string{}
	if w.entry == entryFull {
		jump = []string{"f2/f3", "step", "1-4", "step"}
	}
	switch w.step {
	case stepBasics:
		return helpLine(append(w.basics.hints(), "ctrl+s", save, "f3", "next step")...), helpLine("esc", escWord(w.basics, "cancel"))
	case stepDefaults:
		return helpLine(append(w.defaults.hints(), "ctrl+s", save, "f2/f3", "step")...), helpLine("esc", escWord(w.defaults, "back"))
	case stepStreams:
		add := "add tables"
		if w.isAPI() {
			add = "add endpoint"
		}
		keys := []string{"a", add}
		if len(w.draft.Streams) > 0 {
			keys = append(keys, "enter", "data, sql, steps", "E", "settings", "e", "on/off", "d", "delete")
		}
		return helpLine(append(append(keys, "ctrl+s", save), jump...)...), helpLine("esc", "back")
	}
	return helpLine(append([]string{"ctrl+s", "save", "↑/↓ pgup/pgdn", "scroll"}, jump...)...), helpLine("esc", "back")
}

func (w *repWizard) typing() bool {
	switch {
	case w.ask != nil:
		return false
	case w.child != nil:
		return w.child.typing()
	case w.explore != nil:
		return w.explore.typing()
	case w.pick != nil:
		return true
	case w.stream != nil:
		return w.stream.typing()
	}
	switch w.step {
	case stepBasics:
		return w.basics.typing()
	case stepDefaults:
		return w.defaults.typing()
	}
	return false
}

func (w *repWizard) dirty() bool {
	return w.touched || w.basics.changed() || (w.defaults != nil && w.defaults.changed()) ||
		(w.stream != nil && w.stream.changed()) || (w.pick != nil && w.pick.changed()) || w.child != nil || w.explore != nil
}

func (w *repWizard) keyHelp() (string, [][2]string) {
	rows := [][2]string{}
	switch {
	case w.pick != nil:
		rows = append(rows, [2]string{"type", "filter"}, [2]string{"new", "typed value"},
			[2]string{"space", "tick"}, [2]string{"ctrl+a / ctrl+n", "tick all / none shown"},
			[2]string{"schema.*", "every table of the schema, new ones too"},
			[2]string{"enter", "add the ticked tables"}, [2]string{"esc", "cancel"})
		return "tables", rows
	case w.stream != nil:
		rows := [][2]string{{"ctrl+s", "save the stream"}, {"esc", "cancel"}}
		if w.isAPI() {
			rows = append(rows, [2]string{"ctrl+g", "fetch a page: the key fields list its columns"})
		}
		return "stream", rows
	}
	if w.entry == entryFull {
		rows = append(rows, [2]string{"enter", "next field, next step on the last"}, [2]string{"ctrl+s", "review, then save"},
			[2]string{"f2 / f3", "previous / next step"}, [2]string{"1 2 3 4", "step, when not typing"}, [2]string{"esc", "previous step; cancel on the first"})
	}
	if w.step == stepStreams {
		rows = append(rows, [2]string{"a", "add tables or an endpoint"}, [2]string{"enter", "the stream on its data: sql, columns, steps; ctrl+s saves into the draft"}, [2]string{"E", "settings: object, mode, keys, where, limit"},
			[2]string{"e", "stream on/off"}, [2]string{"d", "delete the stream (y/n)"})
	}
	if w.step == stepReview {
		rows = append(rows, [2]string{"+ / -", "lines added / removed"}, [2]string{"ctrl+s", "write the file"})
	}
	return "replication", rows
}

func (w *repWizard) result() statusMsg { return w.done }
func (w *repWizard) saved() string     { return w.savedName }

func (w *repWizard) start() tea.Cmd {
	switch w.entry {
	case entryAdd:
		w.step = stepStreams
		cmd := w.enterStreams()
		return tea.Batch(cmd, w.add())
	case entryEdit:
		w.step = stepStreams
		cmd := w.enterStreams()
		return tea.Batch(cmd, w.edit(w.cur))
	}
	return w.basics.focus()
}

// enterStreams reads the source's and target's tables for the lists, once.
func (w *repWizard) enterStreams() tea.Cmd {
	var cmds []tea.Cmd
	if !w.isAPI() && w.srcTables == nil && !w.srcLoading {
		cmds = append(cmds, w.discover("source", w.draft.Source))
	}
	if w.tgtTables == nil && !w.tgtLoading {
		cmds = append(cmds, w.discover("target", w.draft.Target))
	}
	return tea.Batch(cmds...)
}

func (w *repWizard) discover(role, name string) tea.Cmd {
	c, err := w.conns.Get(name)
	if err != nil {
		if role == "source" {
			w.srcErr = err.Error()
		}
		return nil
	}
	if role == "source" {
		w.srcLoading, w.srcErr = true, ""
	} else {
		w.tgtLoading = true
	}
	return discoverTables(c, role)
}

func (w *repWizard) update(msg tea.Msg) (wizResult, tea.Cmd) {
	if w.child != nil {
		return w.childUpdate(msg)
	}
	if w.explore != nil {
		return w.exploreUpdate(msg)
	}
	switch msg := msg.(type) {
	case discoverMsg:
		if msg.For == "target" {
			w.tgtLoading = false
			if msg.Err == nil {
				w.tgtTables = msg.Tables
				w.refreshLists()
			}
			return wizContinue, nil
		}
		w.srcLoading = false
		if msg.Err != nil {
			w.srcErr, w.wantPick = txt.FirstLine(msg.Err.Error()), false
			return wizContinue, nil
		}
		w.srcTables = msg.Tables
		w.refreshLists()
		if w.wantPick {
			w.wantPick = false
			w.openPicker()
		}
		return wizContinue, nil
	case columnsMsg:
		if w.colsFor == "" || msg.Stream != w.colsFor || w.editing < 0 || w.editing >= len(w.draft.Streams) {
			return wizContinue, nil
		}
		w.colsFor, w.colsErr = "", ""
		if msg.Err != nil {
			w.colsErr = txt.FirstLine(msg.Err.Error())
		}
		sd := w.draft.Streams[w.editing]
		if len(sd.Stream.PrimaryKey) == 0 && sd.Keys == nil && len(msg.PK) > 0 {
			sd.Stream.PrimaryKey = msg.PK // added in this session: the source's key
		}
		w.stream = w.streamFormKinds(sd, msg.Cols, msg.Kinds)
		return wizContinue, w.stream.focus()
	case tableColsMsg:
		if w.colsSeen == nil {
			w.colsSeen = map[string][]db.Column{}
		}
		maps.Copy(w.colsSeen, msg.Cols)
		return wizContinue, nil
	case sampleMsg:
		if !w.sampling || w.stream == nil {
			return wizContinue, nil
		}
		w.sampling = false
		if msg.Err != nil {
			w.stream.err = "sample: " + txt.FirstLine(msg.Err.Error())
			return wizContinue, nil
		}
		sd, _ := w.formStream()
		names := make([]string, len(msg.Cols))
		kinds := map[string]db.Kind{}
		for i, c := range msg.Cols {
			names[i], kinds[c.Name] = c.Name, c.Kind
		}
		focused := ""
		if w.stream.focused != nil {
			focused = w.stream.focused.Key
		}
		w.stream = w.streamFormKinds(sd, names, kinds)
		w.stream.note = fmt.Sprintf("sample: %s, %s", plural(msg.Records, "record"), plural(len(names), "column"))
		if fl := w.stream.field(focused); fl != nil {
			w.stream.focused = fl
		}
		return wizContinue, w.stream.focus()
	case tea.KeyMsg:
		return w.key(msg)
	}
	return wizContinue, nil
}

// refreshLists puts tables that arrived into the open stream form.
func (w *repWizard) refreshLists() {
	if w.stream == nil {
		return
	}
	if fl := w.stream.field("object"); fl != nil && fl.Kind == kindSelect {
		fl.Options = append([]string{""}, w.tgtTables...)
	}
	if fl := w.stream.field("name"); fl != nil && fl.Kind == kindSelect {
		fl.Options = w.tableOptions()
	}
}

// exploreUpdate runs the explorer on a draft stream; its saves land in the draft.
func (w *repWizard) exploreUpdate(msg tea.Msg) (wizResult, tea.Cmd) {
	res, cmd := w.explore.update(msg)
	if res == wizContinue {
		return wizContinue, cmd
	}
	if w.explore.draftSaved {
		w.touched = true
	}
	w.explore = nil
	return wizContinue, nil
}

// exploreStream opens draft stream i in the explorer, bound to the draft.
func (w *repWizard) exploreStream(i int) tea.Cmd {
	sd, ok := at(w.draft.Streams, i)
	if !ok {
		return nil
	}
	e, err := newDraftStreamExplorer(w.opts, &w.draft, sd)
	if err != nil {
		w.err = err.Error()
		return nil
	}
	e.tables = w.srcTables // listed for the picker already; R lists again
	w.explore, w.err = e, ""
	return e.start()
}

func (w *repWizard) childUpdate(msg tea.Msg) (wizResult, tea.Cmd) {
	res, cmd := w.child.update(msg)
	switch res {
	case wizDone:
		name := w.child.saved()
		w.child = nil
		if conns, err := config.LoadConnections(w.opts.Connections); err == nil {
			w.conns = conns
		}
		f := w.basics
		f.field("source").Options = w.connNames(true)
		f.field("target").Options = w.connNames(false)
		notes := connNotes(w.conns)
		f.field("source").Notes, f.field("target").Notes = notes, notes
		if fl := f.field(w.childField); fl != nil && slices.Contains(fl.Options, name) {
			fl.Value = name
		}
		return wizContinue, f.focus()
	case wizCancel:
		w.child = nil
		return wizContinue, w.basics.focus()
	}
	return wizContinue, cmd
}

func (w *repWizard) key(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	if w.ask != nil {
		ask := w.ask
		res, cmd, ok := ask.key(msg)
		if ok {
			w.ask = nil
		}
		return res, cmd
	}
	switch {
	case w.pick != nil:
		return w.keyPick(msg)
	case w.stream != nil:
		return w.keyStream(msg)
	case w.colsFor != "":
		if msg.String() == "esc" {
			w.colsFor = ""
			if w.entry == entryEdit {
				return wizCancel, nil
			}
		}
		return wizContinue, nil
	}
	if k := msg.String(); w.entry == entryFull && len(k) == 1 && k >= "1" && k <= "4" && !w.typing() {
		return w.jump(repStep(k[0] - '1'))
	}
	if k := msg.String(); w.entry == entryFull && w.child == nil && w.explore == nil && w.pick == nil && w.ask == nil && w.stream == nil && (k == "f2" || k == "f3") {
		if k == "f2" {
			return w.jump(max(w.step-1, stepBasics))
		}
		return w.jump(min(w.step+1, stepReview))
	}
	switch w.step {
	case stepBasics:
		if msg.String() == "ctrl+s" {
			w.basics.commit()
			if w.basics.takeAction() != "" || !w.basics.validate() {
				return wizContinue, w.basics.focus()
			}
			w.applyBasics()
			w.defaultsInto(&w.draft)
			return wizContinue, w.toReview()
		}
		res, cmd := w.basics.update(msg)
		if a := w.basics.takeAction(); a != "" {
			field, _, _ := strings.Cut(a, ":")
			cw, err := newConnWizard(w.opts, "")
			if err != nil {
				w.err = err.Error()
				return wizContinue, nil
			}
			w.child, w.childField = cw, field
			return wizContinue, cw.start()
		}
		switch res {
		case formCancel:
			return wizCancel, nil
		case formSubmit:
			w.applyBasics()
			return w.toDefaults()
		}
		return wizContinue, cmd
	case stepDefaults:
		if msg.String() == "ctrl+s" {
			w.defaults.commit()
			if !w.defaults.validate() {
				return wizContinue, w.defaults.focus()
			}
			w.applyDefaults()
			return wizContinue, w.toReview()
		}
		res, cmd := w.defaults.update(msg)
		switch res {
		case formCancel:
			w.applyDefaults()
			w.step = stepBasics
			return wizContinue, w.basics.focus()
		case formSubmit:
			w.applyDefaults()
			w.step = stepStreams
			return wizContinue, w.enterStreams()
		}
		return wizContinue, cmd
	case stepStreams:
		return w.keyStreams(msg)
	case stepReview:
		switch msg.String() {
		case "esc", "backspace":
			w.step = stepStreams
			if w.entry == entryAdd && w.beforePick != nil {
				w.draft = *w.beforePick
				w.pick, w.lastPick, w.beforePick = w.lastPick, nil, nil
				return wizContinue, w.pick.filter.Focus()
			}
			if w.entry != entryFull {
				return wizCancel, nil
			}
			return wizContinue, nil
		case "ctrl+s":
			if w.reviewErr != "" {
				return wizContinue, nil
			}
			if err := w.save(w.draft); err != nil {
				w.reviewErr = err.Error()
				return wizContinue, nil
			}
			return wizDone, nil
		}
		var cmd tea.Cmd
		w.review, cmd = w.review.Update(msg)
		return wizContinue, cmd
	}
	return wizContinue, nil
}

func (w *repWizard) keyStreams(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	n := len(w.draft.Streams)
	switch msg.String() {
	case "esc", "backspace":
		return w.toDefaults()
	case "up", "k":
		w.cur = max(w.cur-1, 0)
	case "down", "j":
		w.cur = min(w.cur+1, max(n-1, 0))
	case "pgup":
		w.cur = max(w.cur-10, 0)
	case "pgdown":
		w.cur = min(w.cur+10, max(n-1, 0))
	case "g", "home":
		w.cur = 0
	case "G", "end":
		w.cur = max(n-1, 0)
	case "a":
		return wizContinue, w.add()
	case "enter":
		if w.cur < n {
			return wizContinue, w.exploreStream(w.cur)
		}
	case "E":
		if w.cur < n {
			return wizContinue, w.edit(w.cur)
		}
	case "e":
		if w.cur < n {
			w.draft.Streams[w.cur].Stream.Disabled = !w.draft.Streams[w.cur].Stream.Disabled
			w.touched = true
		}
	case "d", "delete":
		if w.cur < n {
			i := w.cur
			w.ask = &wizAsk{text: "delete stream " + w.draft.Streams[i].Name + "? y/n", yes: func() (wizResult, tea.Cmd) {
				w.draft.Streams = slices.Delete(w.draft.Streams, i, i+1)
				w.cur = min(w.cur, max(len(w.draft.Streams)-1, 0))
				w.touched = true
				return wizContinue, nil
			}}
		}
	case "ctrl+s":
		return wizContinue, w.toReview()
	}
	return wizContinue, nil
}

// toDefaults opens the defaults step, built for the source's kind, and reads what its
// lists offer.
func (w *repWizard) toDefaults() (wizResult, tea.Cmd) {
	if w.defaults == nil || w.defaults.field("records") != nil != w.isAPI() {
		w.buildDefaults()
	}
	w.step = stepDefaults
	return wizContinue, tea.Batch(w.defaults.focus(), w.enterStreams(), w.discoverCols())
}

// jump goes to a step of the full wizard, keeping what the current step holds. A step
// that does not validate stays.
func (w *repWizard) jump(to repStep) (wizResult, tea.Cmd) {
	if to == w.step {
		return wizContinue, nil
	}
	switch w.step {
	case stepBasics:
		w.basics.commit()
		if w.basics.takeAction() != "" || !w.basics.validate() {
			return wizContinue, w.basics.focus()
		}
		w.applyBasics()
	case stepDefaults:
		w.defaults.commit()
		if !w.defaults.validate() {
			return wizContinue, w.defaults.focus()
		}
		w.applyDefaults()
	}
	switch to {
	case stepBasics:
		w.step = stepBasics
		return wizContinue, w.basics.focus()
	case stepDefaults:
		return w.toDefaults()
	case stepStreams:
		if w.defaults == nil {
			w.buildDefaults()
		}
		w.step = stepStreams
		return wizContinue, w.enterStreams()
	}
	return wizContinue, w.toReview()
}

// add opens the endpoint form for an API source, else the table list.
func (w *repWizard) add() tea.Cmd {
	w.err = ""
	if w.isAPI() {
		w.editing = -1
		w.stream = w.streamForm(config.StreamDraft{Stream: w.draft.Defaults}, nil)
		return w.stream.focus()
	}
	if w.srcTables != nil {
		w.openPicker()
		return w.pick.filter.Focus()
	}
	w.wantPick = true
	if !w.srcLoading {
		return w.discover("source", w.draft.Source)
	}
	return nil
}

// edit opens a stream's form, after reading the table's columns for a table stream.
func (w *repWizard) edit(i int) tea.Cmd {
	sd, ok := at(w.draft.Streams, i)
	if !ok {
		return nil
	}
	w.editing, w.err, w.colsErr = i, "", ""
	if w.isAPI() || sd.Stream.SQL != "" || strings.ContainsAny(sd.Name, "*?[") {
		w.stream = w.streamForm(sd, nil)
		return w.stream.focus()
	}
	c, err := w.conns.Get(w.draft.Source)
	if err == nil && missingFile(c) != "" {
		err = fmt.Errorf("conn %s: no such file: %s", c.Name, missingFile(c))
	}
	if err != nil {
		w.colsErr = err.Error()
		w.stream = w.streamForm(sd, nil)
		return w.stream.focus()
	}
	w.colsFor = sd.Name
	table := sd.Name
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		d, err := db.Open(ctx, c.DB())
		if err != nil {
			return columnsMsg{Stream: table, Err: err}
		}
		defer d.Close()
		cols, err := d.Columns(ctx, db.ParseTable(table))
		if err == nil && len(cols) == 0 {
			err = fmt.Errorf("%s: no such table", table)
		}
		if err != nil {
			return columnsMsg{Stream: table, Err: err}
		}
		names := make([]string, len(cols))
		kinds := map[string]db.Kind{}
		for i, col := range cols {
			names[i], kinds[col.Name] = col.Name, col.Kind
		}
		pk, _ := d.PrimaryKey(ctx, db.ParseTable(table))
		return columnsMsg{Stream: table, Cols: names, Kinds: kinds, PK: pk}
	}
}

func (w *repWizard) keyStream(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	if msg.String() == "ctrl+g" && w.isAPI() {
		w.stream.commit()
		return wizContinue, w.sample()
	}
	res, cmd := w.stream.update(msg)
	switch res {
	case formCancel:
		if w.entry != entryFull {
			return wizCancel, nil
		}
		w.stream = nil
	case formSubmit:
		d, ok := w.withStream()
		if !ok {
			return wizContinue, w.stream.focus()
		}
		if w.entry != entryFull {
			if err := w.save(d); err != nil {
				w.stream.err = err.Error()
				return wizContinue, nil
			}
			return wizDone, nil
		}
		w.draft, w.stream, w.touched = d, nil, true
		w.cur = min(max(w.editing, 0), len(d.Streams)-1)
		if w.editing < 0 {
			w.cur = len(d.Streams) - 1
		}
	}
	return wizContinue, cmd
}

// formStream is the stream the open form describes.
func (w *repWizard) formStream() (config.StreamDraft, bool) {
	sd := config.StreamDraft{Stream: w.draft.Defaults}
	if w.editing >= 0 && w.editing < len(w.draft.Streams) {
		sd = w.draft.Streams[w.editing]
	}
	sd.Stream = streamFromFields(w.stream, sd.Stream)
	sd.Name = w.stream.get("name")
	return sd, true
}

// sample fetches the first page of the API stream in the form, for its columns.
func (w *repWizard) sample() tea.Cmd {
	sd, _ := w.formStream()
	if sd.Stream.Path == "" {
		w.stream.err = "sample: path is required"
		return nil
	}
	c, err := w.conns.Get(w.draft.Source)
	if err != nil {
		w.stream.err = err.Error()
		return nil
	}
	var parent *config.Stream
	if p := sd.Stream.ForEach; p != "" {
		i := slices.IndexFunc(w.draft.Streams, func(o config.StreamDraft) bool { return o.Name == p })
		if i < 0 {
			w.stream.err = "sample: no stream " + p
			return nil
		}
		ps := w.draft.Streams[i].Stream
		parent = &ps
	}
	w.sampling, w.stream.err, w.stream.note = true, "", "sample: fetching"
	spec := sd.Stream
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		client, err := api.NewClient(c)
		if err != nil {
			return sampleMsg{Err: err}
		}
		var recs []api.Record
		if parent != nil {
			recs, err = forEachSample(ctx, client, spec, *parent, 1)
		} else {
			recs, err = client.FetchWith(ctx, &spec, api.Options{MaxPages: 1})
		}
		if err != nil {
			return sampleMsg{Err: err}
		}
		cols, _ := engine.Tabulate(recs)
		return sampleMsg{Cols: cols, Records: len(recs)}
	}
}

// withStream returns the draft with the stream form applied, or false with the form
// showing why not.
func (w *repWizard) withStream() (config.ReplicationDraft, bool) {
	f := w.stream
	f.err = ""
	sd, _ := w.formStream()
	check := sd.Stream
	check.Mode = cmp.Or(check.Mode, w.draft.Defaults.Mode, "full-refresh")
	if err := config.ValidateStream(sd.Name, &check); err != nil {
		f.err = strings.TrimPrefix(err.Error(), "stream "+sd.Name+": ")
		return w.draft, false
	}
	d := w.draft
	d.Streams = slices.Clone(w.draft.Streams)
	if w.editing >= 0 && w.editing < len(d.Streams) {
		d.Streams[w.editing] = sd
	} else {
		d.Streams = append(d.Streams, sd)
	}
	return d, true
}

// tableOptions are the source's tables with a schema.* row before each schema's.
func (w *repWizard) tableOptions() []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range w.srcTables {
		schema := ""
		if i := strings.LastIndexByte(t, '.'); i > 0 {
			schema = t[:i]
		}
		if !seen[schema] {
			seen[schema] = true
			if schema == "" {
				out = append(out, "*")
			} else {
				out = append(out, schema+".*")
			}
		}
		out = append(out, t)
	}
	return out
}

func (w *repWizard) openPicker() {
	have := map[string]bool{}
	for _, s := range w.draft.Streams {
		have[s.Name] = true
	}
	items := w.tableOptions()
	for _, s := range w.draft.Streams { // streams named by hand stay listed
		if !slices.Contains(items, s.Name) && !w.isAPI() {
			items = append(items, s.Name)
		}
	}
	var pre []string
	for _, it := range items {
		if have[it] {
			pre = append(pre, it)
		}
	}
	p := newPicker("Add tables  "+w.draft.Source, items, pre)
	p.live, p.typing, p.free = true, true, true
	p.filter.Focus()
	for _, it := range items {
		switch {
		case have[it]:
			p.locked[it] = true
			p.note[it] = "in replication"
		case strings.HasSuffix(it, "*"):
			p.note[it] = "every table, new ones too"
		}
	}
	for name, from := range w.coveredBy() {
		if !have[name] {
			p.note[name] = from
		}
	}
	w.pick = p
}

// coveredBy notes the tables a table pattern of the draft brings in ("via sales.*") or
// that exclude leaves out ("excluded").
func (w *repWizard) coveredBy() map[string]string {
	out := map[string]string{}
	text, err := config.RenderReplication(w.draft)
	if err != nil {
		return out
	}
	r, err := config.LoadReplicationText(text, "draft.toml")
	if err != nil {
		return out
	}
	for _, t := range w.srcTables {
		if r.Excluded(t) {
			out[t] = "excluded"
		}
	}
	list, _ := r.Expand(func(schema string) ([]string, error) {
		var ts []string
		for _, t := range w.srcTables {
			i := strings.LastIndexByte(t, '.')
			if (schema == "" && i < 0) || (i > 0 && t[:i] == schema) {
				ts = append(ts, t)
			}
		}
		return ts, nil
	})
	for _, s := range list {
		if s.From != "" {
			out[s.Name] = "via " + s.From
		}
	}
	return out
}

func (w *repWizard) keyPick(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	if msg.String() == "ctrl+s" {
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	}
	res, cmd := w.pick.update(msg)
	switch res {
	case formCancel:
		if w.entry == entryAdd {
			return wizCancel, nil
		}
		w.pick = nil
	case formSubmit:
		before := w.draft
		before.Streams = slices.Clone(w.draft.Streams)
		added := 0
		for _, it := range w.pick.chosen() {
			if !w.pick.locked[it] {
				w.draft.Streams = append(w.draft.Streams, config.StreamDraft{Name: it, Stream: w.draft.Defaults})
				added++
			}
		}
		if added > 0 {
			w.touched = true
			w.cur = len(w.draft.Streams) - 1
		}
		if w.entry == entryAdd {
			if added == 0 {
				return wizCancel, nil
			}
			w.lastPick, w.beforePick, w.pick = w.pick, &before, nil
			return wizContinue, w.toReview()
		}
		w.pick = nil
	}
	return wizContinue, cmd
}

// streamForm edits one stream, starting from its own values.
func (w *repWizard) streamForm(sd config.StreamDraft, cols []string) *form {
	var kinds map[string]db.Kind
	if seen := w.colsSeen[sd.Name]; seen != nil && cols != nil {
		kinds = map[string]db.Kind{}
		for _, c := range seen {
			kinds[c.Name] = c.Kind
		}
	}
	return w.streamFormKinds(sd, cols, kinds)
}

func (w *repWizard) streamFormKinds(sd config.StreamDraft, cols []string, kinds map[string]db.Kind) *form {
	if cols != nil && kinds != nil && !w.isAPI() {
		if w.colsSeen == nil {
			w.colsSeen = map[string][]db.Column{}
		}
		seen := make([]db.Column, len(cols))
		for i, c := range cols {
			seen[i] = db.Column{Name: c, Kind: kinds[c]}
		}
		w.colsSeen[sd.Name] = seen
	}
	api := w.isAPI()
	fs := streamFieldsOf(sd.Stream, cols, kinds, api)
	var name *field
	if api {
		name = text("name", "Name", "the target table unless object is set").eg("users").def(sd.Name)
	} else {
		name = combo("name", "Name", "schema.*: every table   any name with sql", w.tableOptions()...).def(sd.Name)
	}
	name.required().check(func(v string) error {
		for i, o := range w.draft.Streams {
			if o.Name == v && i != w.editing {
				return errors.New("exists")
			}
		}
		return nil
	})
	others := []string{""}
	for _, o := range w.draft.Streams {
		if o.Name != sd.Name {
			others = append(others, o.Name)
		}
	}
	for _, fl := range fs {
		switch fl.Key {
		case "object":
			fl.Options = append([]string{""}, w.tgtTables...)
		case "for_each":
			fl.Options = others
		}
	}
	title := "New stream"
	if sd.Name != "" {
		title = "Stream " + sd.Name
	}
	f := newForm(title, append([]*field{name}, fs...)...)
	if w.colsErr != "" {
		f.note = "columns: " + w.colsErr
	}
	if w.entry != entryFull {
		f.done = "save"
	}
	return f
}

// streamFields lists the editable keys of a stream, filled from s. With cols known the
// column keys are lists of them; api picks the endpoint keys over the table ones. The
// replication wizard and the explorer build their stream forms from it; streamFromFields
// reads a form back.
func streamFields(s config.Stream, cols []string, api bool) []*field {
	return streamFieldsOf(s, cols, nil, api)
}

// keyKinds are the column kinds an update key can compare; nil kinds: any column.
func keyKind(kinds map[string]db.Kind, col string) bool {
	k, ok := kinds[col]
	return !ok || (k != db.Bool && k != db.JSON && k != db.Bytes)
}

// loadedCols are the columns the stream loads: the ticked ones of its column list, else all.
func loadedCols(f *form, all []string) []string {
	for _, key := range []string{"select", "columns"} {
		if fl := f.field(key); fl != nil && fl.Kind == kindMulti {
			if p := f.picked(key); len(p) > 0 {
				return p
			}
		}
	}
	return all
}

// kindNotes labels columns with their kind in lists.
func kindNotes(kinds map[string]db.Kind) map[string]string {
	if len(kinds) == 0 {
		return nil
	}
	out := map[string]string{}
	for c, k := range kinds {
		out[c] = k.String()
	}
	return out
}

// streamFieldsOf is streamFields with the kinds of the columns, so key lists offer only
// loaded columns an update key can compare.
func streamFieldsOf(s config.Stream, cols []string, kinds map[string]db.Kind, api bool) []*field {
	in := func(key string, vals ...string) func(*form) bool {
		return func(f *form) bool { return slices.Contains(vals, f.get(key)) }
	}
	paged := in("paginate", "page", "offset", "cursor")
	var fs []*field
	if api {
		fs = append(fs,
			text("path", "Path", "{parent.field} with for_each").eg("/users").def(s.Path).required(),
			choice("method", "Method", "", "GET", "POST", "PUT", "PATCH").def(cmp.Or(strings.ToUpper(s.Method), "GET")),
			text("body", "Body", "JSON").eg(`{"status": "paid"}`).def(s.Body).when(func(f *form) bool { return f.get("method") != "GET" }),
			text("query", "Query", "").eg("status = paid, sort = id").def(pairs(s.Query)).check(checkPairs),
			text("records", "Records", "JMESPath; blank: the response").eg("data[]   results[]").def(s.Records),
			choice("paginate", "Paginate", "", "", "page", "offset", "cursor", "link").empty("none").def(s.Paginate),
			text("page_size", "Page size", "").eg("100").def(zeroBlank(s.PageSize)).check(isInt).when(paged),
			text("page_param", "Page param", "default: page or offset").eg("page").def(s.PageParam).when(in("paginate", "page", "offset")),
			text("size_param", "Size param", "default: limit").eg("per_page").def(s.SizeParam).when(paged),
			text("cursor_field", "Cursor field", "JMESPath; a URL is followed").eg("next   meta.next_cursor").def(s.CursorField).when(in("paginate", "cursor")).required(),
			text("cursor_param", "Cursor param", "default: cursor").eg("after").def(s.CursorParam).when(in("paginate", "cursor")),
			text("total_field", "Total field", "JMESPath; paging stops at it").eg("meta.total").def(s.TotalField).when(in("paginate", "page", "offset")),
			text("stop_when", "Stop when", "JMESPath; true stops paging").eg("length(data) == `0`").def(s.StopWhen).when(func(f *form) bool { return f.get("paginate") != "" }),
			choice("params_in", "Params in", "", "", "body").empty("query").def(s.ParamsIn).when(paged),
			text("skip_on", "Skip on", "statuses that end the fetch, no error").eg("404").def(ints(s.SkipOn)).check(checkInts),
			choice("for_each", "For each", "parent stream: one request per record, {parent.field}", cmp.Or(s.ForEach, "")).empty("none").def(s.ForEach),
			text("add_columns", "Add columns", "").def(pairs(s.AddColumns)).check(checkPairs).when(func(f *form) bool { return f.get("for_each") != "" }).helpFn(func(f *form) string {
				if f.get("add_columns") != "" {
					return ""
				}
				p := f.get("for_each")
				return strings.TrimSuffix(p, "s") + "_id = {" + p + ".id}"
			}),
		)
		if s.ForEach != "" {
			fs[len(fs)-2].Options = []string{"", s.ForEach}
		}
	} else {
		// sql, columns and steps are shaped in explore, on the data
		fs = append(fs, text("where", "Where", "source SQL").eg("status = 'paid'").def(s.Where))
	}
	fs = append(fs,
		combo("object", "Object", "target table; {stream_table} {stream_schema} {stream_name}", "").empty("stream name").eg("analytics.{stream_table}").def(s.Object),
		choice("mode", "Mode", "", config.Modes...).def(cmp.Or(s.Mode, "full-refresh")),
	)
	if cols != nil {
		pkOpts := func(f *form) []string {
			var out []string
			for _, c := range loadedCols(f, cols) {
				if k, ok := kinds[c]; !ok || (k != db.JSON && k != db.Bytes) {
					out = append(out, c)
				}
			}
			return out
		}
		ukOpts := func(f *form) []string {
			out := []string{""}
			for _, c := range loadedCols(f, cols) {
				if keyKind(kinds, c) {
					out = append(out, c)
				}
			}
			return out
		}
		fs = append(fs,
			multi("primary_key", "Primary key", "incremental: upsert on it", cols, s.PrimaryKey).fixed().optsFn(pkOpts).notes(kindNotes(kinds)),
			choice("update_key", "Update key", "incremental: rows above the target's max", append([]string{""}, cols...)...).empty("none").def(s.UpdateKey).optsFn(ukOpts).notes(kindNotes(kinds)),
		)
	} else {
		fs = append(fs,
			text("primary_key", "Primary key", "incremental: upsert on it").eg("id   order_id, line").def(strings.Join(s.PrimaryKey, ", ")),
			text("update_key", "Update key", "incremental: rows above the target's max").eg("updated_at").def(s.UpdateKey),
		)
	}
	if api {
		inc := in("mode", "incremental")
		fs = append(fs,
			text("since_param", "Since param", "gets the watermark").eg("updated_after").def(s.SinceParam).when(inc),
			choice("since_format", "Since format", "", "rfc3339", "date", "unix", "unix_ms", "raw").def(cmp.Or(s.SinceFormat, "rfc3339")).when(func(f *form) bool { return inc(f) && f.get("since_param") != "" }),
			choice("sorted", "Sorted", "", "", "desc", "asc").empty("unknown").def(s.Sorted).when(inc),
		)
	}
	fs = append(fs,
		text("range", "Range", "on the update key").eg("2026-01-01,2026-02-01   1,1000").def(s.Range).when(in("mode", "backfill")).required(),
		text("limit", "Limit", "blank: all rows").eg("1000").def(zeroBlank(s.Limit)).check(isInt),
	)
	if !api {
		fs = append(fs, text("query_timeout", "Query timeout", "").eg("30s   5m").def(s.QueryTimeout).check(func(v string) error {
			if d, err := time.ParseDuration(v); err != nil || d <= 0 {
				return errors.New("a duration: 30s, 5m")
			}
			return nil
		}))
	}
	return append(fs, toggle("disabled", "Enabled", "off: skipped on every run", s.Disabled).invert())
}

// fieldSubset keeps the fields with the given keys, in the order of fields.
func fieldSubset(fields []*field, keys ...string) []*field {
	var out []*field
	for _, fl := range fields {
		if slices.Contains(keys, fl.Key) {
			out = append(out, fl)
		}
	}
	return out
}

// streamFromFields reads a form built from streamFields back into a stream. Keys the
// form does not have keep base's value; a field hidden since the form opened clears its
// key; a field hidden from the start keeps base's.
func streamFromFields(f *form, base config.Stream) config.Stream {
	s := base
	state := func(key string) (has, read bool, fl *field) {
		fl = f.field(key)
		switch {
		case fl == nil:
			return false, false, nil
		case f.shown(key):
			return true, true, fl
		case fl.shown0:
			return true, false, fl // hidden by a change: cleared
		}
		return false, false, fl
	}
	str := func(key string, dst *string, dflt string) {
		has, read, _ := state(key)
		switch {
		case read:
			v := f.get(key)
			if v == dflt && *dst == "" {
				v = "" // the default stays unwritten
			}
			*dst = v
		case has:
			*dst = ""
		}
	}
	num := func(key string, dst *int) {
		has, read, _ := state(key)
		switch {
		case read:
			*dst, _ = strconv.Atoi(f.get(key))
		case has:
			*dst = 0
		}
	}
	list := func(key string, dst *config.Names) {
		has, read, fl := state(key)
		switch {
		case read && fl.Kind == kindMulti:
			if f.fieldChanged(key) {
				*dst = f.picked(key)
			}
		case read:
			*dst = splitList(f.get(key))
		case has:
			*dst = nil
		}
	}
	kv := func(key string, dst *map[string]string) {
		has, read, _ := state(key)
		switch {
		case read:
			*dst, _ = parsePairs(f.get(key))
		case has:
			*dst = nil
		}
	}
	for key, dst := range map[string]*string{
		"path": &s.Path, "body": &s.Body, "records": &s.Records, "paginate": &s.Paginate,
		"page_param": &s.PageParam, "size_param": &s.SizeParam, "cursor_field": &s.CursorField,
		"cursor_param": &s.CursorParam, "total_field": &s.TotalField, "stop_when": &s.StopWhen,
		"params_in": &s.ParamsIn, "for_each": &s.ForEach, "sql": &s.SQL, "where": &s.Where,
		"steps": &s.Steps, "object": &s.Object, "update_key": &s.UpdateKey, "since_param": &s.SinceParam,
		"sorted": &s.Sorted, "range": &s.Range, "query_timeout": &s.QueryTimeout,
	} {
		str(key, dst, "")
	}
	str("method", &s.Method, "GET")
	str("mode", &s.Mode, "full-refresh")
	str("since_format", &s.SinceFormat, "rfc3339")
	num("page_size", &s.PageSize)
	num("limit", &s.Limit)
	list("primary_key", &s.PrimaryKey)
	kv("query", &s.Query)
	kv("add_columns", &s.AddColumns)
	if has, read, fl := state("select"); read && fl.Kind == kindMulti {
		if f.fieldChanged("select") {
			s.Select = f.picked("select")
			if slices.Equal([]string(s.Select), fl.Options) {
				s.Select = nil // every column
			}
		}
	} else if read {
		s.Select = splitList(f.get("select"))
	} else if has {
		s.Select = nil
	}
	if has, read, _ := state("skip_on"); read {
		s.SkipOn, _ = parseInts(f.get("skip_on"))
	} else if has {
		s.SkipOn = nil
	}
	if _, read, _ := state("disabled"); read {
		s.Disabled = f.on("disabled")
	}
	return s
}

func checkPairs(v string) error { _, err := parsePairs(v); return err }
func checkInts(v string) error  { _, err := parseInts(v); return err }

func (w *repWizard) toReview() tea.Cmd {
	w.reviewErr, w.reviewWarn, w.changes = "", "", 0
	text, diff, err := config.PreviewWrite(w.opts.Home, w.draft)
	if err != nil {
		w.reviewErr = err.Error()
		if path, perr := config.ReplicationPath(w.opts.Home, w.draft.Name); perr == nil {
			w.reviewErr = strings.TrimPrefix(w.reviewErr, path+": ")
		}
	}
	if len(w.draft.Streams) == 0 {
		w.reviewWarn = "no streams"
	}
	var lines []string
	if w.original == "" || len(diff) == 0 {
		lines = strings.Split(strings.TrimRight(txt.CleanText(text), "\n"), "\n")
	} else {
		lines, w.changes = styleDiff(diff)
	}
	w.review = viewport.New(80, 10)
	w.review.SetContent(strings.Join(lines, "\n"))
	w.step = stepReview
	return nil
}

// styleDiff colours the lines of config.PreviewWrite and counts the changed ones.
func styleDiff(diff []string) ([]string, int) {
	out := make([]string, len(diff))
	changes := 0
	for i, l := range diff {
		l = txt.Clean(l)
		switch {
		case strings.HasPrefix(l, "+ "):
			out[i] = sOK.Render(l)
			changes++
		case strings.HasPrefix(l, "- "):
			out[i] = sErr.Render(l)
			changes++
		default:
			out[i] = sDim.Render(l)
		}
	}
	return out, changes
}

// save writes d. A rename copies the old file first so its comments stay, then removes it.
func (w *repWizard) save(d config.ReplicationDraft) error {
	renamed := w.original != "" && w.original != d.Name
	if renamed && d.From == "" {
		d.From = w.original // config patches the old file, comments and all
	}
	if _, err := config.WriteReplication(w.opts.Home, d); err != nil {
		return err
	}
	w.draft = d
	w.savedName = d.Name
	w.done = statusMsg{Text: "saved: " + d.Name + ".toml"}
	if renamed {
		if err := config.DeleteReplication(w.opts.Home, w.original); err != nil {
			w.done = statusMsg{Text: fmt.Sprintf("saved: %s.toml; %s.toml not removed: %v", d.Name, w.original, err), IsErr: true}
		}
	}
	return nil
}

func (w *repWizard) view(width, height int) []string {
	if w.child != nil {
		return w.child.view(width, height)
	}
	if w.explore != nil {
		return w.explore.view(width, height)
	}
	var head []string
	if w.entry == entryFull {
		var parts []string
		for i, n := range stepNames {
			label := fmt.Sprintf("%d %s", i+1, n)
			if repStep(i) == w.step {
				parts = append(parts, sKey.Render(label))
			} else {
				parts = append(parts, sDim.Render(label))
			}
		}
		head = []string{" " + strings.Join(parts, sDim.Render("  ·  ")), ""}
	}
	if w.err != "" {
		head = append(head, " "+sErr.Render(glyphErr+" "+txt.Clean(txt.FirstLine(w.err))), "")
	}
	h := max(height-len(head), 3)
	var body []string
	switch {
	case w.pick != nil:
		body = w.pick.view(width, h)
	case w.stream != nil:
		body = w.stream.view(width, h)
	case w.colsFor != "":
		body = []string{"", "  " + sWarn.Render("◐ columns of "+txt.Clean(w.colsFor))}
	default:
		switch w.step {
		case stepBasics:
			body = w.basics.view(width, h)
		case stepDefaults:
			body = w.defaults.view(width, h)
		case stepStreams:
			body = w.streamsView(width, h)
		case stepReview:
			body = w.reviewView(width, h)
		}
	}
	return append(head, body...)
}

func (w *repWizard) reviewView(width, height int) []string {
	title := "replications/" + w.draft.Name + ".toml"
	if w.original != "" && w.original != w.draft.Name {
		title += sDim.Render("  was " + w.original + ".toml")
	}
	out := []string{" " + sTitle.Render(txt.Clean(title)), ""}
	switch {
	case w.reviewErr != "":
		out = append(out, " "+sErr.Render(glyphErr+" "+txt.Clean(txt.FirstLine(w.reviewErr))), "")
	case w.reviewWarn != "":
		out = append(out, " "+sWarn.Render(w.reviewWarn), "")
	case w.original != "" && w.changes == 0:
		out = append(out, " "+sDim.Render("no changes"), "")
	}
	w.review.Width = max(width-2, 10)
	w.review.Height = max(height-len(out), 3)
	for _, l := range strings.Split(w.review.View(), "\n") {
		out = append(out, " "+l)
	}
	return out
}

func (w *repWizard) streamsView(width, height int) []string {
	out := []string{" " + sTitle.Render("Streams") + "  " + sDim.Render("source ") + txt.Clean(w.draft.Source) +
		sDim.Render("   target ") + txt.Clean(w.draft.Target) + sDim.Render("   mode ") + txt.Clean(cmp.Or(w.draft.Defaults.Mode, "full-refresh")), ""}
	switch {
	case w.srcLoading:
		out = append(out, "  "+sWarn.Render("◐ tables of "+txt.Clean(w.draft.Source)), "")
	case w.srcErr != "":
		out = append(out, "  "+sErr.Render(glyphErr+" tables: "+txt.Clean(w.srcErr)), "")
	}
	if len(w.draft.Streams) == 0 {
		return append(out, "  "+sDim.Render("none"))
	}
	cols := []column{{Title: "STREAM"}, {Title: "OBJECT"}, {Title: "MODE", Width: 12}, {Title: "OVERRIDES"}}
	var rows [][]string
	for _, s := range w.draft.Streams {
		name := txt.Clean(s.Name)
		if s.Stream.Disabled {
			name = sDim.Render(name + " (off)")
		}
		rows = append(rows, []string{name, sDim.Render(txt.Clean(orDash(s.Stream.Object))), txt.Clean(cmp.Or(s.Stream.Mode, w.draft.Defaults.Mode)), sDim.Render(txt.Clean(overrides(s.Stream, w.draft.Defaults)))})
	}
	lines := table(width, cols, rows, w.cur)
	rowsH := max(height-len(out)-1, 1)
	off := max(w.cur-rowsH+1, 0)
	return append(out, window(lines, off, rowsH+1, true)...)
}

// overrides summarises which keys differ from the defaults.
func overrides(s, d config.Stream) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if !slices.Equal(s.PrimaryKey, d.PrimaryKey) {
		add("pk", strings.Join(s.PrimaryKey, ","))
	}
	if s.UpdateKey != d.UpdateKey {
		add("update_key", s.UpdateKey)
	}
	if s.SQL != "" {
		parts = append(parts, "sql")
	}
	if s.Path != "" {
		add("path", s.Path)
	}
	if s.ForEach != "" {
		add("for_each", s.ForEach)
	}
	if s.Paginate != d.Paginate {
		add("paginate", s.Paginate)
	}
	if s.Where != "" {
		parts = append(parts, "where")
	}
	if s.Steps != "" {
		parts = append(parts, "steps")
	}
	if len(s.Select) > 0 {
		parts = append(parts, plural(len(s.Select), "col"))
	}
	if s.Limit > 0 {
		add("limit", strconv.Itoa(s.Limit))
	}
	return strings.Join(parts, "  ")
}

func zeroBlank(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func isInt(v string) error {
	if _, err := strconv.Atoi(v); err != nil {
		return errors.New("a whole number")
	}
	return nil
}
