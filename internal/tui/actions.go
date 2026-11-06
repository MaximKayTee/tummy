package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
	"tummy/internal/daemon"
	"tummy/internal/engine"
	"tummy/internal/lock"
	"tummy/internal/probe"
	"tummy/internal/state"
)

type statusMsg struct {
	Text  string
	IsErr bool
}

type runDoneMsg struct {
	Replication string
	Err         error
}

// planMsg is a dry run: what each stream would do, nothing written.
type planMsg struct {
	Replication string
	Rows        []engine.PlanRow
	Err         error
}

// planNow resolves the replication against its connections without writing. sample
// adds the test load: engine.SampleRows rows per stream, rolled back.
func (m Model) planNow(rep replication, sample bool) tea.Cmd {
	if rep.Err != "" {
		return func() tea.Msg { return planMsg{Replication: rep.Name, Err: errors.New(rep.Err)} }
	}
	if rep.ConnErr != "" {
		return func() tea.Msg { return planMsg{Replication: rep.Name, Err: errors.New(rep.ConnErr)} }
	}
	opts := m.opts
	word := "dry run: "
	if sample {
		word = "test load: "
	}
	return tea.Batch(status(word+rep.Name, false), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		r, err := config.LoadReplication(rep.Path)
		if err != nil {
			return planMsg{Replication: rep.Name, Err: err}
		}
		conns, err := config.LoadConnections(opts.Connections)
		if err != nil {
			return planMsg{Replication: rep.Name, Err: err}
		}
		e := &engine.Engine{Conns: conns, State: opts.State, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		plan := e.Plan
		if sample {
			plan = e.PlanSample
		}
		rows, err := plan(ctx, r)
		return planMsg{Replication: rep.Name, Rows: rows, Err: err}
	})
}

type connTestMsg struct {
	Name   string
	Err    error
	Took   time.Duration
	Tables []string // listed by the test, kept for explore
}

// daemonServes reports whether a live daemon runs this config home's replications.
func (m Model) daemonServes() bool {
	return m.snap.Daemon.Alive() && m.snap.Daemon.Serves(m.opts.Home)
}

// runNow queues a run with the daemon when one serves this home, else runs in this process.
func (m Model) runNow(rep replication, streams []string, detached bool) tea.Cmd {
	if rep.Err != "" {
		return status(rep.Name+": "+txt.FirstLine(rep.Err), true)
	}
	if rep.ConnErr != "" {
		return status(rep.Name+": "+rep.ConnErr, true)
	}
	opts := m.opts
	if m.daemonServes() {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := opts.State.Enqueue(ctx, rep.Name, streams, "tui"); err != nil {
				return statusMsg{Text: err.Error(), IsErr: true}
			}
			return statusMsg{Text: "queued: " + runLabel(rep.Name, streams)}
		}
	}
	if detached {
		return tea.Batch(status("running detached: "+runLabel(rep.Name, streams), false), m.runDetached(rep, streams))
	}
	ctx, cancel := context.WithCancel(context.Background())
	if m.cancels != nil {
		m.cancels[rep.Name] = cancel
	}
	return tea.Batch(
		status("running: "+runLabel(rep.Name, streams), false),
		func() tea.Msg {
			defer cancel()
			r, err := config.LoadReplication(rep.Path)
			if err != nil {
				_ = opts.State.Fail(ctx, rep.Name, "manual", errors.New(strings.TrimPrefix(err.Error(), rep.Path+": ")))
				return runDoneMsg{Replication: rep.Name, Err: err}
			}
			conns, err := config.LoadConnections(opts.Connections)
			if err != nil {
				_ = opts.State.Fail(ctx, rep.Name, "manual", err)
				return runDoneMsg{Replication: rep.Name, Err: err}
			}
			log, closeLog := runLogger(opts.State.Path)
			defer closeLog()
			e := &engine.Engine{
				Conns: conns, State: opts.State, Log: log,
				Trigger: "manual", LockDir: daemon.LockDir(opts.Home),
			}
			_, err = e.Run(ctx, r, streams)
			if errors.Is(err, lock.ErrBusy) {
				_ = opts.State.Skip(ctx, rep.Name, "manual", "already running")
			}
			return runDoneMsg{Replication: rep.Name, Err: err}
		},
	)
}

