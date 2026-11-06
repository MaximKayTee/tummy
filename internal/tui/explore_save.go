package tui

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
	"tummy/internal/db"
)

const newRepOption = "new replication"

// ---------- ctrl+s: back to the stream ----------

// canSaveBack: only a wizard's preview writes a stream; exploring never does.
func (e *explorer) canSaveBack() bool {
	return e.bound != nil && e.streamName != ""
}

// saveBack writes the steps, select and sql of the preview to the stream it came from.
// A table opened through a wildcard becomes its own stream.
func (e *explorer) saveBack() tea.Cmd {
	switch {
	case !e.canSaveBack():
		return nil
	case e.stepsErr != "" && len(e.pipeline) == 0:
		e.setNote(e.stepsErr, true)
		return nil
	case len(e.cols) == 0:
		e.setNote("no preview", true)
		return nil
	}
	d, err := e.loadDraft(e.repName)
	if err != nil {
		e.setNote(txt.Clean(txt.FirstLine(err.Error())), true)
		return nil
	}
	i := slices.IndexFunc(d.Streams, func(s config.StreamDraft) bool { return s.Name == e.streamName })
	var sd config.StreamDraft
	if i >= 0 {
		sd = d.Streams[i]
	} else {
		sd = config.StreamDraft{Name: e.streamName, Stream: e.wildcardBase(d)}
	}
	if err := e.applyEdits(&sd.Stream, e.tickedCols(), i < 0); err != nil {
		e.setNote(err.Error(), true)
		return nil
	}
	if sd.Stream.SQL != "" && sd.Stream.Object == "" {
		sd.Stream.Object = e.streamName
	}
	if i >= 0 {
		d.Streams[i] = sd
	} else {
		d.Streams = append(d.Streams, sd)
	}
	if err := e.writeDraft(d); err != nil {
		e.setNote(txt.Clean(txt.FirstLine(err.Error())), true)
		return nil
	}
	e.baseSteps, e.baseSQL = renderSteps(e.pipeline), e.spec.SQL
	e.tickBase = strings.Join(e.tickedCols(), "\x00")
	e.setNote("saved "+e.repName+".toml › "+e.streamName, false)
	return nil
}

// wildcardBase is the wildcard stream a table was opened through, or the defaults.
func (e *explorer) wildcardBase(d config.ReplicationDraft) config.Stream {
	for _, s := range d.Streams {
		if strings.ContainsAny(s.Name, "*?[") && wildMatch(s.Name, e.streamName) {
			return s.Stream
		}
	}
	return d.Defaults
}

func (e *explorer) loadDraft(rep string) (config.ReplicationDraft, error) {
	if e.bound != nil {
		d := *e.bound
		d.Streams = slices.Clone(d.Streams)
		return d, nil
	}
	path, err := config.ReplicationPath(e.opts.Home, rep)
	if err != nil {
		return config.ReplicationDraft{}, err
	}
	r, err := config.LoadReplication(path)
	if err != nil {
		return config.ReplicationDraft{}, err
	}
	return config.DraftOf(r), nil
}

// applyEdits writes what the preview changed into s. A new stream also takes the
// source settings of the preview: sql, where, the api request.
func (e *explorer) applyEdits(s *config.Stream, ticked []string, isNew bool) error {
	ticksChanged := strings.Join(ticked, "\x00") != e.tickBase
	partial := len(ticked) < len(e.cols)
	if isNew {
		s.SQL, s.Where = e.spec.SQL, e.spec.Where
		if e.isAPI {
			api := e.spec
			api.Mode, api.Object, api.PrimaryKey, api.UpdateKey = s.Mode, s.Object, s.PrimaryKey, s.UpdateKey
			api.Range, api.Limit, api.Disabled, api.QueryTimeout = s.Range, s.Limit, s.Disabled, s.QueryTimeout
			*s = api
		}
	} else if e.spec.SQL != e.baseSQL {
		s.SQL = e.spec.SQL
	}
	if e.isAPI {
		switch {
		case !ticksChanged && !isNew:
		case partial:
			s.Select = ticked
		default:
			s.Select = nil
		}
		return nil
	}
	if e.ticksAsKeep() {
		p, err := e.finalSteps(ticked)
		if err != nil {
			return err
		}
		s.Steps = renderSteps(p)
		s.Select = e.spec.Select
		return nil
	}
	s.Steps = ""
	switch {
	case !ticksChanged && !isNew:
		s.Select = e.origSelect
	case partial:
		s.Select = ticked
	default:
		s.Select = nil
	}
	return nil
}

