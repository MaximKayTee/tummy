package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tummy/internal/lock"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func running(t *testing.T, s *Store) []Run {
	t.Helper()
	r, err := s.Find(context.Background(), RunFilter{Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunsLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id, err := s.Start(ctx, "sales", "orders", "orders", "cli")
	if err != nil {
		t.Fatal(err)
	}
	if r := running(t, s); len(r) != 1 || r[0].ID != id {
		t.Fatalf("running: %+v", r)
	}
	if err := s.Progress(ctx, id, 50, "loading"); err != nil {
		t.Fatal(err)
	}
	if r := running(t, s); r[0].Phase != "loading" || r[0].Rows != 50 {
		t.Fatalf("progress: %+v", r[0])
	}
	if err := s.Finish(ctx, id, 100, nil); err != nil {
		t.Fatal(err)
	}
	if r := running(t, s); len(r) != 0 {
		t.Fatalf("still running: %+v", r)
	}
	id2, _ := s.Start(ctx, "sales", "orders", "orders", "schedule")
	if err := s.Finish(ctx, id2, 0, errors.New("boom\nmore")); err != nil {
		t.Fatal(err)
	}
	id3, _ := s.Start(ctx, "sales", "customers", "customers", "manual")
	s.Finish(ctx, id3, 3, nil)

	recent, err := s.Recent(ctx, 10)
	if err != nil || len(recent) != 3 || recent[0].ID != id3 || recent[2].Rows != 100 || recent[2].Status != "success" {
		t.Fatalf("recent: %v %+v", err, recent)
	}
	latest, _ := s.Latest(ctx)
	o := latest["sales"]["orders"]
	if o.ID != id2 || o.Status != "error" || o.Error != "boom\nmore" || o.Trigger != "schedule" || o.Ended == nil {
		t.Fatalf("latest orders: %+v", o)
	}
	if latest["sales"]["customers"].Rows != 3 {
		t.Fatal("latest customers")
	}
	forStream, _ := s.RunsFor(ctx, "sales", "orders", 10)
	if len(forStream) != 2 || forStream[0].ID != id2 {
		t.Fatalf("runsFor: %+v", forStream)
	}
	errs, _ := s.Find(ctx, RunFilter{Status: "error", Since: time.Now().Add(-time.Hour)})
	if len(errs) != 1 || errs[0].ID != id2 {
		t.Fatalf("find error: %+v", errs)
	}
	if none, _ := s.Find(ctx, RunFilter{Since: time.Now().Add(time.Hour)}); len(none) != 0 {
		t.Fatalf("since: %+v", none)
	}
	last, err := s.LastScheduled(ctx, "sales")
	if err != nil || last.IsZero() || !last.Equal(forStream[0].Started) {
		t.Fatalf("last scheduled: %v %v", last, err)
	}
}

func TestSkipAndFail(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.Skip(ctx, "a", "schedule", "already running")
	s.Fail(ctx, "a", "manual", errors.New("no such file"))
	runs, _ := s.RunsFor(ctx, "a", "*", 10)
	if len(runs) != 2 || runs[0].Status != "error" || runs[1].Status != "skipped" || runs[1].Error != "already running" || runs[0].Ended == nil {
		t.Fatalf("%+v", runs)
	}
}

func TestAbandonDead(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	locks := filepath.Join(t.TempDir(), "locks")
	dead, _ := s.Start(ctx, "dead", "x", "", "cli")
	live, _ := s.Start(ctx, "live", "x", "", "cli")
	held, err := lock.Acquire(locks, "live")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	n, err := s.AbandonDead(ctx, locks)
	if err != nil || n != 1 {
		t.Fatalf("abandoned %d, %v", n, err)
	}
	d, _ := s.RunsFor(ctx, "dead", "x", 1)
	l, _ := s.RunsFor(ctx, "live", "x", 1)
	if d[0].ID != dead || d[0].Status != "error" || d[0].Ended == nil {
		t.Fatalf("dead: %+v", d[0])
	}
	if l[0].ID != live || l[0].Status != "running" {
		t.Fatalf("a run whose lock is held must stay running: %+v", l[0])
	}
}

func TestRequests(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.Enqueue(ctx, "sales", nil)
	s.Enqueue(ctx, "sales", []string{"orders", "daily"})
	s.Enqueue(ctx, "other", nil)
	p, _ := s.Pending(ctx)
	if p["sales"] != 2 || p["other"] != 1 {
		t.Fatalf("pending: %v", p)
	}
	reqs, err := s.Take(ctx)
	if err != nil || len(reqs) != 3 || reqs[0].Replication != "sales" || reqs[2].Replication != "other" {
		t.Fatalf("take: %v %+v", err, reqs)
	}
	if len(reqs[1].Streams) != 2 || reqs[1].Streams[1] != "daily" || reqs[0].Created.IsZero() {
		t.Fatalf("streams: %+v", reqs[1])
	}
	if again, _ := s.Take(ctx); len(again) != 0 {
		t.Fatal("taken twice")
	}
	if p, _ := s.Pending(ctx); len(p) != 0 {
		t.Fatal("still pending")
	}
	if err := s.Release(ctx, reqs[2].ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Pending(ctx); p["other"] != 1 {
		t.Fatalf("released request not pending: %v", p)
	}
	exp, err := s.Expire(ctx, time.Now().Add(time.Minute))
	if err != nil || len(exp) != 1 || exp[0].Replication != "other" {
		t.Fatalf("expire: %v %+v", err, exp)
	}
	if again, _ := s.Take(ctx); len(again) != 0 {
		t.Fatal("expired request taken")
	}
}

func TestTimestampsSortWithinASecond(t *testing.T) {
	a := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := a.Add(500 * time.Millisecond)
	if !(ts(a) < ts(b)) || len(ts(a)) != len(ts(b)) {
		t.Fatalf("%s %s", ts(a), ts(b))
	}
}

func TestPrune(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		id, _ := s.Start(ctx, "a", "x", "", "cli")
		s.Finish(ctx, id, 1, nil)
	}
	s.Enqueue(ctx, "a", nil)
	s.Take(ctx)
	n, err := s.Prune(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	runs, _ := s.Recent(ctx, 10)
	if len(runs) != 1 {
		t.Fatalf("the newest run per stream stays: %+v", runs)
	}
}

func TestDaemonAndSchedules(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if d, _ := s.DaemonStatus(ctx); d != nil || d.Alive() {
		t.Fatal("no daemon yet")
	}
	started := time.Now().Add(-time.Hour)
	home := t.TempDir()
	if err := s.Heartbeat(ctx, os.Getpid(), started, home); err != nil {
		t.Fatal(err)
	}
	d, _ := s.DaemonStatus(ctx)
	if d == nil || d.PID != os.Getpid() || !d.Alive() || d.Home != home || !d.Serves(home+"/.") || d.Serves(t.TempDir()) {
		t.Fatalf("daemon: %+v", d)
	}
	stale := &Daemon{PID: os.Getpid(), Heartbeat: time.Now().Add(-time.Minute)}
	if stale.Alive() {
		t.Fatal("stale heartbeat alive")
	}
	gone := &Daemon{PID: 1 << 30, Heartbeat: time.Now()}
	if gone.Alive() {
		t.Fatal("dead pid alive")
	}

	next := time.Now().Add(time.Minute).Truncate(time.Second)
	err := s.SetSchedules(ctx, []Schedule{
		{Replication: "a", Schedule: "* * * * *", Enabled: true, NextRun: &next, LastFire: &started},
		{Replication: "b", Schedule: "", Enabled: false, Error: "bad"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Schedules(ctx)
	if len(got) != 2 || !got["a"].NextRun.Equal(next) || !got["a"].LastFire.Equal(started) || got["b"].NextRun != nil || got["b"].Enabled || got["b"].Error != "bad" {
		t.Fatalf("schedules: %+v", got)
	}
	s.SetSchedules(ctx, nil)
	if got, _ := s.Schedules(ctx); len(got) != 0 {
		t.Fatal("not cleared")
	}
}

func TestOpenModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	fi, _ := os.Stat(filepath.Join(dir, "state.db"))
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("file %v dir %v", fi.Mode(), di.Mode())
	}
}

func TestMigrateOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// a v0 database: no user_version, no later columns, variable-width timestamps
	for _, q := range []string{
		"DROP INDEX runs_running",
		"ALTER TABLE runs DROP COLUMN object",
		"ALTER TABLE runs DROP COLUMN trigger",
		"ALTER TABLE runs DROP COLUMN phase",
		"ALTER TABLE runs DROP COLUMN cancel",
		"ALTER TABLE requests DROP COLUMN trigger",
		"ALTER TABLE schedules DROP COLUMN error",
		"ALTER TABLE schedules DROP COLUMN last_fire",
		"INSERT INTO runs (replication, stream, started, status) VALUES ('a', 'b', '2026-01-01T00:00:00Z', 'success')",
		"PRAGMA user_version = 0",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	// two processes opening the old file at once
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(path)
			if err == nil {
				s.Close()
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	id, err := s2.Start(context.Background(), "a", "b", "obj", "cli")
	if err != nil {
		t.Fatalf("after migrate: %v", err)
	}
	if err := s2.Cancel(context.Background(), id); err != nil {
		t.Fatalf("cancel after migrate: %v", err)
	}
	if err := s2.Enqueue(context.Background(), "a", nil, "tui"); err != nil {
		t.Fatalf("enqueue after migrate: %v", err)
	}
	var started string
	s2.db.QueryRow("SELECT started FROM runs WHERE id = 1").Scan(&started)
	if started != "2026-01-01T00:00:00.000000000Z" {
		t.Fatalf("timestamp not rewritten: %q", started)
	}
	var v int
	s2.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version %d", v)
	}
}

func TestCancelAndWords(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _ := s.Start(ctx, "r", "x", "", "manual")
	if ok, _ := s.CancelRequested(ctx, id); ok {
		t.Fatal("cancel before request")
	}
	if err := s.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CancelRequested(ctx, id); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	s.Finish(ctx, id, 3, fmt.Errorf("%w: interrupted", ErrCancelled))
	runs, _ := s.RunsFor(ctx, "r", "x", 1)
	if runs[0].Status != "cancelled" || runs[0].Error != "interrupted" || runs[0].Rows != 3 {
		t.Fatalf("%+v", runs[0])
	}
	if err := s.Cancel(ctx, id); err == nil {
		t.Fatal("cancel of a finished run")
	}
	id2, _ := s.Start(ctx, "r", "x", "", "manual")
	s.Finish(ctx, id2, 0, ErrCancelled)
	if runs, _ := s.RunsFor(ctx, "r", "x", 1); runs[0].Status != "cancelled" || runs[0].Error != "" {
		t.Fatalf("%+v", runs[0])
	}

	s.Enqueue(ctx, "r", []string{"x"}, "tui")
	s.Enqueue(ctx, "r", nil)
	reqs, err := s.Take(ctx)
	if err != nil || len(reqs) != 2 || reqs[0].Trigger != "tui" || reqs[1].Trigger != "" || reqs[0].Streams[0] != "x" {
		t.Fatalf("%+v %v", reqs, err)
	}

	for in, want := range map[string]string{"success": "ok", "error": "failed", "skipped": "skipped", "cancelled": "cancelled"} {
		if StatusWord(in) != want || StatusOf(want) != in {
			t.Errorf("%s: %s %s", in, StatusWord(in), StatusOf(want))
		}
	}
	if StreamLabel("*") != "(replication)" || StreamLabel("a.b") != "a.b" {
		t.Error("stream label")
	}
}
