// Package daemon runs replications on their schedules and serves run requests.
package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"tummy/internal/config"
	"tummy/internal/engine"
	"tummy/internal/lock"
	"tummy/internal/state"
)

const (
	maxParallel = 4
	keepRuns    = 90 * 24 * time.Hour
	hookTimeout = time.Minute
)

type Daemon struct {
	Home        string
	Connections string // connections file; empty: config.ConnectionsPath(Home)
	State       *state.Store
	Log         *slog.Logger
	// Tick is the heartbeat, schedule check and request poll interval; ReloadEvery how
	// often files are re-read; requests queued more than RequestTTL before start are dropped.
	Tick        time.Duration
	ReloadEvery time.Duration
	RequestTTL  time.Duration

	reload   chan struct{}
	home     string // absolute
	lockDir  string
	started  time.Time
	files    map[string]*entry // by replication name; loop goroutine only
	conns    *config.Connections
	connsAt  string // mod time and size of the connections file when read
	fired    map[string]time.Time
	dirty    bool // schedules changed since the last publish
	wg       sync.WaitGroup
	sem      chan struct{}
	mu       sync.Mutex
	inflight map[string]bool
}

// entry is one replication file as the scheduler sees it.
type entry struct {
	path  string
	mod   time.Time
	size  int64
	rep   *config.Replication // nil when the file does not load
	err   string
	sched cron.Schedule
	next  time.Time
	last  time.Time // last schedule fire
}

type job struct {
	name, path string
	streams    []string
	trigger    string
	req        *state.Request
}

func New(home string, st *state.Store, log *slog.Logger) *Daemon {
	return &Daemon{
		Home:        home,
		State:       st,
		Log:         log,
		Tick:        2 * time.Second,
		ReloadEvery: 5 * time.Second,
		RequestTTL:  time.Minute,
		reload:      make(chan struct{}, 1),
		files:       map[string]*entry{},
		fired:       map[string]time.Time{},
		sem:         make(chan struct{}, maxParallel),
		inflight:    map[string]bool{},
	}
}

// LockDir holds the per-replication run locks of a config home: HOME/.locks.
// Given the path of a state database, as older callers pass, it returns the old
// location next to it.
func LockDir(home string) string {
	if fi, err := os.Stat(home); err == nil && fi.Mode().IsRegular() {
		return LegacyLockDir(home)
	}
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	return filepath.Join(home, ".locks")
}

// LegacyLockDir is where locks lived before LockDir took the home: next to the state
// database. Runs started by older callers hold their locks there.
func LegacyLockDir(statePath string) string { return filepath.Join(filepath.Dir(statePath), "locks") }