// ---------- +: add to a replication ----------

// writeDraft saves a replication: into the bound draft, else its file.
func (e *explorer) writeDraft(d config.ReplicationDraft) error {
	if e.bound != nil {
		*e.bound = d
		e.draftSaved = true
		return nil
	}
	_, err := config.WriteReplication(e.opts.Home, d)
	return err
}

func (e *explorer) toTargetPick() tea.Cmd {
	if e.bound != nil {
		return e.toStreamForm()
	}
	files, _ := config.ListReplicationFiles(e.opts.Home)
	var reps []string
	for _, f := range files {
		if r, err := config.LoadReplication(f); err == nil && r.Source == e.conn.Name {
			reps = append(reps, r.Name)
		}
	}
	def := newRepOption
	if slices.Contains(reps, e.repName) {
		def = e.repName
	}
	var targets []string
	for _, n := range e.conns.Names() {
		if c := e.conns.Conn[n]; c.Type != "api" && !c.ReadOnly {
			targets = append(targets, n) // the source too: a copy into another schema
		}
	}
	if len(targets) == 0 {
		targets = e.conns.Names()
	}
	isNew := func(f *form) bool { return f.get("rep") == newRepOption }
	e.repPick = newForm("Add to replication",
		choice("rep", "Replication", "", append([]string{newRepOption}, reps...)...).def(def),
		text("name", "Name", "file name, letters digits _ -").when(isNew).required().check(func(v string) error {
			if !nameRe.MatchString(v) {
				return errors.New("letters, digits, _ -")
			}
			if p, err := config.ReplicationPath(e.opts.Home, v); err == nil {
				if _, err := os.Stat(p); err == nil {
					return errors.New("exists")
				}
			}
			return nil
		}),
		choice("target", "Target", "", targets...).when(isNew).notes(connNotes(e.conns)),
	)
	e.repPick.done = "next"
	e.page = pageTarget
	return e.repPick.focus()
}

func (e *explorer) toStreamForm() tea.Cmd {
	rep := newRepOption
	if e.bound != nil {
		rep = e.bound.Name
	} else {
		rep = e.repPick.get("rep")
	}
	if rep == newRepOption {
		e.draft = config.ReplicationDraft{
			Name: e.repPick.get("name"), Source: e.conn.Name, Target: e.repPick.get("target"),
			Enabled: true, Defaults: config.Stream{Mode: "full-refresh"},
		}
	} else {
		d, err := e.loadDraft(rep)
		if err != nil {
			if e.repPick != nil {
				e.repPick.field("rep").err = txt.FirstLine(err.Error())
			}
			return nil
		}
		e.draft = d
	}
	name := e.defaultStreamName()
	s := e.draft.Defaults
	e.editing = ""
	i := slices.IndexFunc(e.draft.Streams, func(sd config.StreamDraft) bool { return sd.Name == name })
	isNew := i < 0 || e.bound == nil // exploring adds a stream; only a wizard edits one
	switch {
	case !isNew:
		s, e.editing = e.draft.Streams[i].Stream, name
	case e.streamName != "":
		s = e.spec // the stream as opened, as a new stream
	}
	if err := e.applyEdits(&s, e.tickedCols(), isNew); err != nil {
		if e.repPick != nil {
			e.repPick.err = err.Error()
		} else {
			e.setNote(err.Error(), true)
		}
		return nil
	}
	e.formBase = s
	title := "Add stream to " + e.draft.Name
	if e.editing != "" {
		title = "Edit stream " + name + " in " + e.draft.Name
	}
	e.stream = e.streamForm(title, name, s)
	e.page = pageStream
	return e.stream.focus()
}

func (e *explorer) defaultStreamName() string {
	switch {
	case e.streamName != "":
		return e.streamName
	case e.isAPI:
		r := strings.NewReplacer("/", "_", "{", "", "}", "", ".", "_")
		return strings.Trim(r.Replace(e.spec.Path), "_")
	case e.spec.SQL != "":
		return "query"
	}
	return e.table
}

// objectPatterns are target names that follow the stream.
var objectPatterns = []string{"{stream_table}", "{stream_schema}.{stream_table}", "{target_schema}.{stream_table}", "{stream_schema}_{stream_table}"}

