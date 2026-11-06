package tui

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	tea "github.com/charmbracelet/bubbletea"
	"sort"
	"strconv"
	"strings"
	"time"
	"tummy/internal/txt"

	"tummy/internal/api"
	"tummy/internal/config"
	"tummy/internal/engine"
)

var placeholderRe = regexp.MustCompile(`\{[A-Za-z0-9_]+\.[^{}]+\}`)

// apiKey is a top-level key of a response: arrays can hold the records, scalars the cursor.
type apiKey struct {
	name  string
	array bool
}

// recordsOptions are the records paths the response offers: its arrays.
func recordsOptions(keys []apiKey, current string) []string {
	out := []string{""}
	for _, k := range keys {
		if k.array {
			out = append(out, k.name+"[]")
		}
	}
	if !slices.Contains(out, current) {
		out = append(out, current)
	}
	return out
}

// cursorOptions are the cursor fields the response offers: its scalar keys.
func cursorOptions(keys []apiKey, current string) []string {
	out := []string{""}
	for _, k := range keys {
		if !k.array {
			out = append(out, k.name)
		}
	}
	if !slices.Contains(out, current) {
		out = append(out, current)
	}
	return out
}

// setRespKeys offers the keys of a response as records path and cursor field.
func (e *explorer) setRespKeys(keys []apiKey) {
	e.respKeys = keys
	if e.endpoint == nil {
		return
	}
	if fl := e.endpoint.field("records"); fl != nil {
		fl.Options = recordsOptions(keys, fl.Value)
	}
	if fl := e.endpoint.field("cursor_field"); fl != nil {
		fl.Options = cursorOptions(keys, fl.Value)
	}
}

func (e *explorer) endpointForm(s config.Stream) *form {
	hasPlaceholders := func(f *form) bool {
		return placeholderRe.MatchString(f.get("path") + f.get("query") + f.get("body"))
	}
	checkPairs := func(v string) error { _, err := parsePairs(v); return err }
	method := strings.ToUpper(s.Method)
	if method != "POST" {
		method = "GET"
	}
	return newForm(e.conn.Name,
		text("path", "Path", "/users   appended to the base URL").def(s.Path).required(),
		choice("method", "Method", "", "GET", "POST").def(method),
		text("query", "Query", "status = paid, limit = 50").def(pairs(s.Query)).check(checkPairs),
		text("body", "Body", "JSON sent with POST").def(s.Body).when(func(f *form) bool { return f.get("method") == "POST" }),
		combo("records", "Records", "data[]   results[]   JMESPath", recordsOptions(e.respKeys, s.Records)...).empty("response").def(s.Records),
		choice("paginate", "Paginate", "", "none", "page", "offset", "cursor", "link").def(cmpOr(s.Paginate, "none")),
		text("page_size", "Page size", "100").def(zeroBlank(s.PageSize)).check(isInt).when(func(f *form) bool { return f.get("paginate") != "none" }),
		combo("cursor_field", "Cursor field", "next   meta.next_cursor   JMESPath in the response", cursorOptions(e.respKeys, s.CursorField)...).empty("none").def(s.CursorField).when(func(f *form) bool { return f.get("paginate") == "cursor" }),
		text("vars", "Values", "users.id = 1").def(pairs(e.vars)).check(checkPairs).when(hasPlaceholders),
	)
}

// specFromEndpoint is the api stream the preview started from with the form's values;
// keys the form does not show are kept.
func (e *explorer) specFromEndpoint() config.Stream {
	f := e.endpoint
	s := e.apiBase
	s.Path, s.Body, s.Records = f.get("path"), f.get("body"), f.get("records")
	s.Method = ""
	if f.get("method") == "POST" {
		s.Method = "POST"
	}
	s.Query, _ = parsePairs(f.get("query"))
	s.Paginate = f.get("paginate")
	if s.Paginate == "none" {
		s.Paginate = ""
	}
	s.PageSize = atoi(f.get("page_size"))
	s.CursorField = f.get("cursor_field")
	e.vars, _ = parsePairs(f.get("vars"))
	return s
}

