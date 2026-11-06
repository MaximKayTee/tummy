package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"tummy/internal/api"
	"tummy/internal/config"
	"tummy/internal/db"
)

// apiRun is one run over an API connection.
type apiRun struct {
	e       *Engine
	client  *api.Client
	records map[string][]api.Record // records of parents, for for_each children
	keep    map[string]bool         // streams a selected child depends on
	full    map[string]bool         // streams whose children need every record: no watermark in the request
	streams map[string]stream       // by name, for parents that are not selected
	onDone  func(int)               // parents requested so far, for the child being opened
}

// plan marks the streams children depend on. streams are parent-first. A parent that is
// not selected itself is fetched for its records when its child runs, not loaded.
func (a *apiRun) plan(streams []stream, selected func(stream) bool) {
	a.keep, a.full, a.streams = map[string]bool{}, map[string]bool{}, map[string]stream{}
	for _, s := range streams {
		a.streams[s.Name] = s
	}
	for i := len(streams) - 1; i >= 0; i-- {
		s := streams[i]
		if (!selected(s) && !a.keep[s.Name]) || s.ForEach == "" {
			continue
		}
		a.keep[s.ForEach] = true
		if s.Mode != "incremental" || s.SkipExisting != "" || a.full[s.Name] || !selected(s) {
			a.full[s.ForEach] = true
		}
	}
}

// feed gets the records of a parent that is not selected: from its target table when it
// is loaded, else fetched from the API. No run, no load.
func (a *apiRun) feed(ctx context.Context, tgt *db.DB, name string, onParent func(string, int)) error {
	s, ok := a.streams[name]
	if !ok {
		return fmt.Errorf("for_each %q is not a stream", name)
	}
	target := targetTable(tgt, s)
	exists, err := tgt.TableExists(ctx, target)
	if err != nil {
		return fmt.Errorf("for_each %s: %w", name, err)
	}
	if exists {
		onParent(name, 0)
		recs, err := a.readTable(ctx, tgt, target, func(n int) { onParent(name, n) })
		if err != nil {
			return fmt.Errorf("for_each %s: %w", name, err)
		}
		a.records[name] = recs
		a.e.log().Info("for_each parent read from target", "stream", name, "table", target.String(), "records", len(recs))
		return nil
	}
	parents, err := a.parents(ctx, tgt, s, db.Table{}, false, onParent)
	if err != nil {
		return err
	}
	onParent(name, 0)
	_, kids, _, err := a.fetch(ctx, s, nil, parents, func(n int) { onParent(name, n) })
	if err != nil {
		return fmt.Errorf("for_each %s: %w", name, err)
	}
	a.records[name] = kids
	a.e.log().Info("for_each parent fetched", "stream", name, "records", len(kids))
	return nil
}

// readTable is every row of a target table as records keyed by column name.
func (a *apiRun) readTable(ctx context.Context, tgt *db.DB, t db.Table, onPage func(int)) ([]api.Record, error) {
	rd, err := tgt.Query(ctx, "SELECT * FROM "+tgt.Quote(t))
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	var out []api.Record
	for {
		row, err := rd.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			return out, nil
		}
		rec := make(api.Record, len(row))
		for i, c := range rd.Columns {
			rec[c.Name] = row[i]
		}
		out = append(out, rec)
		if len(out)%1000 == 0 {
			onPage(len(out))
		}
	}
}