// streamForm is the stream form of the replication wizard, filled with what the preview
// set: sql, where, steps and ticked columns, or the request of an endpoint. Primary
// and update key list the loaded columns; the update key only those that order.
func (e *explorer) streamForm(title, name string, s config.Stream) *form {
	out := e.tickedCols()
	base := e.colNames()
	if (len(e.pipeline) > 0 || e.spec.SQL != "") && len(e.stepCols) > 0 {
		base = e.stepCols[0] // select applies before the steps
	}
	kinds := map[string]db.Kind{}
	for _, c := range e.cols {
		kinds[c.Name] = c.Kind
	}
	orders := func(c string) bool {
		switch kinds[c] {
		case db.Int, db.Float, db.Decimal, db.Date, db.Timestamp:
			return true
		case db.String:
			return e.isAPI // dates come as text
		}
		return false
	}
	others := []string{""}
	for _, o := range e.draft.Streams {
		if o.Name != name {
			others = append(others, o.Name)
		}
	}
	fields := []*field{text("name", "Name", "users   the target table unless object is set").def(name).required()}
	for _, fl := range streamFields(s, base, e.isAPI) {
		switch fl.Key {
		case "object":
			opts := []string{""}
			if d := e.draft.Defaults.Object; d != "" {
				opts = append(opts, d)
			}
			for _, p := range append(objectPatterns, s.Object) {
				if !slices.Contains(opts, p) {
					opts = append(opts, p)
				}
			}
			if e.isAPI {
				fields = append(fields, multi("select", "Columns", "", e.colNames(), s.Columns(e.colNames())).fixed())
			}
			obj := combo("object", "Object", fl.Help, opts...).empty("stream name").def(s.Object)
			if d := e.draft.Defaults.Object; d != "" {
				obj.empty("default").notes(map[string]string{"": d})
			}
			fl = obj
		case "for_each":
			fl.Options = others
		case "primary_key":
			fl.Options = out
		case "update_key":
			opts := []string{""}
			for _, c := range out {
				if orders(c) || c == s.UpdateKey {
					opts = append(opts, c)
				}
			}
			fl.Options = opts
		}
		fields = append(fields, fl)
	}
	f := newForm(title, fields...)
	if fl := f.field("primary_key"); fl != nil && fl.Kind == kindMulti && len(s.PrimaryKey) == 0 && len(e.srcPK) > 0 {
		all := true
		for _, c := range e.srcPK {
			all = all && slices.Contains(out, c)
		}
		if all {
			for _, c := range e.srcPK {
				fl.Picked[c] = true // the source table's key; set after the form opened, so it is saved
			}
		}
	}
	return f
}

// saveStream writes the stream form into the replication and goes back to the preview,
// which from then on saves to that stream.
func (e *explorer) saveStream() error {
	f := e.stream
	name := f.get("name")
	i := slices.IndexFunc(e.draft.Streams, func(sd config.StreamDraft) bool { return sd.Name == name })
	if i >= 0 && name != e.editing {
		return errors.New(name + " exists in " + e.draft.Name)
	}
	var sd config.StreamDraft
	if i >= 0 {
		sd = e.draft.Streams[i]
	}
	sd.Name = name
	sd.Stream = streamFromFields(f, e.formBase)
	s := &sd.Stream
	// one-line fields cannot hold the line breaks of a query: unchanged, keep it as written
	if !f.fieldChanged("sql") {
		s.SQL = e.formBase.SQL
	}
	if !f.fieldChanged("where") {
		s.Where = e.formBase.Where
	}
	if s.SQL != "" && s.Object == "" {
		s.Object = name // a query has no table to name the target after
	}
	check := *s
	check.Mode = cmpOr(check.Mode, cmpOr(e.draft.Defaults.Mode, "full-refresh"))
	if err := config.ValidateStream(name, &check); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "stream "+name+": "))
	}
	if i >= 0 {
		e.draft.Streams[i] = sd
	} else {
		e.draft.Streams = append(e.draft.Streams, sd)
	}
	if err := e.writeDraft(e.draft); err != nil {
		return err
	}
	e.done = statusMsg{Text: fmt.Sprintf("saved: %s.toml", e.draft.Name)}
	if e.draft.Source == e.conn.Name {
		e.repName, e.streamName = e.draft.Name, name
		e.origSelect = s.Select
		e.baseSteps, e.baseSQL = renderSteps(e.pipeline), e.spec.SQL
		e.tickBase = strings.Join(e.tickedCols(), "\x00")
	}
	e.setNote("saved "+e.draft.Name+".toml › "+name, false)
	return nil
}
