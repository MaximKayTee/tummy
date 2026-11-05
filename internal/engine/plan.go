package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tummy/internal/config"
	"tummy/internal/db"
)

// SampleRows is how many source rows PlanSample loads per stream.
const SampleRows = 100

// PlanRow is what one stream of a run would do, worked out without writing.
type PlanRow struct {
	Stream    string
	Object    string
	Mode      string
	Action    string // create, replace, truncate, upsert, append, skip
	Exists    bool
	Watermark any    // incremental: the target's max update key
	SQL       string // source query; API streams: path
	Disabled  bool
	Err       string
	// Columns are the temp table's columns as the run would create them; database streams only.
	Columns []PlanColumn
	// Sample is the test load of PlanSample; nil from Plan.
	Sample *PlanSample
}

// PlanColumn maps one source column to the type the run creates for it.
type PlanColumn struct {
	Name   string
	Source string // driver type name; for columns converted to text, the type before it
	Target string
	Note   string // conversion, column the run adds to the target, differing target type
}

// PlanSample is the first SampleRows rows of the source query loaded into a copy of
// the run's temp table, inside a transaction that is rolled back.
type PlanSample struct {
	Rows     int64
	Read     time.Duration
	Load     time.Duration
	Stripped int64 // values a NUL byte was removed from
	Err      string
}

// Plan resolves every stream of r against its connections: target table, action for
// the mode, the source query with the watermark the run would start from, and the
// columns the run would create. Source and target are only read, with queries that
// return no rows; a missing sqlite target file is created empty, as a run would.
func (e *Engine) Plan(ctx context.Context, r *config.Replication) ([]PlanRow, error) {
	return e.plan(ctx, r, 0)
}

// PlanSample is Plan with a test load per database stream: the first SampleRows rows
// of its source query go through the run's insert path into a copy of its temp table,
// inside a transaction that is rolled back.
func (e *Engine) PlanSample(ctx context.Context, r *config.Replication) ([]PlanRow, error) {
	return e.plan(ctx, r, SampleRows)
}

func (e *Engine) plan(ctx context.Context, r *config.Replication, sample int) ([]PlanRow, error) {
	srcConn, err := e.Conns.Get(r.Source)
	if err != nil {
		return nil, err
	}
	tgtConn, err := e.Conns.Get(r.Target)
	if err != nil {
		return nil, err
	}
	if tgtConn.ReadOnly {
		return nil, fmt.Errorf("conn %s: read_only, not a target", r.Target)
	}
	tp := tgtConn.DB()
	tp.Create = true // a sqlite target the run would create: an empty file, as the run makes
	tgt, err := db.Open(ctx, tp)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	defer tgt.Close()
	var streams []stream
	var src *db.DB
	if srcConn.Type == "api" {
		ordered, err := orderByForEach(r.Resolved)
		if err != nil {
			return nil, err
		}
		for _, s := range ordered {
			streams = append(streams, plain(s))
		}
	} else {
		if src, err = db.Open(ctx, srcConn.DB()); err != nil {
			return nil, fmt.Errorf("source: %w", err)
		}
		defer src.Close()
		if streams, err = e.expand(ctx, src, tgt, r); err != nil {
			return nil, fmt.Errorf("source tables: %w", err)
		}
	}
	if err := sharedTarget(tgt, streams); err != nil {
		return nil, fmt.Errorf("targets: %w", err)
	}
	// the target's tables once, not one lookup per stream: a wildcard can be thousands
	listed, err := tgt.ListAllTables(ctx)
	if err != nil {
		return nil, fmt.Errorf("target tables: %w", err)
	}
	have := map[string]bool{}
	for _, t := range listed {
		have[t.String()] = true
	}
	var out []PlanRow
	for _, s := range streams {
		target := targetTable(tgt, s)
		row := PlanRow{Stream: s.Name, Object: target.String(), Mode: s.Mode, Disabled: s.Disabled}
		if s.Disabled {
			row.Action = "skip"
			out = append(out, row)
			continue
		}
		var exists bool
		if target.Schema != "" || !tgt.Dialect.HasSchemas() {
			exists = have[target.String()]
		} else if exists, err = tgt.TableExists(ctx, target); err != nil { // default schema: the engine knows it
			row.Err = "target exists: " + err.Error()
			out = append(out, row)
			continue
		}
		row.Exists = exists
		row.Action = planAction(s.Mode, exists)
		if src == nil {
			row.SQL = s.Path
			out = append(out, row)
			continue
		}
		wm, err := watermark(ctx, tgt, s, target, exists)
		if err != nil {
			row.Err = "target " + err.Error()
			out = append(out, row)
			continue
		}
		row.Watermark = wm
		q, args, err := sourceQuery(ctx, src, s, wm)
		if err != nil {
			row.Err = "source query: " + err.Error()
			out = append(out, row)
			continue
		}
		q, converted := textUDTs(ctx, src, q, args)
		row.SQL = q
		var sh shape
		if row.Columns, sh, err = planColumns(ctx, src, tgt, s, target, exists, q, args, converted); err != nil {
			row.Err = err.Error()
		} else if sample > 0 {
			row.Sample = sampleLoad(ctx, src, tgt, s, auxTable(target, "dry", r.Name), row.Object, q, args, sh, sample)
		}
		out = append(out, row)
	}
	return out, nil
}

