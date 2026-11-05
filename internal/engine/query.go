package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/steps"
)

const incrementalCond = "{incremental_where_cond}"

var errQueryTimeout = errors.New("query_timeout")

// BaseQuery is the stream's SELECT before incremental conditions: table or sql, then
// where, select and steps. {incremental_where_cond} becomes 1=1.
func BaseQuery(ctx context.Context, src *db.DB, s config.Stream, name string) (string, error) {
	c, err := CompileStream(ctx, src, s, name)
	return c.SQL, err
}

// CompileStream is BaseQuery with the columns at each step. Columns is nil without steps.
func CompileStream(ctx context.Context, src *db.DB, s config.Stream, name string) (steps.Compiled, error) {
	s.SQL = strings.ReplaceAll(s.SQL, incrementalCond, "1=1")
	t := db.ParseTable(name)
	if err := resolveSelect(ctx, src, &s, t); err != nil {
		return steps.Compiled{}, err
	}
	q := selectSQL(src.Dialect, s, t, "")
	if s.Steps == "" {
		return steps.Compiled{SQL: q}, nil
	}
	pipeline, err := steps.Parse(s.Steps)
	if err != nil {
		return steps.Compiled{}, fmt.Errorf("steps: %w", err)
	}
	cols, err := probe(ctx, src, q)
	if err != nil {
		return steps.Compiled{}, err
	}
	return steps.CompileWithColumns(q, true, cols, pipeline, src.Dialect)
}

// resolveSelect turns an exclusion list (select = ["-col"]) into the table's remaining columns.
func resolveSelect(ctx context.Context, src *db.DB, s *config.Stream, t db.Table) error {
	if len(s.Select) == 0 || !strings.HasPrefix(s.Select[0], "-") {
		return nil
	}
	plain := *s
	plain.Select = nil
	plain.Steps = ""
	all, err := probe(ctx, src, selectSQL(src.Dialect, plain, t, ""))
	if err != nil {
		return err
	}
	s.Select = s.Columns(all)
	return nil
}

// selectSQL is the stream's table or sql with where, select and an extra condition.
func selectSQL(dl db.Dialect, s config.Stream, t db.Table, extra string) string {
	var conds []string
	if s.Where != "" {
		conds = append(conds, "("+s.Where+")")
	}
	if extra != "" {
		conds = append(conds, extra)
	}
	if s.SQL != "" {
		q := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s.SQL), ";"))
		if len(conds) == 0 {
			return q
		}
		return db.Wrap(dl, q, "*", "_f") + " WHERE " + strings.Join(conds, " AND ")
	}
	cols := "*"
	if len(s.Select) > 0 {
		qs := make([]string, len(s.Select))
		for i, c := range s.Select {
			qs[i] = dl.Quote(c)
		}
		cols = strings.Join(qs, ", ")
	}
	q := "SELECT " + cols + " FROM " + db.QuoteTable(dl, t)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	return q
}

// probe reads the column names of a query without rows.
func probe(ctx context.Context, src *db.DB, q string) ([]string, error) {
	rd, err := src.Query(ctx, db.Wrap(src.Dialect, q, "*", "_c")+" WHERE 1=0")
	if err != nil {
		return nil, err
	}
	rd.Close()
	names := make([]string, len(rd.Columns))
	for i, c := range rd.Columns {
		names[i] = c.Name
	}
	return names, nil
}

// TargetTable is the table s loads into. Only tgt's Dialect and Schema are read, so a
// DB without a connection will do.
func TargetTable(tgt *db.DB, s config.ResolvedStream) db.Table {
	return targetTable(tgt, plain(s))
}

// Watermark is the update-key value an incremental run of s starts from: MAX(update_key)
// of its existing target. nil for other modes, a missing or empty target.
func Watermark(ctx context.Context, tgt *db.DB, s config.ResolvedStream) (any, error) {
	st := plain(s)
	if st.Mode != "incremental" || st.UpdateKey == "" {
		return nil, nil
	}
	target := targetTable(tgt, st)
	exists, err := tgt.TableExists(ctx, target)
	if err != nil {
		return nil, err
	}
	return watermark(ctx, tgt, st, target, exists)
}

