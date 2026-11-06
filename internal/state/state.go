// Package state is the run history, request queue and daemon status database.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"tummy/internal/lock"
)

type Store struct {
	db   *sql.DB
	Path string
}

type Run struct {
	ID          int64
	Replication string
	Stream      string
	Object      string
	Trigger     string
	Started     time.Time
	Ended       *time.Time
	Rows        int64
	Phase       string // connecting, querying, loading, merging while running
	Status      string // running, success, error, skipped, cancelled
	Error       string
	Note        string // for_each: parents done of total and skips; kept after the run
}

// ErrCancelled ends a run that was cancelled; Finish records it as status cancelled.
var ErrCancelled = errors.New("cancelled")

// StatusWord is the word shown for a stored status: ok for success, failed for error.
func StatusWord(status string) string {
	switch status {
	case "success":
		return "ok"
	case "error":
		return "failed"
	}
	return status
}

// StatusOf is the stored status for a shown word: success for ok, error for failed.
func StatusOf(word string) string {
	switch word {
	case "ok":
		return "success"
	case "failed":
		return "error"
	}
	return word
}

// StreamLabel is the stream shown for a run: (replication) for the "*" row of a
// replication that failed or was skipped before any stream ran.
func StreamLabel(stream string) string {
	if stream == "*" {
		return "(replication)"
	}
	return stream
}

func (r Run) Duration() time.Duration {
	if r.Ended == nil {
		return time.Since(r.Started)
	}
	return r.Ended.Sub(r.Started)
}

type Request struct {
	ID          int64
	Replication string
	Streams     []string
	Trigger     string // recorded on the runs; "" for requests queued without one
	Created     time.Time
}

type Daemon struct {
	PID       int
	Started   time.Time
	Heartbeat time.Time
	Home      string // absolute
}

// Alive reports whether the heartbeat is recent and the process exists.
func (d *Daemon) Alive() bool {
	return d != nil && time.Since(d.Heartbeat) < 6*time.Second && pidAlive(d.PID)
}

// Serves reports whether the daemon runs the replications of home.
func (d *Daemon) Serves(home string) bool {
	return d != nil && sameDir(d.Home, home)
}

func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && aa == bb
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

type Schedule struct {
	Replication string
	Schedule    string
	Enabled     bool
	NextRun     *time.Time
	LastFire    *time.Time // last schedule-triggered start
	Error       string     // load or schedule error seen by the daemon
}

// DefaultPath is TUMMY_STATE, else XDG_STATE_HOME/tummy/state.db, else
// ~/.local/state/tummy/state.db.
func DefaultPath() string {
	if p := os.Getenv("TUMMY_STATE"); p != "" {
		return p
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "tummy", "state.db")
}