// planColumns reads the columns of query q without rows and lays out the run's temp
// table for them, noting what the run would change or convert.
func planColumns(ctx context.Context, src, tgt *db.DB, s stream, target db.Table, exists bool,
	q string, args []any, converted map[string]string) ([]PlanColumn, shape, error) {
	rd, err := src.Query(ctx, db.Wrap(src.Dialect, q, "*", "_c")+" WHERE 1=0", args...)
	if err != nil {
		return nil, shape{}, fmt.Errorf("source columns: %w", err)
	}
	rd.Close()
	sh, err := shapeFor(s, rd.Columns)
	if err != nil {
		return nil, shape{}, err
	}
	var have map[string]db.Column
	if exists {
		tc, err := tgt.Columns(ctx, target)
		if err != nil {
			return nil, shape{}, fmt.Errorf("target columns: %w", err)
		}
		have = map[string]db.Column{}
		for _, c := range tc {
			have[strings.ToLower(c.Name)] = c
		}
	}
	sized := map[string]bool{}
	for _, k := range sh.sized {
		sized[k] = true
	}
	out := make([]PlanColumn, len(sh.cols))
	for i, c := range sh.cols {
		pc := PlanColumn{Name: c.Name, Source: c.Type, Target: tgt.Dialect.TypeName(c, sized[c.Name])}
		var notes []string
		if t, ok := converted[c.Name]; ok {
			pc.Source = t
			notes = append(notes, "converted to text in the source query")
		}
		if c.Name == loadedAtColumn && sh.snapshot {
			pc.Source = ""
			notes = append(notes, "load time, added by the run")
		}
		if have != nil {
			if tc, ok := have[strings.ToLower(c.Name)]; !ok {
				notes = append(notes, "not in the target: the run adds it")
			} else if k := tgt.Dialect.KindOf(tc.Type); k != c.Kind && k != db.String {
				notes = append(notes, "target column is "+tc.Type)
			}
		}
		pc.Note = strings.Join(notes, "; ")
		out[i] = pc
	}
	return out, sh, nil
}

// sampleLoad reads the first n rows of q and loads them into scratch table t, laid out
// as sh, in a transaction that is rolled back. label names the target in errors.
func sampleLoad(ctx context.Context, src, tgt *db.DB, s stream, t db.Table, label, q string, args []any, sh shape, n int) *PlanSample {
	out := &PlanSample{}
	fail := func(err error) *PlanSample {
		out.Err = err.Error()
		return out
	}
	qctx := ctx
	if s.QueryTimeout != "" {
		d, err := time.ParseDuration(s.QueryTimeout)
		if err != nil {
			return fail(fmt.Errorf("query_timeout: %w", err))
		}
		var cancel context.CancelFunc
		qctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	start := time.Now()
	rd, err := src.Query(qctx, src.Dialect.OrderLimit(q, "", n), args...)
	if err != nil {
		return fail(fmt.Errorf("read: %w", err))
	}
	var rows [][]any
	for {
		row, err := rd.Next()
		if err != nil {
			rd.Close()
			return fail(fmt.Errorf("read: %w", err))
		}
		if row == nil {
			break
		}
		rows = append(rows, row)
	}
	rd.Close()
	out.Read = time.Since(start)

	start = time.Now()
	sc, err := tgt.BeginScratch(ctx, t, sh.load, sh.sized)
	if err != nil {
		return fail(fmt.Errorf("create: %w", err))
	}
	defer sc.Close()
	sc.Label = label
	loadedAt := time.Now().UTC()
	for i, row := range rows {
		if err := sc.Add(ctx, sh.extend(row, loadedAt, int64(i+1))); err != nil {
			out.Rows, out.Stripped = sc.Total, sc.Stripped
			return fail(err)
		}
	}
	out.Rows, out.Stripped, out.Load = sc.Total, sc.Stripped, time.Since(start)
	return out
}

// planAction words what a mode does to a target that does or does not exist.
func planAction(mode string, exists bool) string {
	if !exists {
		return "create"
	}
	switch mode {
	case "full-refresh":
		return "replace"
	case "truncate":
		return "truncate"
	case "incremental":
		return "upsert"
	case "backfill", "snapshot":
		return "append"
	}
	return mode
}
