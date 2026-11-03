package tui

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
	"tummy/internal/state"
)

// replication is one file in HOME/replications plus everything known about it.
type replication struct {
	Name     string
	Path     string
	Source   string
	Target   string
	Mode     string
	Schedule string
	Enabled  bool
	Streams  []config.ResolvedStream
	Err      string // the file does not load
	ConnErr  string // source or target is not in connections.toml
	SchedErr string // the daemon could not load or schedule it

	Last    map[string]state.Run // newest run per stream
	Running int
	Failed  int
	Skipped int
	Stopped int // cancelled
	OK      int
	Rows    int64 // rows of the newest run set
	LastAt  time.Time
	Next    *time.Time
	Pending int
}

type connRow struct {
	Name      string
	Type      string
	Target    string // passwords masked
	RateLimit string
	Schema    string
	Missing   string // sqlite: the file, when it does not exist
}

type snapshot struct {
	At     time.Time
	Reps   []replication
	Conns  []connRow
	Daemon *state.Daemon
	Err    error
}

type snapshotMsg snapshot

// fileCache keeps parsed files between refreshes. A file is parsed again when its
// modification time or size changes.
type fileCache struct {
	mu    sync.Mutex
	reps  map[string]cachedRep
	conns cachedConns
}

type stamp struct {
	mod  time.Time
	size int64
}

type cachedRep struct {
	stamp
	rep *config.Replication
	err error
}

type cachedConns struct {
	stamp
	path  string
	conns *config.Connections
	err   error
}

func newFileCache() *fileCache { return &fileCache{reps: map[string]cachedRep{}} }

func stampOf(path string) (stamp, error) {
	st, err := os.Stat(path)
	if err != nil {
		return stamp{}, err
	}
	return stamp{st.ModTime(), st.Size()}, nil
}

// replications parses the given files, reusing unchanged ones, and forgets the rest.
func (c *fileCache) replications(files []string) []cachedRep {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := map[string]cachedRep{}
	out := make([]cachedRep, len(files))
	for i, f := range files {
		s, err := stampOf(f)
		if e, ok := c.reps[f]; ok && err == nil && e.stamp == s {
			next[f], out[i] = e, e
			continue
		}
		r, err := config.LoadReplication(f)
		e := cachedRep{stamp: s, rep: r, err: err}
		next[f], out[i] = e, e
	}
	c.reps = next
	return out
}

func (c *fileCache) connections(path string) (*config.Connections, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, err := stampOf(path)
	if err == nil && c.conns.path == path && c.conns.stamp == s && c.conns.conns != nil {
		return c.conns.conns, c.conns.err
	}
	conns, err := config.LoadConnections(path)
	c.conns = cachedConns{stamp: s, path: path, conns: conns, err: err}
	if err != nil {
		c.conns.conns = nil
	}
	return conns, err
}