// Reload makes a running daemon re-read every file at its next chance.
func (d *Daemon) Reload() {
	select {
	case d.reload <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled. A second daemon on the same home or state file
// returns an error at once.
func (d *Daemon) Run(ctx context.Context) error {
	var err error
	if d.home, err = filepath.Abs(d.Home); err != nil {
		return err
	}
	if d.Connections == "" {
		d.Connections = config.ConnectionsPath(d.home)
	}
	d.lockDir = LockDir(d.home)
	held, err := lock.Acquire(d.lockDir, ".serve")
	if errors.Is(err, lock.ErrBusy) {
		return fmt.Errorf("tummy serve already running for %s", d.home)
	} else if err != nil {
		return err
	}
	defer held.Release()
	held2, err := lock.Acquire(filepath.Dir(d.State.Path), "serve")
	if errors.Is(err, lock.ErrBusy) {
		return fmt.Errorf("tummy serve already running on %s", d.State.Path)
	} else if err != nil {
		return err
	}
	defer held2.Release()

	d.started = time.Now()
	d.abandon(ctx)
	d.expire(ctx)
	d.prune(ctx)
	if sc, err := d.State.Schedules(ctx); err == nil {
		for name, s := range sc {
			if s.LastFire != nil {
				d.fired[name] = *s.LastFire
			}
		}
	}
	d.reloadFiles(ctx, true)
	d.publish(ctx)
	d.Log.Info("daemon started", "home", d.home, "pid", os.Getpid(), "replications", len(d.files))
	d.heartbeat(ctx)

	lastReload, lastPrune := time.Now(), time.Now()
	t := time.NewTicker(d.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.Log.Info("stopping")
			d.wg.Wait()
			return nil
		case <-d.reload:
			d.Log.Info("reload")
			d.reloadFiles(ctx, true)
			lastReload = time.Now()
		case <-t.C:
			d.heartbeat(ctx)
			if time.Since(lastReload) >= d.ReloadEvery {
				lastReload = time.Now()
				d.reloadFiles(ctx, false)
				d.abandon(ctx)
			}
			if time.Since(lastPrune) >= 24*time.Hour {
				lastPrune = time.Now()
				d.prune(ctx)
			}
			d.fireDue(ctx, time.Now().UTC())
			d.serveRequests(ctx)
		}
		d.publish(ctx)
	}
}

func (d *Daemon) heartbeat(ctx context.Context) {
	if err := d.State.Heartbeat(ctx, os.Getpid(), d.started, d.home); err != nil {
		d.Log.Warn("heartbeat", "err", err)
	}
}

// abandon marks runs whose process is gone as failed.
func (d *Daemon) abandon(ctx context.Context) {
	if n, err := d.State.AbandonDead(ctx, d.lockDir, LegacyLockDir(d.State.Path)); err != nil {
		d.Log.Warn("abandon", "err", err)
	} else if n > 0 {
		d.Log.Info("marked runs of ended processes as failed", "runs", n)
	}
}

// expire drops requests queued well before this daemon started.
func (d *Daemon) expire(ctx context.Context) {
	reqs, err := d.State.Expire(ctx, d.started.Add(-d.RequestTTL))
	if err != nil {
		d.Log.Warn("requests", "err", err)
		return
	}
	for _, r := range reqs {
		d.Log.Warn("request expired", "replication", r.Replication, "queued", r.Created)
		_ = d.State.Skip(ctx, r.Replication, requestTrigger(r), "request expired: queued "+r.Created.UTC().Format(time.DateTime)+" UTC with no daemon running")
	}
}

func (d *Daemon) prune(ctx context.Context) {
	if _, err := d.State.Prune(ctx, time.Now().Add(-keepRuns)); err != nil {
		d.Log.Warn("prune", "err", err)
	}
}

// reloadFiles re-reads files that changed, or all of them with force. Unchanged files
// keep their next fire time.
func (d *Daemon) reloadFiles(ctx context.Context, force bool) {
	files, err := config.ListReplicationFiles(d.home)
	if err != nil {
		d.Log.Warn("reload", "err", err)
		return
	}
	seen := map[string]bool{}
	changed := false
	var loaded []string
	for _, f := range files {
		name := config.NameOf(f)
		seen[name] = true
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		old := d.files[name]
		if !force && old != nil && old.mod.Equal(fi.ModTime()) && old.size == fi.Size() {
			continue
		}
		d.files[name] = d.load(ctx, name, f, fi)
		loaded = append(loaded, name)
		changed = true
	}
	for name := range d.files {
		if !seen[name] {
			delete(d.files, name)
			changed = true
		}
	}
	if changed {
		d.dirty = true
		scheduled := 0
		for _, e := range d.files {
			if e.sched != nil {
				scheduled++
			}
		}
		d.Log.Info("loaded", "replications", len(d.files), "scheduled", scheduled)
	}
	if d.reloadConns(force) {
		loaded = loaded[:0]
		for name := range d.files {
			loaded = append(loaded, name)
		}
	}
	d.checkConns(loaded)
}

// reloadConns re-reads the connections file when it changed, or with force. It reports
// whether it read it.
func (d *Daemon) reloadConns(force bool) bool {
	at := "missing"
	if fi, err := os.Stat(d.Connections); err == nil {
		at = fmt.Sprint(fi.ModTime().UnixNano(), fi.Size())
	}
	if !force && at == d.connsAt {
		return false
	}
	d.connsAt = at
	c, err := config.LoadConnectionsRaw(d.Connections)
	if err != nil {
		d.Log.Warn("connections", "err", err)
		c = nil
	}
	d.conns = c
	return true
}

// checkConns warns about replications whose source or target is not a connection.
func (d *Daemon) checkConns(names []string) {
	if d.conns == nil {
		return
	}
	sort.Strings(names)
	for _, name := range names {
		e := d.files[name]
		if e == nil || e.rep == nil {
			continue
		}
		for _, c := range []string{e.rep.Source, e.rep.Target} {
			if c != "" && d.conns.Conn[c] == nil {
				d.Log.Warn("unknown connection "+c, "replication", name)
			}
		}
	}
}

func (d *Daemon) load(ctx context.Context, name, path string, fi os.FileInfo) *entry {
	e := &entry{path: path, mod: fi.ModTime(), size: fi.Size(), last: d.lastFire(ctx, name)}
	r, err := config.LoadReplicationRaw(path)
	if err != nil {
		e.err = trimPath(err, path)
		d.Log.Warn("skipping", "file", path, "err", e.err)
		return e
	}
	// env and sql file problems: still scheduled, the run fails and is recorded
	if exp, err := config.LoadReplication(path); err != nil {
		e.err = trimPath(err, path)
		d.Log.Warn("load", "file", path, "err", e.err)
	} else {
		r = exp
	}
	e.rep = r
	if !r.IsEnabled() || r.Schedule == "" {
		return e
	}
	sched, err := config.ParseSchedule(r.Schedule)
	if err != nil {
		e.err = err.Error()
		d.Log.Warn("bad schedule", "file", path, "err", err)
		return e
	}
	e.sched = sched
	now := time.Now().UTC()
	if e.last.IsZero() {
		e.next = config.NextRun(sched, now)
	} else {
		// anchored to the last fire; a fire missed while no daemon ran is due at once
		e.next = config.NextRun(sched, e.last)
	}
	return e
}

func (d *Daemon) lastFire(ctx context.Context, name string) time.Time {
	if t, ok := d.fired[name]; ok {
		return t
	}
	t, err := d.State.LastScheduled(ctx, name)
	if err != nil {
		d.Log.Warn("history", "replication", name, "err", err)
	}
	return t
}

// fireDue starts every schedule whose next time has passed on the wall clock, once,
// however many fires were missed.
func (d *Daemon) fireDue(ctx context.Context, now time.Time) {
	names := make([]string, 0, len(d.files))
	for name := range d.files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := d.files[name]
		if e.sched == nil || now.Before(e.next) {
			continue
		}
		e.last = now
		d.fired[name] = now
		e.next = config.NextRun(e.sched, now)
		d.dirty = true
		d.start(ctx, job{name: name, path: e.path, trigger: "schedule"})
	}
}

