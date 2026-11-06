package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
	"tummy/internal/state"
)

// detailOn opens the detail of rep and moves the cursor to stream.
func detailOn(t *testing.T, m Model, rep, stream string) Model {
	t.Helper()
	for i, r := range m.snap.Reps {
		if r.Name == rep {
			m.cursor[viewList] = i
		}
	}
	m = press(m, "enter")
	r, _ := m.rep(rep)
	for i, n := range r.streamNames() {
		if n == stream {
			m.cursor[viewDetail] = i
			return m
		}
	}
	t.Fatalf("%s › %s not listed: %v", rep, stream, r.streamNames())
	return m
}

func TestDeleteHistoryOnlyStreamForgetsRuns(t *testing.T) {
	opts := testHome(t)
	ctx := context.Background()
	id, _ := opts.State.Start(ctx, "alpha", "gone", "gone", "cli")
	opts.State.Finish(ctx, id, 3, nil)
	m := detailOn(t, loaded(t, opts), "alpha", "gone")
	if _, cmd := m.Update(keyRunes("e")); !strings.Contains(cmd().(statusMsg).Text, "run history only") {
		t.Fatalf("e: %+v", cmd())
	}
	m = press(m, "d")
	if m.ask == nil || !strings.HasPrefix(m.ask.text, "forget the runs of gone?") {
		t.Fatalf("ask: %+v", m.ask)
	}
	mm, cmd := m.Update(keyRunes("y"))
	m = mm.(Model)
	if d, ok := cmd().(deletedMsg); !ok || d.IsErr || d.Text != "forgot 1 runs of gone" {
		t.Fatalf("forget: %+v", cmd())
	}
	runs, _ := opts.State.RunsFor(ctx, "alpha", "gone", 10)
	if len(runs) != 0 {
		t.Fatalf("runs kept: %d", len(runs))
	}
	// a running stream is kept
	opts.State.Start(ctx, "alpha", "busy", "busy", "cli")
	if _, err := opts.State.DeleteRuns(ctx, "alpha", "busy"); err == nil {
		t.Fatal("running run deleted")
	}
}

func TestDeletePatternMatchExcludes(t *testing.T) {
	opts := testHome(t)
	path := filepath.Join(config.ReplicationsDir(opts.Home), "wild.toml")
	os.WriteFile(path, []byte("source = \"SRC\"\ntarget = \"TGT\"\n[streams.\"th*\"]\n"), 0o644)
	ctx := context.Background()
	id, _ := opts.State.Start(ctx, "wild", "things", "things", "cli")
	opts.State.Finish(ctx, id, 3, nil)
	m := detailOn(t, loaded(t, opts), "wild", "things")
	if _, cmd := m.Update(keyRunes("e")); !strings.Contains(cmd().(statusMsg).Text, "table pattern") {
		t.Fatalf("e: %+v", cmd())
	}
	m = press(m, "d")
	if m.ask == nil || !strings.HasPrefix(m.ask.text, "exclude things from wild?") {
		t.Fatalf("ask: %+v", m.ask)
	}
	_, cmd := m.Update(keyRunes("y"))
	if d, ok := cmd().(deletedMsg); !ok || d.IsErr {
		t.Fatalf("exclude: %+v", cmd())
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "exclude = [\"things\"]") || !strings.Contains(string(raw), "[streams.\"th*\"]") {
		t.Fatalf("file:\n%s", raw)
	}
	// twice is an error, the file unchanged
	m = detailOn(t, loaded(t, opts), "wild", "things")
	m = press(m, "d")
	_, cmd = m.Update(keyRunes("y"))
	if d, ok := cmd().(statusMsg); !ok || !d.IsErr {
		t.Fatalf("second exclude: %+v", cmd())
	}
}

func TestDeleteStreamOfBrokenFile(t *testing.T) {
	opts := testHome(t)
	ctx := context.Background()
	id, _ := opts.State.Start(ctx, "gamma", "x", "x", "cli")
	opts.State.Finish(ctx, id, 1, nil)
	m := detailOn(t, loaded(t, opts), "gamma", "x")
	mm, cmd := m.Update(keyRunes("d"))
	if m = mm.(Model); m.ask != nil || cmd == nil || cmd().(statusMsg).Text != "gamma.toml does not load" {
		t.Fatalf("broken: ask=%v cmd=%v", m.ask, cmd)
	}
}