// apiFetch reads up to n pages. A for_each stream runs for the first records of its
// parent, three more parents per batch, or for typed values.
func (e *explorer) apiFetch(spec config.Stream, n int) func() previewMsg {
	conn := e.conn
	vars := e.vars
	var parent *config.Stream
	var parentErr error
	if spec.ForEach != "" && len(vars) == 0 {
		parent, parentErr = e.parentStream(spec.ForEach)
	}
	return func() previewMsg {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if parentErr != nil {
			return previewMsg{Err: parentErr, Total: -1}
		}
		client, err := api.NewClient(conn)
		if err != nil {
			return previewMsg{Err: err, Total: -1}
		}
		msg := previewMsg{Total: -1}
		var recs []api.Record
		var keys []string
		if parent != nil {
			var s forEachResult
			s, err = forEachRun(ctx, client, spec, *parent, n, &keys)
			recs, msg.Parents, msg.Complete = s.recs, s.parents, s.complete
			if len(s.parentCols) > 0 {
				msg.ParentCols = &parentColsMsg{Parent: spec.ForEach, Cols: s.parentCols}
			}
		} else {
			pages := 0
			recs, err = client.FetchWith(ctx, &spec, api.Options{Vars: vars, MaxPages: n, Keys: &keys, OnPage: func(int) { pages++ }})
			recs = withAdded(recs, spec.AddColumns, vars)
			msg.Complete = pages < n || spec.Paginate == ""
		}
		if err != nil {
			msg.Err = err
			return msg
		}
		if len(recs) > previewLimit*n {
			recs, msg.Complete = recs[:previewLimit*n], false
		}
		msg.Took = time.Since(start)
		switch {
		case len(recs) == 0 && spec.Records != "":
			msg.Note, msg.Keys = responseKeys(ctx, client, spec, vars)
		case spec.Records == "" && len(recs) == 1:
			msg.Keys = recordKeys(recs[0], keys) // the response is one object: offer its arrays
		}
		msg.Cols, msg.Rows = engine.Tabulate(recs, append(keys, addedKeys(spec.AddColumns)...)...)
		return msg
	}
}

// parentColsMsg carries a for_each parent's columns, from its first page.
type parentColsMsg struct {
	Parent string
	Cols   []string
	Err    error
}

// parentColumns reads the parent's first page for its columns.
func parentColumns(conn *config.Conn, name string, parent config.Stream) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		client, err := api.NewClient(conn)
		if err != nil {
			return parentColsMsg{Parent: name, Err: err}
		}
		var keys []string
		recs, err := client.FetchWith(ctx, &parent, api.Options{MaxPages: 1, Keys: &keys})
		if err != nil {
			return parentColsMsg{Parent: name, Err: err}
		}
		cols, _ := engine.Tabulate(recs, keys...)
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		return parentColsMsg{Parent: name, Cols: names}
	}
}

// parentCache holds the columns of for_each parents read so far, and the one being read.
type parentCache struct {
	cols    map[string][]string
	pending string
}

func (c *parentCache) get(f *form) []string { return c.cols[f.get("for_each")] }

func (c *parentCache) set(msg parentColsMsg) {
	if c.pending == msg.Parent {
		c.pending = ""
	}
	if c.cols == nil {
		c.cols = map[string][]string{}
	}
	c.cols[msg.Parent] = msg.Cols
	if c.cols[msg.Parent] == nil {
		c.cols[msg.Parent] = []string{} // read once; an error is shown, not retried per key
	}
}

// want starts a read of the form's for_each parent when its columns are unknown. The
// parent is looked up in the draft's streams.
func (c *parentCache) want(f *form, conn *config.Conn, streams []config.StreamDraft) tea.Cmd {
	name := f.get("for_each")
	if name == "" || c.cols[name] != nil || c.pending == name {
		return nil
	}
	i := slices.IndexFunc(streams, func(o config.StreamDraft) bool { return o.Name == name })
	if i < 0 {
		return nil
	}
	c.pending = name
	return parentColumns(conn, name, streams[i].Stream)
}