// publish writes next-run times and load errors for the TUI and tummy status.
func (d *Daemon) publish(ctx context.Context) {
	if !d.dirty {
		return
	}
	var list []state.Schedule
	for name, e := range d.files {
		sc := state.Schedule{Replication: name, Error: e.err}
		if e.rep != nil {
			sc.Schedule, sc.Enabled = e.rep.Schedule, e.rep.IsEnabled()
		}
		if e.sched != nil {
			next := e.next
			sc.NextRun = &next
		}
		if !e.last.IsZero() {
			last := e.last
			sc.LastFire = &last
		}
		list = append(list, sc)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Replication < list[j].Replication })
	if err := d.State.SetSchedules(ctx, list); err != nil {
		d.Log.Warn("schedules", "err", err)
		return
	}
	d.dirty = false
}

func (d *Daemon) serveRequests(ctx context.Context) {
	reqs, err := d.State.Take(ctx)
	if err != nil {
		d.Log.Warn("requests", "err", err)
		return
	}
	for _, req := range reqs {
		trigger := requestTrigger(req)
		if !config.ValidName(req.Replication) {
			d.Log.Warn("request for an invalid name", "replication", req.Replication)
			_ = d.State.Skip(ctx, req.Replication, trigger, "invalid replication name")
			continue
		}
		path := filepath.Join(config.ReplicationsDir(d.home), req.Replication+".toml")
		if _, err := os.Stat(path); err != nil {
			d.Log.Warn("request for a missing file", "replication", req.Replication)
			_ = d.State.Skip(ctx, req.Replication, trigger, "no file "+path)
			continue
		}
		d.start(ctx, job{name: req.Replication, path: path, streams: req.Streams, trigger: trigger, req: &req})
	}
}