func (m Model) loadSnapshot() tea.Cmd {
	opts, st, cache := m.opts, m.opts.State, m.cache
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		snap := snapshot{At: time.Now()}

		files, err := config.ListReplicationFiles(opts.Home)
		if err != nil {
			snap.Err = err
			return snapshotMsg(snap)
		}
		latest, err := st.Latest(ctx)
		if err != nil {
			snap.Err = err
			return snapshotMsg(snap)
		}
		sched, err := st.Schedules(ctx)
		if err != nil {
			snap.Err = err
			return snapshotMsg(snap)
		}
		pending, err := st.Pending(ctx)
		if err != nil {
			snap.Err = err
			return snapshotMsg(snap)
		}
		snap.Daemon, err = st.DaemonStatus(ctx)
		if err != nil {
			snap.Err = err
			return snapshotMsg(snap)
		}

		for i, c := range cache.replications(files) {
			f := files[i]
			rep := replication{Path: f, Last: map[string]state.Run{}}
			if c.err != nil {
				rep.Name = config.NameOf(f)
				rep.Err = strings.TrimPrefix(c.err.Error(), f+": ")
			} else {
				r := c.rep
				rep.Name, rep.Source, rep.Target = r.Name, r.Source, r.Target
				rep.Mode, rep.Schedule, rep.Enabled = r.Mode, r.Schedule, r.IsEnabled()
				rep.Streams = r.Resolved
			}
			if sc, ok := sched[rep.Name]; ok {
				rep.Next, rep.SchedErr = sc.NextRun, sc.Error
			}
			rep.Pending = pending[rep.Name]
			runs := latest[rep.Name]
			if star, ok := runs["*"]; ok {
				// a replication-level row is history once a stream ran after it
				for stream, run := range runs {
					if stream != "*" && run.Started.After(star.Started) {
						runs = maps.Clone(runs)
						delete(runs, "*")
						break
					}
				}
			}
			for stream, run := range runs {
				rep.Last[stream] = run
				switch run.Status {
				case "running":
					rep.Running++
				case "error":
					rep.Failed++
				case "skipped":
					rep.Skipped++
				case "cancelled":
					rep.Stopped++
				default:
					rep.OK++
				}
				rep.Rows += run.Rows
				if run.Started.After(rep.LastAt) {
					rep.LastAt = run.Started
				}
			}
			snap.Reps = append(snap.Reps, rep)
		}

		conns, err := cache.connections(opts.Connections)
		if err == nil {
			for _, n := range conns.Names() {
				c := conns.Conn[n]
				snap.Conns = append(snap.Conns, connRow{Name: n, Type: c.Type, Target: c.Target(), RateLimit: c.RateLimit, Schema: c.Schema, Missing: missingFile(c)})
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			snap.Conns = append(snap.Conns, connRow{Name: "(error)", Target: err.Error()})
		}
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			for i := range snap.Reps {
				r := &snap.Reps[i]
				if r.Err != "" {
					continue
				}
				var missing []string
				for _, n := range []string{r.Source, r.Target} {
					if _, ok := connsOf(conns)[n]; !ok && !slices.Contains(missing, n) {
						missing = append(missing, n)
					}
				}
				if len(missing) > 0 {
					r.ConnErr = "unknown connection " + strings.Join(missing, ", ")
				}
			}
		}
		return snapshotMsg(snap)
	}
}

func connsOf(c *config.Connections) map[string]*config.Conn {
	if c == nil {
		return nil
	}
	return c.Conn
}

// missingFile is a sqlite connection's file when it does not exist, else "".
func missingFile(c *config.Conn) string {
	if c == nil || c.Type != "sqlite" {
		return ""
	}
	x, err := c.Expanded()
	if err != nil {
		return ""
	}
	p := x.SQLitePath()
	if p == "" || p == ":memory:" || strings.HasPrefix(p, "file:") {
		return ""
	}
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return p
	}
	return ""
}

type runsMsg struct {
	Replication string
	Stream      string
	Runs        []state.Run
	Err         error
}

func (m Model) loadRuns(replication, stream string) tea.Cmd {
	st := m.opts.State
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runs, err := st.Find(ctx, state.RunFilter{Replication: replication, Stream: stream, Limit: 200})
		return runsMsg{Replication: replication, Stream: stream, Runs: runs, Err: err}
	}
}

// streamNames returns the stream names of a replication in file order, then streams that
// ran but are not in the file (wildcard matches). "*", a replication-level row, goes first.
func (r replication) streamNames() []string {
	names := make([]string, 0, len(r.Streams))
	for _, s := range r.Streams {
		names = append(names, s.Name)
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	var extra []string
	for n := range r.Last {
		if !seen[n] && n != "*" {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	names = append(names, extra...)
	if _, ok := r.Last["*"]; ok {
		names = append([]string{"*"}, names...)
	}
	return names
}

func (r replication) stream(name string) (config.ResolvedStream, bool) {
	for _, s := range r.Streams {
		if s.Name == name {
			return s, true
		}
	}
	return config.ResolvedStream{}, false
}