// RunQuery is the source SELECT and its arguments as a run of database stream s sends
// them, given the watermark wm from Watermark: sql or table, where, select, steps, the
// incremental or backfill condition and limit.
func RunQuery(ctx context.Context, src *db.DB, s config.ResolvedStream, wm any) (string, []any, error) {
	switch {
	case s.Path != "":
		return "", nil, fmt.Errorf("stream %s: an API stream has no SQL", s.Name)
	case strings.Contains(s.Name, "*"):
		return "", nil, fmt.Errorf("stream %s: a table pattern has no single query", s.Name)
	}
	q, args, err := sourceQuery(ctx, src, plain(s), wm)
	if err != nil {
		return "", nil, err
	}
	q, _ = textUDTs(ctx, src, q, args)
	return q, args, nil
}

// watermark is the highest update key of an existing incremental target, or nil. The
// update key is matched to the target's columns ignoring case: the source may spell it
// differently, and postgres compares quoted names exactly.
func watermark(ctx context.Context, tgt *db.DB, s stream, target db.Table, exists bool) (any, error) {
	if s.Mode != "incremental" || s.UpdateKey == "" || !exists {
		return nil, nil
	}
	cols, err := tgt.Columns(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("watermark: %w", err)
	}
	i := slices.IndexFunc(cols, func(c db.Column) bool { return strings.EqualFold(c.Name, s.UpdateKey) })
	if i < 0 {
		return nil, fmt.Errorf("watermark: %s has no column %s", target, s.UpdateKey)
	}
	wm, err := tgt.Max(ctx, target, cols[i])
	if err != nil {
		return nil, fmt.Errorf("watermark: %w", err)
	}
	return wm, nil
}

// spelled is the name in names that matches col ignoring case, else col as written.
func spelled(names []string, col string) string {
	for _, n := range names {
		if strings.EqualFold(n, col) {
			return n
		}
	}
	return col
}

// bound is the update-key condition of a run: operators and their values.
type bound struct {
	ops  []string
	vals []any
}

func boundFor(s stream, wm any) (bound, error) {
	switch s.Mode {
	case "incremental":
		if wm == nil {
			return bound{}, nil
		}
		// with a key, rows at the watermark load again
		op := ">"
		if len(s.PrimaryKey) > 0 {
			op = ">="
		}
		return bound{ops: []string{op}, vals: []any{wm}}, nil
	case "backfill":
		lo, hi, ok := strings.Cut(s.Range, ",")
		if !ok {
			return bound{}, fmt.Errorf("range must be lo,hi")
		}
		return bound{ops: []string{">=", "<="}, vals: []any{strings.TrimSpace(lo), strings.TrimSpace(hi)}}, nil
	}
	return bound{}, nil
}