// requestTrigger is the trigger runs of a queued request record: the requester's, else daemon.
func requestTrigger(r state.Request) string { return cmp.Or(r.Trigger, "daemon") }

// start runs a job in the background. A replication already running here or in another
// process is skipped (schedule) or left queued (request).
func (d *Daemon) start(ctx context.Context, j job) {
	d.mu.Lock()
	busy := d.inflight[j.name] || !lock.Free(d.lockDir, j.name) || !lock.Free(LegacyLockDir(d.State.Path), j.name)
	if !busy {
		d.inflight[j.name] = true
	}
	d.mu.Unlock()
	if busy {
		d.busy(ctx, j)
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.inflight, j.name)
			d.mu.Unlock()
		}()
		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			if j.req != nil {
				_ = d.State.Release(ctx, j.req.ID)
			}
			return
		}
		defer func() { <-d.sem }()
		d.run(ctx, j)
	}()
}

func (d *Daemon) busy(ctx context.Context, j job) {
	if j.req != nil {
		d.Log.Debug("busy, request stays queued", "replication", j.name)
		_ = d.State.Release(ctx, j.req.ID)
		return
	}
	d.Log.Warn("skipped, already running", "replication", j.name)
	_ = d.State.Skip(ctx, j.name, j.trigger, "already running")
}

func (d *Daemon) run(ctx context.Context, j job) {
	r, err := config.LoadReplication(j.path)
	if err != nil {
		err = errors.New(trimPath(err, j.path))
		d.Log.Error("load", "replication", j.name, "err", err)
		_ = d.State.Fail(ctx, j.name, j.trigger, err)
		return
	}
	if unknown := r.UnknownStreams(j.streams); len(unknown) > 0 {
		msg := "no stream " + strings.Join(unknown, ", ")
		d.Log.Warn("request skipped", "replication", r.Name, "reason", msg)
		_ = d.State.Skip(ctx, r.Name, j.trigger, msg)
		return
	}
	conns, err := config.LoadConnections(d.Connections)
	if err != nil {
		d.Log.Error("connections", "err", err)
		_ = d.State.Fail(ctx, r.Name, j.trigger, err)
		d.hook(ctx, r, j.trigger, nil, err)
		return
	}
	e := &engine.Engine{Conns: conns, State: d.State, Log: d.Log, Trigger: j.trigger, LockDir: d.lockDir}
	d.Log.Info("run", "replication", r.Name, "trigger", j.trigger, "streams", j.streams)
	results, err := e.Run(ctx, r, j.streams)
	switch {
	case err == nil:
	case errors.Is(err, lock.ErrBusy):
		d.busy(ctx, j)
	case errors.Is(err, engine.ErrCancelled) && ctx.Err() == nil:
		d.Log.Info("run cancelled", "replication", r.Name)
	case ctx.Err() != nil:
		d.Log.Warn("run stopped", "replication", r.Name)
	default:
		d.Log.Error("run failed", "replication", r.Name, "err", err)
		d.hook(ctx, r, j.trigger, results, err)
	}
}

// hook runs the replication's on_error command with the failure in its environment.
func (d *Daemon) hook(ctx context.Context, r *config.Replication, trigger string, results []engine.Result, runErr error) {
	if r.OnError == "" {
		return
	}
	var streams, msgs []string
	for _, res := range results {
		if res.Err != nil {
			streams = append(streams, res.Stream)
			msgs = append(msgs, res.Stream+": "+res.Err.Error())
		}
	}
	if len(msgs) == 0 {
		msgs = []string{runErr.Error()}
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hookTimeout)
	defer cancel()
	cmd := exec.CommandContext(hctx, "/bin/sh", "-c", r.OnError)
	cmd.Dir = d.home
	cmd.Env = append(os.Environ(),
		"TUMMY_REPLICATION="+r.Name,
		"TUMMY_STREAMS="+strings.Join(streams, ","),
		"TUMMY_ERROR="+strings.Join(msgs, "\n"),
		"TUMMY_TRIGGER="+trigger,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		d.Log.Warn("on_error", "replication", r.Name, "err", err, "output", strings.TrimSpace(string(out)))
	}
}

func trimPath(err error, path string) string {
	return strings.TrimPrefix(err.Error(), path+": ")
}