// runDetached starts the run as its own session with its output in tui.log, so it
// finishes when this process is gone. The run reports through the state database.
func (m Model) runDetached(rep replication, streams []string) tea.Cmd {
	opts := m.opts
	args := append(slices.Clone(opts.RunCommand[1:]), rep.Name)
	if len(streams) > 0 {
		args = append(args, "--streams", strings.Join(streams, ","))
	}
	return func() tea.Msg {
		c := exec.Command(opts.RunCommand[0], args...)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if f := openLog(opts.State.Path); f != nil {
			defer f.Close()
			c.Stdout, c.Stderr = f, f
		}
		if err := c.Start(); err != nil {
			_ = opts.State.Fail(context.Background(), rep.Name, "manual", err)
			return runDoneMsg{Replication: rep.Name, Err: err}
		}
		if err := c.Wait(); err != nil {
			return runDoneMsg{Replication: rep.Name, Err: errors.New("failed, see history")}
		}
		return runDoneMsg{Replication: rep.Name}
	}
}

func runLabel(name string, streams []string) string {
	if len(streams) == 0 {
		return name
	}
	return name + " (" + strings.Join(streams, ", ") + ")"
}

func status(text string, isErr bool) tea.Cmd {
	return func() tea.Msg { return statusMsg{Text: text, IsErr: isErr} }
}

// logLimit is the size at which tui.log starts over.
const logLimit = 5 << 20

// runLogger writes engine output to tui.log next to the state database, off the screen.
// The returned func closes the file.
func runLogger(statePath string) (*slog.Logger, func()) {
	f := openLog(statePath)
	if f == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}
	}
	return slog.New(slog.NewTextHandler(f, nil)), func() { f.Close() }
}

// openLog opens tui.log next to the state database for appending, truncated past
// logLimit; nil when it cannot be opened.
func openLog(statePath string) *os.File {
	path := filepath.Join(filepath.Dir(statePath), "tui.log")
	flags := os.O_CREATE | os.O_APPEND | os.O_WRONLY
	if st, err := os.Stat(path); err == nil && st.Size() > logLimit {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil
	}
	return f
}

func (m Model) toggleEnabled(rep replication) tea.Cmd {
	return func() tea.Msg {
		if err := config.SetEnabled(rep.Path, !rep.Enabled); err != nil {
			return statusMsg{Text: err.Error(), IsErr: true}
		}
		if rep.Enabled {
			return statusMsg{Text: rep.Name + ": disabled"}
		}
		return statusMsg{Text: rep.Name + ": enabled"}
	}
}

func (m Model) testConn(name string) tea.Cmd {
	opts := m.opts
	return func() tea.Msg {
		conns, err := config.LoadConnections(opts.Connections)
		if err != nil {
			return connTestMsg{Name: name, Err: err}
		}
		c, ok := conns.Conn[name]
		if !ok {
			return connTestMsg{Name: name, Err: errors.New("no connection " + name)}
		}
		res := probeNow(c)
		return connTestMsg{Name: name, Err: res.Err, Took: res.Took, Tables: res.Tables}
	}
}