// sourceQuery builds the SELECT for a stream with the incremental or backfill condition.
// The condition goes under the steps when the update key passes through them unchanged,
// else around them.
func sourceQuery(ctx context.Context, src *db.DB, s stream, wm any) (string, []any, error) {
	dl := src.Dialect
	b, err := boundFor(s, wm)
	if err != nil {
		return "", nil, err
	}
	var args []any
	render := func(col string) string {
		parts := make([]string, len(b.ops))
		for i, op := range b.ops {
			args = append(args, b.vals[i])
			parts[i] = dl.Quote(col) + " " + op + " " + dl.Placeholder(len(args))
		}
		return strings.Join(parts, " AND ")
	}
	cond := func(col string) string {
		if len(b.ops) == 0 {
			return ""
		}
		return render(col)
	}

	spec := s.Stream
	if err := resolveSelect(ctx, src, &spec, s.table); err != nil {
		return "", nil, err
	}
	s.Stream = spec
	if strings.Contains(spec.SQL, incrementalCond) {
		// each occurrence gets its own placeholders
		parts := strings.Split(spec.SQL, incrementalCond)
		var sb strings.Builder
		for i, p := range parts {
			if i > 0 {
				sb.WriteString(cmp.Or(cond(s.UpdateKey), "1=1"))
			}
			sb.WriteString(p)
		}
		spec.SQL = sb.String()
		b = bound{}
	}

	// the update key as the source query spells it, for the condition and the order
	uk := s.UpdateKey
	needKey := uk != "" && (len(b.ops) > 0 || s.Limit > 0 && s.Mode == "incremental")
	plainSpec := s.Stream
	plainSpec.SQL = strings.ReplaceAll(plainSpec.SQL, incrementalCond, "1=1")
	var q string
	if spec.Steps == "" {
		if needKey && src.DB != nil { // without a connection (tests) the key stays as written
			names, err := probe(ctx, src, selectSQL(dl, plainSpec, s.table, ""))
			if err != nil {
				return "", nil, err
			}
			uk = spelled(names, uk)
		}
		q = selectSQL(dl, spec, s.table, cond(uk))
	} else {
		pipeline, err := steps.Parse(spec.Steps)
		if err != nil {
			return "", nil, fmt.Errorf("steps: %w", err)
		}
		cols, err := probe(ctx, src, selectSQL(dl, plainSpec, s.table, ""))
		if err != nil {
			return "", nil, err
		}
		c, err := steps.CompileWithColumns(selectSQL(dl, spec, s.table, ""), true, cols, pipeline, dl)
		if err != nil {
			return "", nil, fmt.Errorf("steps: %w", err)
		}
		if n := len(c.Columns); n > 0 {
			uk = spelled(c.Columns[n-1], uk)
		}
		q = c.SQL
		if len(b.ops) > 0 {
			if origin, ok := c.Origin(uk); ok {
				q, err = steps.Compile(selectSQL(dl, spec, s.table, render(origin)), true, cols, pipeline, dl)
				if err != nil {
					return "", nil, fmt.Errorf("steps: %w", err)
				}
			} else {
				q = db.Wrap(dl, q, "*", "_w") + " WHERE " + render(uk)
			}
		}
	}
	if s.Limit > 0 {
		order := ""
		if s.Mode == "incremental" && uk != "" {
			order = dl.Quote(uk) // oldest first
		}
		q = dl.OrderLimit(q, order, s.Limit)
	}
	return q, args, nil
}

// textUDTs wraps q so columns of the source's BinaryUDTs arrive as text, and returns
// the converted columns with their source types. q is returned as is when it has none,
// or when the probe fails: the query then reports its own error.
func textUDTs(ctx context.Context, src *db.DB, q string, args []any) (string, map[string]string) {
	udts := src.Dialect.BinaryUDTs()
	if len(udts) == 0 {
		return q, nil
	}
	rd, err := src.Query(ctx, db.Wrap(src.Dialect, q, "*", "_c")+" WHERE 1=0", args...)
	if err != nil {
		return q, nil
	}
	rd.Close()
	sel := make([]string, len(rd.Columns))
	converted := map[string]string{}
	for i, c := range rd.Columns {
		name := src.Dialect.Quote(c.Name)
		sel[i] = "_u." + name
		if f, ok := udts[c.Type]; ok {
			sel[i] = fmt.Sprintf(f, sel[i]) + " AS " + name
			converted[c.Name] = c.Type
		}
	}
	if len(converted) == 0 {
		return q, nil
	}
	return db.Wrap(src.Dialect, q, strings.Join(sel, ", "), "_u"), converted
}

// openDB runs the source query for a database stream. timeout bounds the wait for the first row.
func (e *Engine) openDB(ctx context.Context, src, tgt *db.DB, s stream, target db.Table, exists bool, timeout time.Duration) (*rowSource, error) {
	wm, err := watermark(ctx, tgt, s, target, exists)
	if err != nil {
		return nil, err
	}
	query, args, err := sourceQuery(ctx, src, s, wm)
	if err != nil {
		return nil, err
	}
	query, _ = textUDTs(ctx, src, query, args)
	e.log().Debug("source query", "stream", s.Name, "sql", query, "args", args)

	qctx, cancel := context.WithCancelCause(ctx)
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() { cancel(fmt.Errorf("%w %s exceeded", errQueryTimeout, timeout)) })
	}
	started := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
	}
	explain := func(err error) error {
		if c := context.Cause(qctx); errors.Is(c, errQueryTimeout) {
			return fmt.Errorf("%w: %w", c, err)
		}
		return err
	}
	rd, err := src.Query(qctx, query, args...)
	if err != nil {
		started()
		cancel(nil)
		return nil, explain(err)
	}
	return &rowSource{
		cols: rd.Columns,
		next: func() ([]any, error) {
			row, err := rd.Next()
			started()
			if err != nil {
				return nil, explain(err)
			}
			return row, nil
		},
		close: func() {
			rd.Close()
			cancel(nil)
		},
	}, nil
}
