// Package engine runs replications: reads each stream from a database or API source,
// loads it into a temp table on the target and merges it by mode.
package engine

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"tummy/internal/api"
	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/lock"
	"tummy/internal/state"
)

const (
	loadedAtColumn = "_tummy_loaded_at"
	seqColumn      = "_tummy_seq" // load order in the temp table; dedupe tiebreak
)

type Engine struct {
	Conns   *config.Connections
	State   *state.Store
	Log     *slog.Logger
	Trigger string      // manual (default), schedule, daemon, or the requester's own
	LockDir string      // one lock per replication name; empty disables locking
	OnEvent func(Event) // optional live progress
	// CancelPoll is how often a running stream checks State for a cancel request; 0 is 1s.
	CancelPoll time.Duration
}

// ErrCancelled ends a run stopped by State.Cancel or by its context.
var ErrCancelled = state.ErrCancelled

// errInterrupted: the run's context ended (signal, shutdown).
var errInterrupted = fmt.Errorf("%w: interrupted", state.ErrCancelled)

// Event is one step of a run, for progress displays.
type Event struct {
	Stream string
	Object string
	Phase  string // connecting, querying, loading, merging, done, failed
	Rows   int64
	Err    error
}

const (
	PhaseConnecting = "connecting"
	PhaseQuerying   = "querying"
	PhaseFetching   = "fetching"
	PhaseLoading    = "loading"
	PhaseMerging    = "merging"
	PhaseDone       = "done"
	PhaseFailed     = "failed"
)

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (e *Engine) emit(ev Event) {
	if e.OnEvent != nil {
		e.OnEvent(ev)
	}
}

// phase records the phase in state and emits it.
func (e *Engine) phase(ctx context.Context, runID int64, s stream, object, phase string, rows int64) {
	if err := e.State.Progress(ctx, runID, rows, phase); err != nil {
		e.log().Warn("state write failed", "stream", s.Name, "err", err)
	}
	e.emit(Event{Stream: s.Name, Object: object, Phase: phase, Rows: rows})
}

type Result struct {
	Stream string
	Object string
	Rows   int64
	Took   time.Duration
	Err    error
}

// rowSource yields the columns and rows of one stream.
type rowSource struct {
	cols  []db.Column
	next  func() ([]any, error) // nil row at the end
	close func()
}

// stream is a resolved stream with its source table.
type stream struct {
	config.ResolvedStream
	table db.Table // source table; from ListTables for wildcard matches
	from  string   // configured stream name; the wildcard for its matches
}

func plain(s config.ResolvedStream) stream {
	return stream{ResolvedStream: s, table: db.ParseTable(s.Name), from: s.Name}
}

type opener func(ctx context.Context, s stream, target db.Table, exists bool, runID int64, timeout time.Duration) (*rowSource, error)

