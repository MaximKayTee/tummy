package tui

import (
	"cmp"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"tummy/internal/config"
	"tummy/internal/db"
)

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
	fs := streamFieldsOf(sd.Stream, cols, kinds, w.parentCols[sd.Stream.ForEach], api)
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

// placeholders lists the parent's fields as {parent.field}, for the add_columns help.
func placeholders(parent string, cols []string) string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, "{"+parent+"."+c+"}")
	}
	return strings.Join(out, "  ")
}

// sinceOptions offers "col, 30d" per parent column.
func sinceOptions(cols []string) []string {
	out := []string{""}
	for _, c := range cols {
		out = append(out, c+", 30d")
	}
	return out
}

// streamFields lists the editable keys of a stream, filled from s. With cols known the
// column keys are lists of them; api picks the endpoint keys over the table ones. The
// replication wizard and the explorer build their stream forms from it; streamFromFields
// reads a form back.
func streamFields(s config.Stream, cols []string, api bool) []*field {
	return streamFieldsOf(s, cols, nil, nil, api)
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
// loaded columns an update key can compare, and the for_each parent's columns for the
// keys that name them.
func streamFieldsOf(s config.Stream, cols []string, kinds map[string]db.Kind, parentCols []string, api bool) []*field {
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
				p := f.get("for_each")
				if len(parentCols) > 0 {
					return placeholders(p, parentCols)
				}
				if f.get("add_columns") != "" {
					return ""
				}
				return strings.TrimSuffix(p, "s") + "_id = {" + p + ".id}"
			}),
			combo("skip_existing", "Skip existing", "target column; parents already loaded are skipped", append([]string{""}, cols...)...).empty("none").eg("product_id").def(s.SkipExisting).when(func(f *form) bool { return f.get("for_each") != "" }),
			combo("for_each_since", "For each since", "parent field, lower bound; Nd = days ago", sinceOptions(parentCols)...).empty("none").eg("created_at, 2026-01-01   created_at, 30d").def(s.ForEachSince).when(func(f *form) bool { return f.get("for_each") != "" }).check(func(v string) error {
				if v == "" {
					return nil
				}
				_, _, err := config.ParseForEachSince(v)
				return err
			}),
		)
		if s.ForEach != "" {
			fs[len(fs)-4].Options = []string{"", s.ForEach}
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
		"skip_existing": &s.SkipExisting, "for_each_since": &s.ForEachSince,
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