func TestDetailRunKeysPickTheStream(t *testing.T) {
	opts := testHome(t)
	opts.RunCommand = []string{"sh", "-c", `[ "$1" = alpha ] && [ "$2" = --streams ] && [ "$3" = copy ] && echo ran >&2`, "tummy"}
	m := detailOn(t, loaded(t, opts), "alpha", "copy")
	// S: the selected stream, detached
	mm, cmd := m.Update(keyRunes("S"))
	m = mm.(Model)
	var done *runDoneMsg
	for _, msg := range runBatch(t, cmd) {
		if d, ok := msg.(runDoneMsg); ok {
			done = &d
		}
	}
	if done == nil || done.Err != nil {
		t.Fatalf("S detached run of copy: %+v", done)
	}
	// s: the selected stream, in process
	m = detailOn(t, loaded(t, opts), "alpha", "copy")
	_, cmd = m.Update(keyRunes("s"))
	var started string
	for _, msg := range runBatch(t, cmd) {
		if s, ok := msg.(statusMsg); ok && strings.HasPrefix(s.Text, "running: ") {
			started = s.Text
		}
	}
	if !strings.Contains(started, "copy") || strings.Contains(started, "things") {
		t.Fatalf("s runs the selected stream: %q", started)
	}
}

func TestForgetRunShowsThePreviousOne(t *testing.T) {
	opts := testHome(t)
	ctx := context.Background()
	good, _ := opts.State.Start(ctx, "alpha", "things", "things", "cli")
	opts.State.Finish(ctx, good, 3, nil)
	bad, _ := opts.State.Start(ctx, "alpha", "things", "things", "tui")
	opts.State.Finish(ctx, bad, 0, state.ErrCancelled)
	m := detailOn(t, loaded(t, opts), "alpha", "things")
	if r, _ := m.rep("alpha"); r.Last["things"].Status != "cancelled" {
		t.Fatalf("last: %+v", r.Last["things"])
	}
	m = press(m, "H")
	mm, cmd := m.Update(cmd0(m.loadRuns("alpha", "things")))
	m = mm.(Model)
	if len(m.runs) != 2 || m.runs[0].ID != bad {
		t.Fatalf("history: %+v", m.runs)
	}
	m = press(m, "d")
	if m.ask == nil || m.ask.text != "forget this run? y/n" {
		t.Fatalf("ask: %+v", m.ask)
	}
	mm, cmd = m.Update(keyRunes("y"))
	m = mm.(Model)
	d, ok := cmd().(deletedMsg)
	if !ok || d.IsErr {
		t.Fatalf("forget: %+v", cmd())
	}
	for _, msg := range runBatch(t, func() tea.Cmd { mm, c := m.Update(d); m = mm.(Model); return c }()) {
		mm, _ := m.Update(msg)
		m = mm.(Model)
	}
	if len(m.runs) != 1 || m.runs[0].ID != good {
		t.Fatalf("history after: %+v", m.runs)
	}
	if r, _ := m.rep("alpha"); r.Last["things"].ID != good || r.Last["things"].Status != "success" {
		t.Fatalf("last after: %+v", r.Last["things"])
	}
	// a running run is kept
	busy, _ := opts.State.Start(ctx, "alpha", "things", "things", "cli")
	if err := opts.State.DeleteRun(ctx, busy); err == nil {
		t.Fatal("running run deleted")
	}
	m = press(m, "esc", "H")
	mm, _ = m.Update(cmd0(m.loadRuns("alpha", "things")))
	m = mm.(Model)
	if _, cmd := m.Update(keyRunes("d")); m.ask != nil || !strings.Contains(cmd().(statusMsg).Text, "cancel first") {
		t.Fatalf("running: %+v", cmd())
	}
}

func cmd0(c tea.Cmd) tea.Msg { return c() }