// addedKeys are the add_columns names, sorted.
func addedKeys(add map[string]string) []string {
	out := make([]string, 0, len(add))
	for k := range add {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parentStream is the for_each parent from the stream's replication.
func (e *explorer) parentStream(name string) (*config.Stream, error) {
	if e.repName == "" {
		return nil, fmt.Errorf("for_each %s: parent unknown", name)
	}
	path, err := config.ReplicationPath(e.opts.Home, e.repName)
	if err != nil {
		return nil, err
	}
	r, err := config.LoadReplication(path)
	if err != nil {
		return nil, err
	}
	for _, s := range r.Resolved {
		if s.Name == name {
			if s.ForEach != "" {
				return nil, fmt.Errorf("for_each %s: its parent has for_each too", name)
			}
			p := s.Stream
			return &p, nil
		}
	}
	return nil, fmt.Errorf("for_each %s: no such stream in %s", name, e.repName)
}

const forEachParents = 3 // parents a for_each preview runs for, per batch

// forEachResult is a for_each sample: the records, how many parents it ran for of how
// many, and whether that was all.
type forEachResult struct {
	recs       []api.Record
	parents    string
	parentCols []string // the parent's record keys, in response order
	complete   bool
}

// forEachRun fetches the parent's first page, then the child for its first records:
// three parents per batch. keys collects the record keys in response order.
func forEachRun(ctx context.Context, client *api.Client, spec, parent config.Stream, n int, keys *[]string) (forEachResult, error) {
	parentPages := 0
	var out forEachResult
	parents, err := client.FetchWith(ctx, &parent, api.Options{MaxPages: 1, Keys: &out.parentCols, OnPage: func(int) { parentPages++ }})
	if err != nil {
		return forEachResult{}, fmt.Errorf("for_each %s: %w", spec.ForEach, err)
	}
	if len(parents) > 0 {
		cols, _ := engine.Tabulate(parents, out.parentCols...)
		out.parentCols = out.parentCols[:0]
		for _, c := range cols {
			out.parentCols = append(out.parentCols, c.Name)
		}
	}
	tried, more := 0, false
	for _, p := range parents {
		if tried == forEachParents*n || len(out.recs) >= previewLimit*n {
			break
		}
		vars := map[string]string{}
		for k, v := range p {
			if v != nil {
				vars[spec.ForEach+"."+k] = fmt.Sprint(v)
			}
		}
		pages := 0
		recs, err := client.FetchWith(ctx, &spec, api.Options{Vars: vars, MaxPages: n, Keys: keys, OnPage: func(int) { pages++ }})
		if err != nil {
			if tried == 0 {
				return forEachResult{}, err
			}
			break
		}
		tried++
		more = more || (pages >= n && spec.Paginate != "")
		out.recs = append(out.recs, withAdded(recs, spec.AddColumns, vars)...)
	}
	of := strconv.Itoa(len(parents))
	if parent.Paginate != "" && parentPages > 0 {
		of += "+" // the parent has further pages
	}
	out.parents = fmt.Sprintf("%d of %s %s", tried, of, spec.ForEach)
	out.complete = tried == len(parents) && !more && !strings.HasSuffix(of, "+")
	return out, nil
}

// withAdded fills add_columns from the placeholder values.
func withAdded(recs []api.Record, add, vars map[string]string) []api.Record {
	if len(add) == 0 {
		return recs
	}
	for _, r := range recs {
		for col, tmpl := range add {
			v := tmpl
			for k, val := range vars {
				v = strings.ReplaceAll(v, "{"+k+"}", val)
			}
			r[col] = v
		}
	}
	return recs
}

// responseKeys reads one page without the records path and lists its top-level keys.
func responseKeys(ctx context.Context, client *api.Client, spec config.Stream, vars map[string]string) (string, []apiKey) {
	probe := spec
	probe.Records, probe.Paginate = "", ""
	var order []string
	recs, err := client.FetchWith(ctx, &probe, api.Options{Vars: vars, MaxPages: 1, Keys: &order})
	if err != nil || len(recs) != 1 {
		return "", nil
	}
	keys := recordKeys(recs[0], order)
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.name
	}
	return txt.Clean(fmt.Sprintf("records %s: none  keys: %s", spec.Records, strings.Join(names, ", "))), keys
}

// recordKeys are the keys of a response read as one record, in response order: JSON
// arrays can hold records, scalars a cursor.
func recordKeys(r api.Record, order []string) []apiKey {
	names := slices.Clone(order)
	for k := range r {
		if !slices.Contains(names, k) {
			names = append(names, k)
		}
	}
	var out []apiKey
	for _, k := range names {
		v, ok := r[k]
		if !ok {
			continue
		}
		t, isJSON := v.(api.JSONText)
		out = append(out, apiKey{name: k, array: isJSON && strings.HasPrefix(string(t), "[")})
	}
	return out
}
