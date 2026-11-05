package tui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
	"tummy/internal/txt"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
	"tummy/internal/db"
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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
		c := w.conns.Conn[n]
		if apiToo || (c.Type != "api" && !c.ReadOnly) {
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
		if c.ReadOnly {
			out[n] += "  read only"
		}
	}
	return out
}

func (w *repWizard) isAPI() bool {
	c, ok := w.conns.Conn[w.draft.Source]
	return ok && c.Type == "api"
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
		jump = []string{"tab/shift+tab", "step"}
	}
	switch w.step {
	case stepBasics:
		return helpLine(append(w.basics.hints(), "ctrl+s", save)...), helpLine("esc", escWord(w.basics, "cancel"))
	case stepDefaults:
		return helpLine(append(w.defaults.hints(), "ctrl+s", save)...), helpLine("esc", escWord(w.defaults, "back"))
	case stepStreams:
		add := "add tables"
		if w.isAPI() {
			add = "add endpoint"
		}
		keys := []string{"a", add}
		if !w.isAPI() {
			keys = append(keys, "s", "sql query")
		}
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
			[2]string{"ctrl+e", "the table's data; ctrl+s there adds it as shaped"},
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
			[2]string{"tab / shift+tab", "next / previous step"}, [2]string{"1 2 3 4", "step, when not typing"}, [2]string{"esc", "previous step; cancel on the first"})
	}
	if w.step == stepStreams {
		rows = append(rows, [2]string{"a", "add tables or an endpoint"}, [2]string{"s", "a query on the source; + adds it as a stream"}, [2]string{"enter", "the stream on its data: sql, columns, steps; ctrl+s saves into the draft"}, [2]string{"E", "settings: object, mode, keys, where, limit"},
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
	w.basics.tabSteps = w.entry == entryFull // the entry is known only after construction
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
	if k := msg.String(); w.entry == entryFull && w.child == nil && w.explore == nil && w.pick == nil && w.ask == nil && w.stream == nil && w.step >= stepStreams && (k == "tab" || k == "shift+tab") {
		if k == "shift+tab" {
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
		case formSubmit, formNext:
			w.basics.commit()
			if !w.basics.validate() {
				return wizContinue, w.basics.focus()
			}
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
		case formSubmit, formNext:
			w.defaults.commit()
			if !w.defaults.validate() {
				return wizContinue, w.defaults.focus()
			}
			w.applyDefaults()
			w.step = stepStreams
			return wizContinue, w.enterStreams()
		case formPrev:
			return w.jump(stepBasics)
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
