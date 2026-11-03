package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"tummy/internal/config"
	"tummy/internal/lock"
	"tummy/internal/state"
)

func home(t *testing.T) (string, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	src, err := sql.Open("sqlite", filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE things (id INTEGER PRIMARY KEY, v TEXT)",
		"INSERT INTO things VALUES (1,'a'),(2,'b')",
		"CREATE TABLE others (id INTEGER PRIMARY KEY)",
		"INSERT INTO others VALUES (1)",
	} {
		if _, err := src.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	src.Close()
	os.MkdirAll(config.ReplicationsDir(dir), 0o755)
	writeFile(t, filepath.Join(dir, "connections.toml"),
		"[conn.SRC]\ntype = \"sqlite\"\npath = \"src.db\"\n[conn.TGT]\ntype = \"sqlite\"\npath = \"tgt.db\"\n")
	rep(t, dir, "sched", "source = \"SRC\"\ntarget = \"TGT\"\nschedule = \"@every 1s\"\n[streams.things]\n")
	rep(t, dir, "manual", "source = \"SRC\"\ntarget = \"TGT\"\n[streams.things]\nobject = \"things_manual\"\n[streams.others]\n")
	rep(t, dir, "broken", "source = \"SRC\"\n")
	rep(t, dir, "badcron", "source = \"SRC\"\ntarget = \"TGT\"\nschedule = \"every 5 min\"\n[streams.things]\n")
	st, err := state.Open(filepath.Join(dir, "state", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return dir, st
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rep(t *testing.T, dir, name, body string) {
	writeFile(t, filepath.Join(config.ReplicationsDir(dir), name+".toml"), body)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func start(t *testing.T, dir string, st *state.Store) (*Daemon, context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := New(dir, st, quiet())
	d.Tick = 50 * time.Millisecond
	d.ReloadEvery = 150 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	return d, cancel, done
}

func wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func runs(st *state.Store, rep, stream string) []state.Run {
	r, _ := st.RunsFor(context.Background(), rep, stream, 50)
	return r
}

func TestDaemon(t *testing.T) {
	dir, st := home(t)
	ctx := context.Background()
	start(t, dir, st)

	wait(t, "heartbeat", func() bool { dm, _ := st.DaemonStatus(ctx); return dm.Alive() && dm.Serves(dir) })
	wait(t, "schedules", func() bool {
		sc, _ := st.Schedules(ctx)
		return sc["sched"].NextRun != nil && sc["manual"].Schedule == "" && sc["manual"].Enabled
	})
	sc, _ := st.Schedules(ctx)
	if !strings.Contains(sc["broken"].Error, "source and target are required") {
		t.Fatalf("load error not published: %+v", sc["broken"])
	}
	if !strings.Contains(sc["badcron"].Error, `schedule "every 5 min"`) || sc["badcron"].NextRun != nil {
		t.Fatalf("bad cron not published: %+v", sc["badcron"])
	}

	wait(t, "scheduled run", func() bool {
		r := runs(st, "sched", "things")
		return len(r) > 0 && r[0].Status == "success" && r[0].Trigger == "schedule"
	})

	// a request for a subset of streams
	st.Enqueue(ctx, "manual", []string{"others"})
	wait(t, "manual run", func() bool {
		r := runs(st, "manual", "others")
		return len(r) == 1 && r[0].Status == "success" && r[0].Trigger == "daemon"
	})
	st.Enqueue(ctx, "manual", []string{"others"}, "tui")
	wait(t, "run with the requester's trigger", func() bool {
		r := runs(st, "manual", "others")
		return len(r) == 2 && r[0].Status == "success" && r[0].Trigger == "tui"
	})
	if r := runs(st, "manual", "things"); len(r) != 0 {
		t.Fatalf("stream outside the request ran: %+v", r)
	}

	// requests that cannot run are recorded, not dropped silently
	st.Enqueue(ctx, "gone", nil)
	st.Enqueue(ctx, "../../x", nil)
	st.Enqueue(ctx, "manual", []string{"nope"})
	wait(t, "dropped requests recorded", func() bool {
		g, x, n := runs(st, "gone", "*"), runs(st, "../../x", "*"), runs(st, "manual", "*")
		return len(g) == 1 && g[0].Status == "skipped" && len(x) == 1 && x[0].Error == "invalid replication name" &&
			len(n) == 1 && n[0].Error == "no stream nope"
	})

	// disabling a file is picked up on reload
	config.SetEnabled(filepath.Join(config.ReplicationsDir(dir), "sched.toml"), false)
	wait(t, "reload after disable", func() bool {
		sc, _ := st.Schedules(ctx)
		return !sc["sched"].Enabled && sc["sched"].NextRun == nil
	})
}

func TestSecondDaemonRefused(t *testing.T) {
	dir, st := home(t)
	start(t, dir, st)
	wait(t, "heartbeat", func() bool { dm, _ := st.DaemonStatus(context.Background()); return dm.Alive() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := New(dir, st, quiet()).Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second daemon: %v", err)
	}
}

func TestBusyReplication(t *testing.T) {
	dir, st := home(t)
	ctx := context.Background()
	held, err := lock.Acquire(LockDir(dir), "sched")
	if err != nil {
		t.Fatal(err)
	}
	heldManual, _ := lock.Acquire(LockDir(dir), "manual")
	st.Enqueue(ctx, "manual", nil)
	start(t, dir, st)

	wait(t, "busy schedule skipped", func() bool {
		r := runs(st, "sched", "*")
		return len(r) > 0 && r[0].Status == "skipped" && r[0].Error == "already running" && r[0].Trigger == "schedule"
	})
	time.Sleep(300 * time.Millisecond)
	if p, _ := st.Pending(ctx); p["manual"] != 1 {
		t.Fatalf("a request for a busy replication must stay queued: %v", p)
	}
	heldManual.Release()
	held.Release()
	wait(t, "queued request runs once free", func() bool {
		r := runs(st, "manual", "things")
		return len(r) == 1 && r[0].Status == "success"
	})
}

func TestBrokenConnections(t *testing.T) {
	dir, st := home(t)
	writeFile(t, filepath.Join(dir, "connections.toml"), "[conn.SRC\n")
	marker := filepath.Join(dir, "hook.out")
	rep(t, dir, "manual", "source = \"SRC\"\ntarget = \"TGT\"\non_error = \"echo $TUMMY_REPLICATION $TUMMY_TRIGGER > "+marker+"\"\n[streams.things]\n")
	st.Enqueue(context.Background(), "manual", nil)
	start(t, dir, st)
	wait(t, "failure recorded", func() bool {
		r := runs(st, "manual", "*")
		return len(r) == 1 && r[0].Status == "error" && strings.Contains(r[0].Error, "connections.toml")
	})
	wait(t, "on_error ran", func() bool {
		b, _ := os.ReadFile(marker)
		return string(b) == "manual daemon\n"
	})
}

func TestStaleRequestsExpire(t *testing.T) {
	dir, st := home(t)
	ctx := context.Background()
	st.Enqueue(ctx, "manual", nil)
	time.Sleep(20 * time.Millisecond)
	cctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	d := New(dir, st, quiet())
	d.Tick = 50 * time.Millisecond
	d.RequestTTL = time.Millisecond
	if err := d.Run(cctx); err != nil {
		t.Fatal(err)
	}
	r := runs(st, "manual", "*")
	if len(r) != 1 || r[0].Status != "skipped" || !strings.Contains(r[0].Error, "request expired") {
		t.Fatalf("%+v", r)
	}
	if r := runs(st, "manual", "things"); len(r) != 0 {
		t.Fatal("stale request ran")
	}
}

func TestEveryAnchoredToLastFire(t *testing.T) {
	dir, st := home(t)
	ctx := context.Background()
	rep(t, dir, "sched", "source = \"SRC\"\ntarget = \"TGT\"\nschedule = \"@every 1h\"\n[streams.things]\n")
	id, _ := st.Start(ctx, "sched", "things", "", "schedule")
	st.Finish(ctx, id, 2, nil)
	last := runs(st, "sched", "things")[0].Started
	start(t, dir, st)
	wait(t, "next run anchored", func() bool {
		sc, _ := st.Schedules(ctx)
		return sc["sched"].NextRun != nil && sc["sched"].NextRun.Sub(last) < time.Hour+time.Second && sc["sched"].NextRun.Sub(last) > time.Hour-time.Second
	})
}

func TestMissedFireRunsOnStart(t *testing.T) {
	dir, st := home(t)
	ctx := context.Background()
	rep(t, dir, "sched", "source = \"SRC\"\ntarget = \"TGT\"\nschedule = \"0 3 * * *\"\n[streams.things]\n")
	st.SetSchedules(ctx, []state.Schedule{{Replication: "sched", Schedule: "0 3 * * *", Enabled: true, LastFire: ptr(time.Now().Add(-48 * time.Hour))}})
	start(t, dir, st)
	wait(t, "catch-up run", func() bool {
		r := runs(st, "sched", "things")
		return len(r) == 1 && r[0].Trigger == "schedule" && r[0].Status == "success"
	})
	time.Sleep(300 * time.Millisecond)
	if r := runs(st, "sched", "things"); len(r) != 1 {
		t.Fatalf("missed fires run once: %d runs", len(r))
	}
}

func TestAbandonsOnlyDeadRuns(t *testing.T) {
	dir, st := home(t)
	ctx := context.Background()
	dead, _ := st.Start(ctx, "manual", "things", "", "cli")
	live, _ := st.Start(ctx, "sched", "things", "", "cli")
	held, err := lock.Acquire(LockDir(dir), "sched")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	start(t, dir, st)
	wait(t, "dead run abandoned", func() bool {
		r := runs(st, "manual", "things")
		return len(r) > 0 && r[len(r)-1].ID == dead && r[len(r)-1].Status == "error"
	})
	for _, r := range runs(st, "sched", "things") {
		if r.ID == live && r.Status != "running" {
			t.Fatalf("live run touched: %+v", r)
		}
	}
}

func ptr[T any](v T) *T { return &v }

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestUnknownConnectionWarned(t *testing.T) {
	dir, st := home(t)
	rep(t, dir, "nocon", "source = \"SRC\"\ntarget = \"SHOP\"\n[streams.things]\n")
	var buf logBuf
	ctx, cancel := context.WithCancel(context.Background())
	d := New(dir, st, slog.New(slog.NewTextHandler(&buf, nil)))
	d.Tick = 50 * time.Millisecond
	d.ReloadEvery = 100 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); <-done }()
	wait(t, "warning at load", func() bool {
		return strings.Contains(buf.String(), `msg="unknown connection SHOP" replication=nocon`)
	})
	if strings.Contains(buf.String(), "unknown connection SRC") {
		t.Fatal("known connection reported")
	}
	writeFile(t, filepath.Join(dir, "connections.toml"), "[conn.SOURCE]\ntype = \"sqlite\"\npath = \"src.db\"\n[conn.TGT]\ntype = \"sqlite\"\npath = \"tgt.db\"\n")
	wait(t, "warning after connections.toml changed", func() bool {
		return strings.Contains(buf.String(), `msg="unknown connection SRC" replication=sched`)
	})
}