// orderByForEach sorts streams so parents run before children.
func orderByForEach(streams []config.ResolvedStream) ([]config.ResolvedStream, error) {
	byName := map[string]config.ResolvedStream{}
	for _, s := range streams {
		byName[s.Name] = s
	}
	var out []config.ResolvedStream
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(name string, chain []string) error
	visit = func(name string, chain []string) error {
		switch state[name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("for_each cycle: %s", strings.Join(append(chain, name), " -> "))
		}
		s, ok := byName[name]
		if !ok {
			return fmt.Errorf("stream %s: for_each refers to unknown stream %q", chain[len(chain)-1], name)
		}
		state[name] = 1
		if s.ForEach != "" {
			if err := visit(s.ForEach, append(chain, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		out = append(out, s)
		return nil
	}
	for _, s := range streams {
		if err := visit(s.Name, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// openAPI fetches every record for an API stream, then serves them as rows.
func (e *Engine) openAPI(ctx context.Context, a *apiRun, tgt *db.DB, s stream, target db.Table, exists bool, runID int64, timeout time.Duration) (*rowSource, error) {
	wm, err := watermark(ctx, tgt, s, target, exists)
	if err != nil {
		return nil, err
	}
	fctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		fctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var last time.Time
	var cur string
	report := func(phase, object string, n int) {
		if phase+object != cur || time.Since(last) > 300*time.Millisecond {
			cur, last = phase+object, time.Now()
			e.phase(ctx, runID, s, object, phase, int64(n))
		}
	}
	done, of := 0, 0
	onPage := func(n int) {
		if phase, object := PhaseFetching, target.String(); phase+object != cur || time.Since(last) > 300*time.Millisecond {
			cur, last = phase+object, time.Now()
			e.phase(ctx, runID, s, object, phase, int64(n), done, of)
		}
	}
	onParent := func(name string, n int) { report(PhaseParents, name, n) }
	parents, err := a.parents(fctx, tgt, s, target, exists, onParent)
	if err != nil {
		return nil, err
	}
	of = len(parents)
	a.onDone = func(n int) { done = n }
	own, kids, keys, err := a.fetch(fctx, s, wm, parents, onPage)
	if err != nil {
		return nil, err
	}
	if of > 0 { // the final count, past the throttle
		e.phase(ctx, runID, s, target.String(), PhaseFetching, int64(len(own)), done, of)
	}
	if a.keep[s.Name] {
		a.records[s.Name] = kids
	}
	e.log().Debug("api fetched", "stream", s.Name, "records", len(own))
	cols := recordColumns(own, keys)
	if len(s.Select) > 0 {
		cols = selectColumns(cols, s.Select)
	}
	i := 0
	return &rowSource{
		cols: cols,
		next: func() ([]any, error) {
			if i >= len(own) {
				return nil, nil
			}
			i++
			return recordRow(own[i-1], cols), nil
		},
		close: func() {},
	}, nil
}

// fetch returns the records s loads (own), those its children iterate (kids) and the
// record keys in response order.
func (a *apiRun) fetch(ctx context.Context, s stream, wm any, parents []api.Record, onPage func(int)) (own, kids []api.Record, keys []string, err error) {
	incremental := s.Mode == "incremental" && s.UpdateKey != "" && wm != nil
	inclusive := len(s.PrimaryKey) > 0
	narrow := incremental && !a.full[s.Name] // the request may use the watermark
	spec := s.Stream
	vars := map[string]string{}
	if narrow {
		w := formatWatermark(wm, s.SinceFormat)
		vars["watermark"] = w
		if s.SinceParam != "" {
			spec.Query = maps.Clone(spec.Query)
			if spec.Query == nil {
				spec.Query = map[string]string{}
			}
			spec.Query[s.SinceParam] = w
		}
	}

	var stops []func([]api.Record) bool
	if narrow && s.Sorted == "desc" {
		stops = append(stops, func(p []api.Record) bool { return reachedWatermark(p, s.UpdateKey, wm, inclusive) })
	}
	enough := func(int) bool { return false }
	if s.Limit > 0 {
		switch {
		case !incremental:
			total := 0
			stops = append(stops, func(p []api.Record) bool { total += len(p); return total >= s.Limit })
			enough = func(n int) bool { return n >= s.Limit }
		case narrow && s.Sorted == "asc":
			// ascending: stop after limit newer records
			newer := 0
			stops = append(stops, func(p []api.Record) bool {
				newer += len(newerThan(p, s.UpdateKey, wm, inclusive))
				return newer >= s.Limit
			})
		}
	}
	o := api.Options{OnPage: onPage, Keys: &keys}
	if len(stops) > 0 {
		o.StopAfter = func(p []api.Record) bool {
			stop := false
			for _, f := range stops {
				stop = f(p) || stop
			}
			return stop
		}
	}

	all, err := a.fetchAll(ctx, s, spec, vars, parents, o, enough)
	if err != nil {
		return nil, nil, nil, err
	}
	own = all
	if incremental {
		own = newerThan(all, s.UpdateKey, wm, inclusive)
	}
	if s.Limit > 0 {
		if incremental {
			own = oldestFirst(own, s.UpdateKey)
		}
		own = own[:min(len(own), s.Limit)]
	}
	kids = own
	if a.full[s.Name] {
		kids = all
		if s.Limit > 0 {
			kids = kids[:min(len(kids), s.Limit)]
		}
	}
	return own, kids, keys, nil
}

// parents are the records a for_each stream runs for: the parent's records minus those
// below for_each_since and those whose skip_existing value the target already holds.
// A parent that did not run is fetched first.
func (a *apiRun) parents(ctx context.Context, tgt *db.DB, s stream, target db.Table, exists bool, onParent func(string, int)) ([]api.Record, error) {
	if s.ForEach == "" {
		return nil, nil
	}
	parents, ok := a.records[s.ForEach]
	if !ok {
		if err := a.feed(ctx, tgt, s.ForEach, onParent); err != nil {
			return nil, err
		}
		parents = a.records[s.ForEach]
	}
	total := len(parents)
	if s.ForEachSince != "" {
		key, since, err := config.ParseForEachSince(s.ForEachSince)
		if err != nil {
			return nil, err
		}
		parents = newerThan(parents, key, since, true)
	}
	if s.SkipExisting != "" && exists {
		have, err := a.loaded(ctx, tgt, s, target)
		if err != nil {
			return nil, err
		}
		kept := parents[:0:0]
		for _, p := range parents {
			if v := parentValue(s, s.SkipExisting, p); v == "" || !have[v] {
				kept = append(kept, p)
			}
		}
		parents = kept
	}
	if len(parents) < total {
		a.e.log().Info("for_each parents", "stream", s.Name, "of", total, "run", len(parents))
	}
	return parents, nil
}

// loaded is the set of skip_existing values in the target, as text.
func (a *apiRun) loaded(ctx context.Context, tgt *db.DB, s stream, target db.Table) (map[string]bool, error) {
	cols, err := tgt.Columns(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("skip_existing: %w", err)
	}
	i := slices.IndexFunc(cols, func(c db.Column) bool { return strings.EqualFold(c.Name, s.SkipExisting) })
	if i < 0 {
		return nil, fmt.Errorf("skip_existing: %s has no column %s", target, s.SkipExisting)
	}
	vals, err := tgt.Distinct(ctx, "SELECT "+tgt.Dialect.Quote(cols[i].Name)+" FROM "+tgt.Quote(target), cols[i].Name, 0)
	if err != nil {
		return nil, fmt.Errorf("skip_existing: %w", err)
	}
	have := make(map[string]bool, len(vals))
	for _, v := range vals {
		if v != nil {
			have[keyText(v)] = true
		}
	}
	return have, nil
}

// parentValue is what col holds for parent p: its add_columns template filled, else the
// parent field of that name.
func parentValue(s stream, col string, p api.Record) string {
	if tmpl, ok := s.AddColumns[col]; ok {
		v := tmpl
		for k, val := range p {
			v = strings.ReplaceAll(v, "{"+s.ForEach+"."+k+"}", formatValue(val))
		}
		return keyText(v)
	}
	return keyText(field(p, col))
}

// keyText is a value as comparison text: integers without decimals, times in RFC 3339.
func keyText(v any) string {
	if v == nil {
		return ""
	}
	if n, ok := asInt(v); ok {
		return strconv.FormatInt(n, 10)
	}
	if t, ok := asTime(v); ok {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return formatValue(v)
}

// fetchAll runs the requests for s, once per parent record with for_each. Parents whose
// placeholder fields are missing or null are skipped.
func (a *apiRun) fetchAll(ctx context.Context, s stream, spec config.Stream, vars map[string]string, parents []api.Record, o api.Options, enough func(int) bool) ([]api.Record, error) {
	if s.ForEach == "" {
		o.Vars = vars
		recs, err := a.client.FetchWith(ctx, &spec, o)
		if err != nil {
			return nil, err
		}
		return withColumns(recs, s.AddColumns, vars), nil
	}
	var all []api.Record
	skipped, empty := 0, 0
	byStatus := map[int]int{}
	onPage := o.OnPage
	for i, p := range parents {
		if enough(len(all)) {
			break
		}
		if a.onDone != nil {
			a.onDone(i)
		}
		req := maps.Clone(vars) // nulls left out: the client reports them as ErrNoValue
		add := maps.Clone(vars) // nulls empty
		for k, v := range p {
			key := s.ForEach + "." + k
			add[key] = formatValue(v)
			if v != nil {
				req[key] = add[key]
			}
		}
		o.Vars = req
		have := len(all)
		o.OnPage = func(n int) {
			if onPage != nil {
				onPage(have + n)
			}
		}
		status := 0
		o.OnSkip = func(st int) { status = st }
		recs, err := a.client.FetchWith(ctx, &spec, o)
		if errors.Is(err, api.ErrNoValue) {
			skipped++
			continue
		}
		if err != nil {
			return nil, err
		}
		switch {
		case status != 0:
			byStatus[status]++
			a.e.log().Warn("for_each parent skipped", "stream", s.Name, "status", status, "parent", parentKey(s, p))
		case len(recs) == 0:
			empty++
		}
		all = append(all, withColumns(recs, s.AddColumns, add)...)
		if onPage != nil {
			onPage(len(all)) // a parent with no records still moves the count
		}
	}
	if a.onDone != nil {
		a.onDone(len(parents))
	}
	if skipped > 0 {
		a.e.log().Warn("for_each parents skipped: placeholder field missing or null", "stream", s.Name, "parents", skipped)
	}
	args := []any{"stream", s.Name, "parents", len(parents), "records", len(all), "empty", empty}
	for st, n := range byStatus {
		args = append(args, "skipped_"+strconv.Itoa(st), n)
	}
	a.e.log().Info("for_each done", args...)
	return all, nil
}

// parentKey is the placeholder fields of parent p as the request used them.
func parentKey(s stream, p api.Record) string {
	var parts []string
	for _, k := range api.Placeholders(&s.Stream, s.ForEach+".") {
		parts = append(parts, k+"="+formatValue(field(p, k)))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func withColumns(recs []api.Record, add map[string]string, vars map[string]string) []api.Record {
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

func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// Tabulate infers columns from records and turns them into rows. Columns follow keys
// (api.Options.Keys), then the keys keys lacks, sorted per record.
func Tabulate(recs []api.Record, keys ...string) ([]db.Column, [][]any) {
	cols := recordColumns(recs, keys)
	rows := make([][]any, len(recs))
	for i, r := range recs {
		rows[i] = recordRow(r, cols)
	}
	return cols, rows
}

// recordColumns infers columns: the keys of order that some record has, then the other
// keys in first-seen order, sorted per record; kinds widened over all values.
func recordColumns(recs []api.Record, order []string) []db.Column {
	kinds := map[string]db.Kind{}
	seen := map[string]bool{}
	hasValue := map[string]bool{}
	var rest []string
	for _, r := range recs {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := r[k]
			if !seen[k] {
				seen[k] = true
				rest = append(rest, k)
			}
			if v == nil {
				continue
			}
			k2 := kindOfValue(v)
			if !hasValue[k] {
				kinds[k], hasValue[k] = k2, true
				continue
			}
			kinds[k] = widen(kinds[k], k2)
		}
	}
	var names []string
	placed := map[string]bool{}
	for _, k := range order {
		if seen[k] && !placed[k] {
			placed[k] = true
			names = append(names, k)
		}
	}
	for _, k := range rest {
		if !placed[k] {
			names = append(names, k)
		}
	}
	cols := make([]db.Column, len(names))
	for i, k := range names {
		cols[i] = db.Column{Name: k, Kind: kinds[k]}
	}
	return cols
}

// recordRow reads cols from r; missing keys are null.
func recordRow(r api.Record, cols []db.Column) []any {
	row := make([]any, len(cols))
	for j, c := range cols {
		row[j] = coerce(r[c.Name], c.Kind)
	}
	return row
}

// selectColumns keeps the named columns, in the order given. Unknown names become null columns.
func selectColumns(cols []db.Column, names []string) []db.Column {
	out := make([]db.Column, len(names))
	for i, n := range names {
		j := slices.IndexFunc(cols, func(c db.Column) bool { return strings.EqualFold(c.Name, n) })
		if j >= 0 {
			out[i] = cols[j]
		} else {
			out[i] = db.Column{Name: n, Kind: db.String}
		}
	}
	return out
}

func kindOfValue(v any) db.Kind {
	switch v.(type) {
	case bool:
		return db.Bool
	case int64:
		return db.Int
	case float64:
		return db.Float
	case json.Number:
		return db.Decimal
	case api.JSONText:
		return db.JSON
	}
	return db.String
}

func numeric(k db.Kind) bool { return k == db.Int || k == db.Float || k == db.Decimal }

func widen(a, b db.Kind) db.Kind {
	switch {
	case a == b:
		return a
	case numeric(a) && numeric(b):
		if a == db.Decimal || b == db.Decimal {
			return db.Decimal
		}
		return db.Float
	}
	return db.String
}

func coerce(v any, k db.Kind) any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case api.JSONText:
		return string(x)
	case json.Number:
		return x.String() // exact digits
	}
	switch k {
	case db.String:
		if x, ok := v.(string); ok {
			return x
		}
		b, _ := json.Marshal(v)
		return string(b)
	case db.Float:
		if i, ok := v.(int64); ok {
			return float64(i)
		}
	}
	return v
}