func describeRunErr(err error) string {
	switch {
	case errors.Is(err, lock.ErrBusy):
		return "already running"
	case errors.Is(err, state.ErrCancelled), errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return txt.FirstLine(err.Error())
}

// cancelRuns stops the given runs: directly when this process runs them, else through
// the state database the running process polls.
func (m Model) cancelRuns(rep string, ids []int64) tea.Cmd {
	st := m.opts.State
	if cancel := m.cancels[rep]; cancel != nil {
		cancel()
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, id := range ids {
			if err := st.Cancel(ctx, id); err != nil {
				return statusMsg{Text: err.Error(), IsErr: true}
			}
		}
		return statusMsg{Text: rep + ": cancel sent"}
	}
}

// missingFile is the file of a sqlite connection when it does not exist.
func (m Model) missingFile(name string) string {
	for _, c := range m.snap.Conns {
		if c.Name == name {
			return c.Missing
		}
	}
	return ""
}

// createFile makes an empty sqlite file for a connection, then tests it.
func (m Model) createFile(name, path string) tea.Cmd {
	test := m.testConn(name)
	return func() tea.Msg {
		if err := probe.CreateSQLite(path); err != nil {
			return connTestMsg{Name: name, Err: err}
		}
		return test()
	}
}

// editDraft loads a replication as written, changes it and writes it back.
func (m Model) editDraft(rep replication, done string, change func(d *config.ReplicationDraft) error) tea.Cmd {
	home := m.opts.Home
	return func() tea.Msg {
		r, err := config.LoadReplicationRaw(rep.Path)
		if err != nil {
			return statusMsg{Text: err.Error(), IsErr: true}
		}
		d := config.DraftOf(r)
		if err := change(&d); err != nil {
			return statusMsg{Text: err.Error(), IsErr: true}
		}
		if _, err := config.WriteReplication(home, d); err != nil {
			return statusMsg{Text: strings.TrimPrefix(err.Error(), rep.Path+": "), IsErr: true}
		}
		return deletedMsg{Text: done}
	}
}

// toggleStream turns one stream of the file on or off.
func (m Model) toggleStream(rep replication, stream string) tea.Cmd {
	on := false
	if s, ok := rep.stream(stream); ok {
		on = s.Disabled
	}
	word := "off"
	if on {
		word = "on"
	}
	return m.editDraft(rep, rep.Name+" › "+stream+": "+word, func(d *config.ReplicationDraft) error {
		for i := range d.Streams {
			if d.Streams[i].Name == stream {
				d.Streams[i].Stream.Disabled = !on
				return nil
			}
		}
		return errors.New(stream + ": not in " + rep.Name + ".toml")
	})
}

// deleteStream takes one stream out of the file.
func (m Model) deleteStream(rep replication, stream string) tea.Cmd {
	return m.editDraft(rep, "deleted stream "+stream, func(d *config.ReplicationDraft) error {
		for i := range d.Streams {
			if d.Streams[i].Name == stream {
				if len(d.Streams) == 1 {
					return errors.New(stream + " is the only stream: delete " + rep.Name + ".toml instead")
				}
				d.Streams = append(d.Streams[:i], d.Streams[i+1:]...)
				return nil
			}
		}
		return errors.New(stream + ": not in " + rep.Name + ".toml")
	})
}

// excludeStream adds a table matched by a pattern to the file's exclude list.
func (m Model) excludeStream(rep replication, stream string) tea.Cmd {
	return m.editDraft(rep, "excluded "+stream, func(d *config.ReplicationDraft) error {
		if slices.Contains(d.Exclude, stream) {
			return errors.New(stream + ": already excluded")
		}
		d.Exclude = append(d.Exclude, stream)
		return nil
	})
}

// forgetRuns deletes the run history of a stream that is no longer in the file.
func (m Model) forgetRuns(rep, stream string) tea.Cmd {
	st := m.opts.State
	return func() tea.Msg {
		if st == nil {
			return statusMsg{Text: "no state database", IsErr: true}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		n, err := st.DeleteRuns(ctx, rep, stream)
		if err != nil {
			return statusMsg{Text: err.Error(), IsErr: true}
		}
		return deletedMsg{Text: fmt.Sprintf("forgot %d runs of %s", n, stream)}
	}
}

// deleteBookmark removes one bookmark from bookmarks.toml.
func (m Model) deleteBookmark(name string) tea.Cmd {
	home := m.opts.Home
	return func() tea.Msg {
		all, err := config.LoadBookmarks(config.BookmarksPath(home))
		if err != nil {
			return statusMsg{Text: err.Error(), IsErr: true}
		}
		delete(all.Bookmark, name)
		if err := config.WriteBookmarks(all.Path, all); err != nil {
			return statusMsg{Text: err.Error(), IsErr: true}
		}
		return deletedMsg{Text: "deleted bookmark " + name}
	}
}

// forgetRun deletes one run from the history; the one before it shows as the last.
func (m Model) forgetRun(run state.Run) tea.Cmd {
	st := m.opts.State
	return func() tea.Msg {
		if st == nil {
			return statusMsg{Text: "no state database", IsErr: true}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := st.DeleteRun(ctx, run.ID); err != nil {
			return statusMsg{Text: err.Error(), IsErr: true}
		}
		return deletedMsg{Text: "forgot run of " + run.Stream + " " + run.Started.Local().Format("2006-01-02 15:04")}
	}
}

// firstOf is cmp.Or.
//
// Deprecated: use cmp.Or.

// redact is config.Redact.
//
// Deprecated: use config.Redact or (*config.Conn).Target.

func userHome() (string, error) { return os.UserHomeDir() }