// Run executes every enabled stream of r. only limits it to the named streams; a wildcard
// stream's name selects all its matches.
func (e *Engine) Run(ctx context.Context, r *config.Replication, only []string) ([]Result, error) {
	if e.LockDir != "" {
		l, err := lock.Acquire(e.LockDir, r.Name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		defer l.Release()
	}
	results, err := e.run(ctx, r, only)
	if err != nil && ctx.Err() != nil && !errors.Is(err, ErrCancelled) {
		err = errInterrupted
	}
	if err != nil && len(results) == 0 {
		// Nothing started, so nothing was recorded. Keep the failure visible in history.
		e.recordFailure(ctx, r.Name, err)
	}
	return results, err
}

func (e *Engine) trigger() string { return cmp.Or(e.Trigger, "manual") }

func (e *Engine) recordFailure(ctx context.Context, replication string, err error) {
	ctx = context.WithoutCancel(ctx)
	id, serr := e.State.Start(ctx, replication, "*", "", e.trigger())
	if serr == nil {
		serr = e.State.Finish(ctx, id, 0, err)
	}
	if serr != nil {
		e.log().Warn("state write failed", "replication", replication, "err", serr)
	}
}

func (e *Engine) run(ctx context.Context, r *config.Replication, only []string) ([]Result, error) {
	srcConn, err := e.Conns.Get(r.Source)
	if err != nil {
		return nil, err
	}
	tgtConn, err := e.Conns.Get(r.Target)
	if err != nil {
		return nil, err
	}
	e.emit(Event{Phase: PhaseConnecting, Object: r.Target})
	tp := tgtConn.DB()
	tp.Create = true // a new sqlite target
	tgt, err := db.Open(ctx, tp)
	if err != nil {
		return nil, err
	}
	defer tgt.Close()

	var streams []stream
	var open opener
	var arun *apiRun
	if srcConn.Type == "api" {
		client, err := api.NewClient(srcConn)
		if err != nil {
			return nil, err
		}
		ordered, err := orderByForEach(r.Resolved)
		if err != nil {
			return nil, err
		}
		for _, s := range ordered {
			streams = append(streams, plain(s))
		}
		arun = &apiRun{e: e, client: client, records: map[string][]api.Record{}}
		open = func(ctx context.Context, s stream, target db.Table, exists bool, runID int64, timeout time.Duration) (*rowSource, error) {
			return e.openAPI(ctx, arun, tgt, s, target, exists, runID, timeout)
		}
	} else {
		e.emit(Event{Phase: PhaseConnecting, Object: r.Source})
		src, err := db.Open(ctx, srcConn.DB())
		if err != nil {
			return nil, err
		}
		defer src.Close()
		if streams, err = e.expand(ctx, src, tgt, r); err != nil {
			return nil, err
		}
		open = func(ctx context.Context, s stream, target db.Table, exists bool, runID int64, timeout time.Duration) (*rowSource, error) {
			return e.openDB(ctx, src, tgt, s, target, exists, timeout)
		}
	}

	want, err := wanted(streams, only)
	if err != nil {
		return nil, err
	}
	selected := func(s stream) bool {
		return !s.Disabled && (want == nil || want[s.Name] || want[s.from])
	}
	if arun != nil {
		// a child needs its ancestors; streams are parent-first
		for i := len(streams) - 1; i >= 0; i-- {
			if s := streams[i]; want != nil && want[s.Name] && s.ForEach != "" {
				want[s.ForEach] = true
			}
		}
		arun.plan(streams, selected)
	}

	var results []Result
	var failed int
	for _, s := range streams {
		if !selected(s) {
			continue
		}
		if ctx.Err() != nil {
			return results, errInterrupted
		}
		res := e.runStream(ctx, r, tgt, s, open)
		results = append(results, res)
		if errors.Is(res.Err, ErrCancelled) {
			e.log().Warn("run cancelled", "stream", s.Name, "rows", res.Rows)
			return results, res.Err
		}
		if res.Err != nil {
			failed++
			e.log().Error("stream failed", "stream", s.Name, "err", res.Err)
		} else {
			e.log().Info("stream done", "stream", s.Name, "object", res.Object, "rows", res.Rows, "took", res.Took.Round(time.Millisecond))
		}
	}
	if failed > 0 {
		return results, fmt.Errorf("%d of %d streams failed", failed, len(results))
	}
	return results, nil
}

// wanted is the set of names in only; nil for all. Unknown names are an error.
func wanted(streams []stream, only []string) (map[string]bool, error) {
	if len(only) == 0 {
		return nil, nil
	}
	have := map[string]bool{}
	for _, s := range streams {
		have[s.Name], have[s.from] = true, true
	}
	want := map[string]bool{}
	var unknown []string
	for _, n := range only {
		if !have[n] {
			unknown = append(unknown, n)
		}
		want[n] = true
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown streams: %s", strings.Join(unknown, ", "))
	}
	return want, nil
}

// expand turns wildcard streams into one stream per matching table and applies exclude to
// source and target names. An explicit stream wins over a wildcard match of the same name.
// Tables whose names hold dots, quotes or brackets are skipped.
func (e *Engine) expand(ctx context.Context, src, tgt *db.DB, r *config.Replication) ([]stream, error) {
	explicit := map[string]bool{}
	for _, s := range r.Resolved {
		if !strings.Contains(s.Name, "*") {
			explicit[s.Name] = true
		}
	}
	excluded := func(names ...string) bool {
		for _, pat := range r.Exclude {
			for _, n := range names {
				if ok, _ := path.Match(pat, n); ok {
					return true
				}
			}
		}
		return false
	}
	var out []stream
	for _, s := range r.Resolved {
		if !strings.Contains(s.Name, "*") {
			if !excluded(s.Name) {
				out = append(out, plain(s))
			}
			continue
		}
		if s.SQL != "" {
			return nil, fmt.Errorf("stream %s: wildcard with sql", s.Name)
		}
		pt := db.ParseTable(s.Name)
		tables, err := src.ListTables(ctx, pt.Schema)
		if err != nil {
			return nil, fmt.Errorf("stream %s: %w", s.Name, err)
		}
		for _, t := range tables {
			if ok, _ := path.Match(pt.Name, t.Name); !ok {
				continue
			}
			if !t.Plain() {
				e.log().Warn("table skipped: name holds dots, quotes or brackets", "stream", s.Name, "schema", t.Schema, "table", t.Name)
				continue
			}
			name := t.String()
			m := stream{ResolvedStream: config.ResolvedStream{Name: name, Stream: s.Stream}, table: t, from: s.Name}
			if explicit[name] || excluded(name, targetTable(tgt, m).String()) {
				continue
			}
			out = append(out, m)
		}
	}
	return out, nil
}

func (e *Engine) runStream(ctx context.Context, r *config.Replication, tgt *db.DB, s stream, open opener) (res Result) {
	start := time.Now()
	res.Stream = s.Name
	target := targetTable(tgt, s)
	res.Object = target.String()
	runID, err := e.State.Start(ctx, r.Name, s.Name, res.Object, e.trigger())
	if err != nil {
		res.Err = err
		return
	}
	ctx, stop := e.watchCancel(ctx, runID)
	defer stop()
	defer func() {
		if res.Err != nil && ctx.Err() != nil {
			res.Err = errInterrupted
			if errors.Is(context.Cause(ctx), ErrCancelled) {
				res.Err = ErrCancelled
			}
		}
		res.Took = time.Since(start)
		if ferr := e.State.Finish(context.WithoutCancel(ctx), runID, res.Rows, res.Err); ferr != nil {
			e.log().Warn("state write failed", "stream", s.Name, "err", ferr)
		}
		if res.Err != nil {
			e.emit(Event{Stream: s.Name, Object: res.Object, Phase: PhaseFailed, Rows: res.Rows, Err: res.Err})
		} else {
			e.emit(Event{Stream: s.Name, Object: res.Object, Phase: PhaseDone, Rows: res.Rows})
		}
	}()
	e.phase(ctx, runID, s, res.Object, PhaseQuerying, 0)
	if s.Mode == "backfill" && len(s.PrimaryKey) == 0 {
		e.log().Warn("backfill without primary_key: reruns append duplicates", "stream", s.Name)
	}

	var timeout time.Duration
	if s.QueryTimeout != "" {
		if timeout, err = time.ParseDuration(s.QueryTimeout); err != nil {
			res.Err = fmt.Errorf("query_timeout: %w", err)
			return
		}
	}
	exists, err := tgt.TableExists(ctx, target)
	if err != nil {
		res.Err = err
		return
	}
	src, err := open(ctx, s, target, exists, runID, timeout)
	if err != nil {
		res.Err = err
		return
	}
	defer src.close()

	cols := src.cols
	switch {
	case len(cols) == 0: // API stream without records
		if !exists || (s.Mode != "full-refresh" && s.Mode != "truncate") {
			return
		}
		// full-refresh and truncate empty the target; its columns stand in
		if cols, err = tgt.Columns(ctx, target); err != nil {
			res.Err = err
			return
		}
	case s.Mode == "snapshot":
		cols = append(slices.Clone(cols), db.Column{Name: loadedAtColumn, Kind: db.Timestamp})
	}
	keys, err := columnNames(cols, s.PrimaryKey)
	if err != nil {
		res.Err = fmt.Errorf("primary_key: %w", err)
		return
	}
	uk := ""
	if s.UpdateKey != "" {
		if k, err := columnNames(cols, []string{s.UpdateKey}); err == nil {
			uk = k[0]
		}
	}

	dedupe := len(keys) > 0
	load := cols
	if dedupe {
		load = append(slices.Clone(cols), db.Column{Name: seqColumn, Kind: db.Int})
	}
	tmp := auxTable(target, "tmp", r.Name)
	if err := tgt.DropTable(ctx, tmp); err != nil {
		res.Err = err
		return
	}
	sized := keys
	if uk != "" {
		sized = append(slices.Clone(keys), uk)
	}
	if err := tgt.CreateTable(ctx, tmp, load, sized); err != nil {
		res.Err = err
		return
	}
	defer func() {
		if err := tgt.DropTable(context.WithoutCancel(ctx), tmp); err != nil {
			e.log().Warn("temp table not dropped", "table", tmp.String(), "err", err)
		}
	}()

	ins := tgt.NewInserter(tmp, load)
	ins.Label = res.Object
	loadedAt := time.Now().UTC()
	lastProgress := time.Now()
	var seq int64
	e.phase(ctx, runID, s, res.Object, PhaseLoading, 0)
	for {
		if err := ctx.Err(); err != nil {
			res.Err = err
			return
		}
		row, err := src.next()
		if err != nil {
			res.Err = fmt.Errorf("read %s: %w", s.Name, err)
			return
		}
		if row == nil {
			break
		}
		if s.Mode == "snapshot" {
			row = append(row, loadedAt)
		}
		if dedupe {
			seq++
			row = append(row, seq)
		}
		if err := ins.Add(ctx, row); err != nil {
			res.Err = err
			return
		}
		if time.Since(lastProgress) > 500*time.Millisecond {
			lastProgress = time.Now()
			e.phase(ctx, runID, s, res.Object, PhaseLoading, ins.Total)
		}
	}
	if err := ins.Flush(ctx); err != nil {
		res.Err = err
		return
	}
	res.Rows = ins.Total

	e.phase(ctx, runID, s, res.Object, PhaseMerging, res.Rows)
	if dedupe {
		if err := dedupeTable(ctx, tgt, tmp, keys, uk); err != nil {
			res.Err = err
			return
		}
	}
	res.Err = e.finalize(ctx, tgt, s, r.Name, tmp, target, cols, keys, uk, exists, dedupe)
	return
}

// watchCancel returns a context that ends when State.Cancel is called for runID, with
// cause ErrCancelled, or when ctx ends. stop ends the watch.
func (e *Engine) watchCancel(ctx context.Context, runID int64) (_ context.Context, stop func()) {
	sctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(cmp.Or(e.CancelPoll, time.Second))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-sctx.Done():
				return
			case <-t.C:
				if ok, err := e.State.CancelRequested(sctx, runID); err == nil && ok {
					cancel(ErrCancelled)
					return
				}
			}
		}
	}()
	return sctx, func() {
		close(done)
		cancel(nil)
	}
}