const schemaVersion = 5

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id INTEGER PRIMARY KEY,
  replication TEXT NOT NULL,
  stream TEXT NOT NULL,
  object TEXT NOT NULL DEFAULT '',
  trigger TEXT NOT NULL DEFAULT 'cli',
  phase TEXT NOT NULL DEFAULT '',
  started TEXT NOT NULL,
  ended TEXT,
  rows INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  cancel INTEGER NOT NULL DEFAULT 0,
  note TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS requests (
  id INTEGER PRIMARY KEY,
  replication TEXT NOT NULL,
  streams TEXT NOT NULL DEFAULT '',
  trigger TEXT NOT NULL DEFAULT '',
  created TEXT NOT NULL,
  taken TEXT
);
CREATE TABLE IF NOT EXISTS daemon (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  pid INTEGER NOT NULL,
  started TEXT NOT NULL,
  heartbeat TEXT NOT NULL,
  home TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS schedules (
  replication TEXT PRIMARY KEY,
  schedule TEXT NOT NULL,
  enabled INTEGER NOT NULL,
  next_run TEXT
);
CREATE TABLE IF NOT EXISTS table_stats (
  id INTEGER PRIMARY KEY,
  conn TEXT NOT NULL,
  schema_name TEXT NOT NULL,
  table_name TEXT NOT NULL,
  refreshed TEXT NOT NULL,
  full INTEGER NOT NULL,
  rows INTEGER,
  bytes INTEGER,
  exact INTEGER NOT NULL
);`

const indexes = `
CREATE INDEX IF NOT EXISTS runs_rep ON runs(replication, stream, id);
CREATE INDEX IF NOT EXISTS runs_started ON runs(started);
CREATE INDEX IF NOT EXISTS runs_running ON runs(replication) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS requests_pending ON requests(id) WHERE taken IS NULL;
CREATE INDEX IF NOT EXISTS table_stats_conn ON table_stats(conn, schema_name, table_name, refreshed);
CREATE INDEX IF NOT EXISTS table_stats_full ON table_stats(full, conn, refreshed);`

// Open opens or creates the database. A new file is 0600 in a 0700 directory.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// migrate brings any earlier layout up to schemaVersion inside one write transaction, so
// two processes opening an old file do not both alter it.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	for table, cols := range map[string][][2]string{
		"runs":      {{"object", "TEXT NOT NULL DEFAULT ''"}, {"trigger", "TEXT NOT NULL DEFAULT 'cli'"}, {"phase", "TEXT NOT NULL DEFAULT ''"}, {"cancel", "INTEGER NOT NULL DEFAULT 0"}, {"note", "TEXT NOT NULL DEFAULT ''"}},
		"requests":  {{"trigger", "TEXT NOT NULL DEFAULT ''"}},
		"schedules": {{"error", "TEXT NOT NULL DEFAULT ''"}, {"last_fire", "TEXT"}},
	} {
		have, err := columns(ctx, tx, table)
		if err != nil {
			return err
		}
		for _, c := range cols {
			if !have[c[0]] {
				if _, err := tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD "+c[0]+" "+c[1]); err != nil {
					return err
				}
			}
		}
	}
	if _, err := tx.ExecContext(ctx, indexes); err != nil {
		return err
	}
	if v < 2 {
		if err := fixTimestamps(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func columns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

// fixTimestamps rewrites variable-width RFC 3339 timestamps to the fixed width ts writes,
// so they compare correctly as text.
func fixTimestamps(ctx context.Context, tx *sql.Tx) error {
	for table, cols := range map[string][]string{
		"runs":      {"started", "ended"},
		"requests":  {"created", "taken"},
		"daemon":    {"started", "heartbeat"},
		"schedules": {"next_run", "last_fire"},
	} {
		for _, col := range cols {
			rows, err := tx.QueryContext(ctx, "SELECT rowid, "+col+" FROM "+table+" WHERE "+col+" IS NOT NULL")
			if err != nil {
				return err
			}
			fix := map[int64]string{}
			for rows.Next() {
				var id int64
				var v string
				if err := rows.Scan(&id, &v); err != nil {
					rows.Close()
					return err
				}
				if t, err := time.Parse(time.RFC3339Nano, v); err == nil && ts(t) != v {
					fix[id] = ts(t)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for id, v := range fix {
				if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET "+col+" = ? WHERE rowid = ?", v, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// tsLayout is fixed width, so text order is time order.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad timestamp %q", s)
	}
	return t, nil
}

func parseNullTS(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTS(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ---------- runs ----------

func (s *Store) Start(ctx context.Context, replication, stream, object, trigger string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO runs (replication, stream, object, trigger, started, status) VALUES (?, ?, ?, ?, ?, 'running')",
		replication, stream, object, trigger, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Progress(ctx context.Context, id, rows int64, phase string, note ...string) error {
	if len(note) > 0 {
		_, err := s.db.ExecContext(ctx, "UPDATE runs SET rows = ?, phase = ?, note = ? WHERE id = ?", rows, phase, note[0], id)
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE runs SET rows = ?, phase = ? WHERE id = ?", rows, phase, id)
	return err
}

// Finish ends a run: success without runErr, cancelled when runErr is ErrCancelled,
// else error.
func (s *Store) Finish(ctx context.Context, id int64, rows int64, runErr error) error {
	status, msg := "success", ""
	switch {
	case errors.Is(runErr, ErrCancelled):
		// "cancelled: interrupted" keeps the reason only
		status = "cancelled"
		msg = strings.TrimPrefix(strings.TrimPrefix(runErr.Error(), ErrCancelled.Error()), ": ")
	case runErr != nil:
		status, msg = "error", runErr.Error()
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE runs SET ended = ?, rows = ?, status = ?, error = ?, phase = '' WHERE id = ?",
		ts(time.Now()), rows, status, msg, id)
	return err
}

// Cancel asks the process running a run to stop it. The engine checks between batches;
// the run ends with status cancelled.
func (s *Store) Cancel(ctx context.Context, runID int64) error {
	res, err := s.db.ExecContext(ctx, "UPDATE runs SET cancel = 1 WHERE id = ? AND status = 'running'", runID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("run %d is not running", runID)
	}
	return nil
}

// CancelRequested reports whether Cancel was called for a run.
func (s *Store) CancelRequested(ctx context.Context, runID int64) (bool, error) {
	var c int
	err := s.db.QueryRowContext(ctx, "SELECT cancel FROM runs WHERE id = ?", runID).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return c != 0, err
}

// record adds a finished row for something that did not get as far as a run.
func (s *Store) record(ctx context.Context, replication, stream, trigger, status, msg string) error {
	now := ts(time.Now())
	_, err := s.db.ExecContext(context.WithoutCancel(ctx),
		"INSERT INTO runs (replication, stream, trigger, started, ended, status, error) VALUES (?, ?, ?, ?, ?, ?, ?)",
		replication, stream, trigger, now, now, status, msg)
	return err
}

// Fail records a replication that failed before any stream started, as stream "*".
func (s *Store) Fail(ctx context.Context, replication, trigger string, err error) error {
	return s.record(ctx, replication, "*", trigger, "error", err.Error())
}

// Skip records a run that was not started, as stream "*" with status skipped.
func (s *Store) Skip(ctx context.Context, replication, trigger, reason string) error {
	return s.record(ctx, replication, "*", trigger, "skipped", reason)
}

// AbandonDead marks running rows as errors when no process holds their replication's
// lock in any of lockDirs, which means the process that started them is gone.
func (s *Store) AbandonDead(ctx context.Context, lockDirs ...string) (int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT replication FROM runs WHERE status = 'running'")
	if err != nil {
		return 0, err
	}
	var reps []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return 0, err
		}
		reps = append(reps, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, rep := range reps {
		k, err := s.abandon(ctx, rep, lockDirs)
		if err != nil {
			return n, err
		}
		n += k
	}
	return n, nil
}

func (s *Store) abandon(ctx context.Context, rep string, lockDirs []string) (int64, error) {
	for _, dir := range lockDirs {
		l, err := lock.Acquire(dir, rep)
		if err != nil {
			return 0, nil // held, or not lockable: the run may be alive
		}
		defer l.Release()
	}
	// rows still running while every lock is held here have no process behind them
	res, err := s.db.ExecContext(ctx,
		"UPDATE runs SET ended = ?, status = 'error', phase = '', error = 'process ended before the run finished' WHERE status = 'running' AND replication = ?",
		ts(time.Now()), rep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const runCols = "id, replication, stream, object, trigger, phase, started, ended, rows, status, error, note"

func scanRuns(rows *sql.Rows) ([]Run, error) {
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started string
		var ended sql.NullString
		if err := rows.Scan(&r.ID, &r.Replication, &r.Stream, &r.Object, &r.Trigger, &r.Phase, &started, &ended, &r.Rows, &r.Status, &r.Error, &r.Note); err != nil {
			return nil, err
		}
		var err error
		if r.Started, err = parseTS(started); err != nil {
			return nil, err
		}
		if r.Ended, err = parseNullTS(ended); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunFilter selects runs; zero fields match everything.
type RunFilter struct {
	Replication string
	Stream      string
	Status      string
	Trigger     string
	Since       time.Time
	Limit       int
}

// Find lists runs matching f, newest first.
func (s *Store) Find(ctx context.Context, f RunFilter) ([]Run, error) {
	var where []string
	var args []any
	for _, c := range []struct{ col, v string }{
		{"replication", f.Replication}, {"stream", f.Stream}, {"status", f.Status}, {"trigger", f.Trigger},
	} {
		if c.v != "" {
			where = append(where, c.col+" = ?")
			args = append(args, c.v)
		}
	}
	if !f.Since.IsZero() {
		where = append(where, "started >= ?")
		args = append(args, ts(f.Since))
	}
	q := "SELECT " + runCols + " FROM runs"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

func (s *Store) Recent(ctx context.Context, limit int) ([]Run, error) {
	return s.Find(ctx, RunFilter{Limit: limit})
}

// RunsFor lists runs of a replication, optionally one stream, newest first.
func (s *Store) RunsFor(ctx context.Context, replication, stream string, limit int) ([]Run, error) {
	return s.Find(ctx, RunFilter{Replication: replication, Stream: stream, Limit: limit})
}

// Latest returns the newest run per (replication, stream).
func (s *Store) Latest(ctx context.Context) (map[string]map[string]Run, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+runCols+" FROM runs WHERE id IN (SELECT MAX(id) FROM runs GROUP BY replication, stream)")
	if err != nil {
		return nil, err
	}
	runs, err := scanRuns(rows)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]Run{}
	for _, r := range runs {
		if out[r.Replication] == nil {
			out[r.Replication] = map[string]Run{}
		}
		out[r.Replication][r.Stream] = r
	}
	return out, nil
}

// LastScheduled is the start of the newest schedule-triggered row of a replication.
func (s *Store) LastScheduled(ctx context.Context, replication string) (time.Time, error) {
	var v sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT MAX(started) FROM runs WHERE replication = ? AND trigger = 'schedule'", replication).Scan(&v)
	if err != nil || !v.Valid {
		return time.Time{}, err
	}
	return parseTS(v.String)
}

// Prune deletes finished runs started before cutoff, except the newest per stream, and
// requests taken before cutoff.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM runs WHERE started < ? AND status != 'running' AND id NOT IN (SELECT MAX(id) FROM runs GROUP BY replication, stream)", ts(cutoff))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx, "DELETE FROM requests WHERE taken IS NOT NULL AND taken < ?", ts(cutoff))
	if err != nil {
		return n, err
	}
	k, _ := res.RowsAffected()
	return n + k, nil
}

// DeleteRuns forgets every run of one stream. A running one stays and is an error.
func (s *Store) DeleteRuns(ctx context.Context, replication, stream string) (int64, error) {
	var running int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE replication = ? AND stream = ? AND status = 'running'", replication, stream).Scan(&running); err != nil {
		return 0, err
	}
	if running > 0 {
		return 0, fmt.Errorf("%s › %s: running", replication, stream)
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM runs WHERE replication = ? AND stream = ?", replication, stream)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteRun forgets one run. A running one stays and is an error.
func (s *Store) DeleteRun(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM runs WHERE id = ? AND status != 'running'", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("run %d: running or gone", id)
	}
	return nil
}

// ---------- requests ----------

// Enqueue queues a run for the daemon. The runs record trigger; without one, "daemon".
func (s *Store) Enqueue(ctx context.Context, replication string, streams []string, trigger ...string) error {
	t := ""
	if len(trigger) > 0 {
		t = trigger[0]
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO requests (replication, streams, trigger, created) VALUES (?, ?, ?, ?)",
		replication, strings.Join(streams, ","), t, ts(time.Now()))
	return err
}

func scanRequests(rows *sql.Rows) ([]Request, error) {
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		var streams, created string
		if err := rows.Scan(&r.ID, &r.Replication, &streams, &r.Trigger, &created); err != nil {
			return nil, err
		}
		if streams != "" {
			r.Streams = strings.Split(streams, ",")
		}
		var err error
		if r.Created, err = parseTS(created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Take claims every pending request.
func (s *Store) Take(ctx context.Context) ([]Request, error) {
	rows, err := s.db.QueryContext(ctx,
		"UPDATE requests SET taken = ? WHERE taken IS NULL RETURNING id, replication, streams, trigger, created", ts(time.Now()))
	if err != nil {
		return nil, err
	}
	return scanRequests(rows)
}

// Expire claims pending requests created before cutoff and returns them, so they are
// not run long after they were made.
func (s *Store) Expire(ctx context.Context, cutoff time.Time) ([]Request, error) {
	rows, err := s.db.QueryContext(ctx,
		"UPDATE requests SET taken = ? WHERE taken IS NULL AND created < ? RETURNING id, replication, streams, trigger, created", ts(time.Now()), ts(cutoff))
	if err != nil {
		return nil, err
	}
	return scanRequests(rows)
}

// Release puts a taken request back in the queue.
func (s *Store) Release(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(context.WithoutCancel(ctx), "UPDATE requests SET taken = NULL WHERE id = ?", id)
	return err
}

func (s *Store) Pending(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT replication, count(*) FROM requests WHERE taken IS NULL GROUP BY 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var rep string
		var n int
		if err := rows.Scan(&rep, &n); err != nil {
			return nil, err
		}
		out[rep] = n
	}
	return out, rows.Err()
}

// ---------- daemon ----------

func (s *Store) Heartbeat(ctx context.Context, pid int, started time.Time, home string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO daemon (id, pid, started, heartbeat, home) VALUES (1, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET pid = excluded.pid, started = excluded.started, heartbeat = excluded.heartbeat, home = excluded.home`,
		pid, ts(started), ts(time.Now()), home)
	return err
}

