package tui

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/api"
	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/engine"
)

// exploreUpdate runs the explorer on a draft stream; its saves land in the draft.
func (w *repWizard) exploreUpdate(msg tea.Msg) (wizResult, tea.Cmd) {
	res, cmd := w.explore.update(msg)
	if res == wizContinue {
		return wizContinue, cmd
	}
	if w.explore.draftSaved {
		w.touched = true
		if w.pick != nil { // saved from the table list's preview: the list learns the stream
			for _, sd := range w.draft.Streams {
				w.pick.lock(sd.Name, "in replication")
			}
		}
	}
	w.explore = nil
	return wizContinue, nil
}

// exploreSQL opens the explorer on the source with the SQL editor: + adds the query as a
// stream of the draft.
func (w *repWizard) exploreSQL() tea.Cmd {
	e, err := newExplorer(w.opts, w.draft.Source)
	if err != nil {
		w.err = err.Error()
		return nil
	}
	e.bound, e.repName, e.tables, e.fromStream = &w.draft, w.draft.Name, w.srcTables, true // esc: back to the step
	e.resetPreview()
	e.page = pagePreview
	e.editSQL = true
	e.sqlInput.SetValue("")
	w.explore, w.err = e, ""
	return e.sqlInput.Focus()
}

// previewTable opens a source table from the picker in the explorer; ctrl+s there adds
// it to the draft as shaped.
func (w *repWizard) previewTable(table string) tea.Cmd {
	e, err := newDraftStreamExplorer(w.opts, &w.draft, config.StreamDraft{Name: table, Stream: w.draft.Defaults})
	if err != nil {
		w.err = err.Error()
		return nil
	}
	e.tables = w.srcTables
	w.explore, w.err = e, ""
	return e.start()
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

func (w *repWizard) keyStreams(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	n := len(w.draft.Streams)
	switch msg.String() {
	case "esc":
		return wizCancel, nil
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
	case "s":
		if !w.isAPI() {
			return wizContinue, w.exploreSQL()
		}
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
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
		var sample forEachResult
		if parent != nil {
			sample, err = forEachRun(ctx, client, spec, *parent, 1, nil)
			recs = sample.recs
		} else {
			recs, err = client.FetchWith(ctx, &spec, api.Options{MaxPages: 1})
		}
		if err != nil {
			return sampleMsg{Err: err}
		}
		cols, _ := engine.Tabulate(recs)
		return sampleMsg{Cols: cols, Records: len(recs), Parent: spec.ForEach, ParentCols: sample.parentCols}
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
	if msg.String() == "ctrl+e" && !w.isAPI() {
		if item, ok := w.pick.current(); ok && !strings.ContainsAny(item, "*?[") {
			return wizContinue, w.previewTable(item)
		}
		return wizContinue, nil
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
			if added == 0 && reflect.DeepEqual(w.draft, w.orig) {
				return wizCancel, nil
			}
			w.lastPick, w.beforePick, w.pick = w.pick, &before, nil
			return wizContinue, w.toReview()
		}
		w.pick = nil
	}
	return wizContinue, cmd
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