// dedupeTable keeps one row per key in t: the highest update key, then the last loaded.
func dedupeTable(ctx context.Context, tgt *db.DB, t db.Table, keys []string, uk string) error {
	var one int
	err := tgt.QueryRowContext(ctx, db.DuplicatesSQL(tgt.Dialect, t, keys)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", tgt.Name, err)
	}
	return tgt.Exec(ctx, tgt.Dialect.Dedupe(t, keys, uk, seqColumn))
}

// columnNames maps names to the columns' own spelling, ignoring case.
func columnNames(cols []db.Column, names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		i := slices.IndexFunc(cols, func(c db.Column) bool { return strings.EqualFold(c.Name, n) })
		if i < 0 {
			return nil, fmt.Errorf("no column %q", n)
		}
		out = append(out, cols[i].Name)
	}
	return out, nil
}

// auxTable names a helper table or index for t: _tummy_<kind>_<name>_<hash>, at most 63 bytes.
// salt separates replications writing the same target.
func auxTable(t db.Table, kind, salt string) db.Table {
	h := fnv.New32a()
	h.Write([]byte(kind + "\x00" + salt + "\x00" + t.String()))
	suffix := fmt.Sprintf("_%08x", h.Sum32())
	prefix := "_tummy_" + kind + "_"
	name := t.Name
	if room := 63 - len(prefix) - len(suffix); len(name) > room {
		name = name[:room]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	return db.Table{Schema: t.Schema, Name: prefix + name + suffix}
}

func targetTable(tgt *db.DB, s stream) db.Table {
	st := s.table
	obj := s.Object
	if obj == "" {
		switch {
		case s.SQL != "" || s.Path != "":
			obj = s.Name
		case !tgt.Dialect.HasSchemas() && st.Schema != "":
			obj = "{stream_schema}_{stream_table}"
		default:
			obj = "{stream_schema}.{stream_table}"
			if st.Schema == "" {
				obj = "{stream_table}"
			}
		}
	}
	rep := strings.NewReplacer(
		"{stream_schema}", st.Schema,
		"{stream_table}", st.Name,
		"{stream_name}", strings.ReplaceAll(s.Name, ".", "_"),
		"{target_schema}", tgt.Schema,
	)
	t := db.ParseTable(rep.Replace(obj))
	if !tgt.Dialect.HasSchemas() {
		t.Schema = ""
	} else if t.Schema == "" {
		t.Schema = tgt.Schema
	}
	return t
}

// finalize moves the temp table into the target according to mode.
func (e *Engine) finalize(ctx context.Context, tgt *db.DB, s stream, replication string, tmp, target db.Table,
	cols []db.Column, keys []string, uk string, exists, hasSeq bool) error {
	dl := tgt.Dialect
	swap := func() error {
		if hasSeq {
			if err := tgt.Exec(ctx, db.DropColumnSQL(dl, tmp, seqColumn)); err != nil {
				return err
			}
		}
		old := auxTable(target, "old", replication)
		if err := tgt.DropTable(ctx, old); err != nil {
			return err
		}
		stmts, inTx := dl.Swap(tmp, target, old, exists)
		return tgt.ExecAll(ctx, inTx, stmts...)
	}

	switch s.Mode {
	case "full-refresh":
		return swap()
	case "truncate":
		if !exists {
			return swap()
		}
		if err := addMissingColumns(ctx, tgt, target, cols); err != nil {
			return err
		}
		return tgt.ExecAll(ctx, true, dl.Truncate(target), db.InsertSelectSQL(dl, target, tmp, cols))
	case "incremental", "backfill", "snapshot":
		if !exists {
			if err := swap(); err != nil {
				return err
			}
			e.addIndexes(ctx, tgt, s, target, keys, uk)
			return nil
		}
		if err := addMissingColumns(ctx, tgt, target, cols); err != nil {
			return err
		}
		var stmts []string
		if len(keys) > 0 && s.Mode != "snapshot" {
			stmts = append(stmts, dl.DeleteMatching(target, tmp, keys))
		}
		return tgt.ExecAll(ctx, true, append(stmts, db.InsertSelectSQL(dl, target, tmp, cols))...)
	}
	return fmt.Errorf("unknown mode %q", s.Mode)
}

// addIndexes indexes a new incremental or backfill target on its keys and update key.
// Failures are logged, not returned.
func (e *Engine) addIndexes(ctx context.Context, tgt *db.DB, s stream, target db.Table, keys []string, uk string) {
	var sets [][]string
	if len(keys) > 0 && (s.Mode == "incremental" || s.Mode == "backfill") {
		sets = append(sets, keys)
	}
	if s.Mode == "incremental" && uk != "" && !(len(keys) == 1 && keys[0] == uk) {
		sets = append(sets, []string{uk})
	}
	for _, cols := range sets {
		name := auxTable(target, "ix", strings.Join(cols, ",")).Name
		if err := tgt.Exec(ctx, db.CreateIndexSQL(tgt.Dialect, name, target, cols)); err != nil {
			e.log().Warn("index not created", "table", target.String(), "columns", cols, "err", err)
		}
	}
}

func addMissingColumns(ctx context.Context, tgt *db.DB, target db.Table, cols []db.Column) error {
	have, err := tgt.Columns(ctx, target)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, c := range have {
		seen[strings.ToLower(c.Name)] = true
	}
	for _, c := range cols {
		if seen[strings.ToLower(c.Name)] {
			continue
		}
		if err := tgt.Exec(ctx, db.AddColumnSQL(tgt.Dialect, target, c)); err != nil {
			return err
		}
	}
	return nil
}