// DaemonStatus returns the last heartbeat, nil if no daemon ever ran.
func (s *Store) DaemonStatus(ctx context.Context) (*Daemon, error) {
	var d Daemon
	var started, hb string
	err := s.db.QueryRowContext(ctx, "SELECT pid, started, heartbeat, home FROM daemon WHERE id = 1").Scan(&d.PID, &started, &hb, &d.Home)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if d.Started, err = parseTS(started); err != nil {
		return nil, err
	}
	if d.Heartbeat, err = parseTS(hb); err != nil {
		return nil, err
	}
	return &d, nil
}

// ---------- schedules ----------

// SetSchedules replaces the published schedules.
func (s *Store) SetSchedules(ctx context.Context, list []Schedule) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM schedules"); err != nil {
		return err
	}
	opt := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return ts(*t)
	}
	for _, sc := range list {
		if _, err := tx.ExecContext(ctx, "INSERT INTO schedules (replication, schedule, enabled, next_run, last_fire, error) VALUES (?, ?, ?, ?, ?, ?)",
			sc.Replication, sc.Schedule, sc.Enabled, opt(sc.NextRun), opt(sc.LastFire), sc.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Schedules(ctx context.Context) (map[string]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT replication, schedule, enabled, next_run, last_fire, error FROM schedules")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Schedule{}
	for rows.Next() {
		var sc Schedule
		var next, last sql.NullString
		if err := rows.Scan(&sc.Replication, &sc.Schedule, &sc.Enabled, &next, &last, &sc.Error); err != nil {
			return nil, err
		}
		if sc.NextRun, err = parseNullTS(next); err != nil {
			return nil, err
		}
		if sc.LastFire, err = parseNullTS(last); err != nil {
			return nil, err
		}
		out[sc.Replication] = sc
	}
	return out, rows.Err()
}
