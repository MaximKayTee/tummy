package tui

import (
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
	"tummy/internal/db"
)

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
	} else if d.From != "" {
		title = "Copy of " + d.From
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
	if f.get("source") != d.Source { // listed again for the new source
		w.srcTables, w.srcErr, w.srcLoading, w.srcSeq, w.wantPick = nil, "", false, 0, false
		w.colsSeen, w.colsAsked = nil, false
	}
	if f.get("target") != d.Target {
		w.tgtTables, w.tgtLoading, w.tgtSeq = nil, false, 0
	}
	d.Name, d.Source, d.Target = f.get("name"), f.get("source"), f.get("target")
	d.Enabled = f.on("enabled")
	d.Schedule = scheduleOf(f)
}

func (w *repWizard) applyBasics() {
	old := w.draft.Defaults
	w.basicsInto(&w.draft)
	w.rebase(old)
}

// defaultsFields are the replication-wide keys the defaults step edits.
var defaultsKeys = []string{"mode", "primary_key", "update_key", "object", "records", "paginate", "page_size", "page_param", "size_param", "cursor_field", "cursor_param"}

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
	source := w.draft.Source
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out := map[string][]db.Column{}
		d, err := db.Open(ctx, c.DB())
		if err != nil {
			return tableColsMsg{Source: source, Cols: out}
		}
		defer d.Close()
		for _, t := range tables {
			if cols, err := d.Columns(ctx, db.ParseTable(t)); err == nil && len(cols) > 0 {
				out[t] = cols
			}
		}
		return tableColsMsg{Source: source, Cols: out}
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
