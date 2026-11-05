package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/lock"
	"tummy/internal/state"
)

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestQuitStopsInProcessRuns(t *testing.T) {
	m := loaded(t, testHome(t))
	stopped := false
	m.inflight["alpha"] = true
	m.cancels["alpha"] = func() { stopped = true }

	mm, cmd := m.quit("")
	m = mm.(Model)
	if cmd != nil || m.ask == nil || !strings.Contains(m.ask.text, "alpha running. stop and quit? y/n") {
		t.Fatalf("ask: %+v cmd=%v", m.ask, cmd)
	}
	msg := m.ask.onYes()()
	if _, ok := msg.(quitMsg); !ok {
		t.Fatalf("yes gives %T", msg)
	}
	mm, cmd = m.Update(msg)
	m = mm.(Model)
	if !stopped || !m.quitting || cmd == nil { // cmd is the grace tick, not run here
		t.Fatalf("after quitMsg: stopped=%v quitting=%v cmd=%v", stopped, m.quitting, cmd)
	}
	if m.status.Text != "stopping" {
		t.Fatalf("status %q", m.status.Text)
	}
	mm, cmd = m.Update(runDoneMsg{Replication: "alpha", Err: context.Canceled})
	m = mm.(Model)
	if !isQuit(cmd) || len(m.inflight) != 0 || len(m.cancels) != 0 {
		t.Fatalf("run end should quit: cmd=%v inflight=%v", cmd, m.inflight)
	}
	if _, cmd := m.Update(forceQuitMsg{}); !isQuit(cmd) {
		t.Fatal("forceQuit")
	}
}

func TestQuitWithoutRunsLeavesAtOnce(t *testing.T) {
	m := loaded(t, testHome(t))
	if _, cmd := m.quit(""); !isQuit(cmd) {
		t.Fatal("quit with nothing running should leave")
	}
	if _, cmd := m.Update(quitMsg{}); !isQuit(cmd) {
		t.Fatal("quitMsg with nothing running should leave")
	}
}

func TestCancelRunsStopsInProcessRun(t *testing.T) {
	m := loaded(t, testHome(t))
	stopped := false
	m.cancels["alpha"] = func() { stopped = true }
	msg := m.cancelRuns("alpha", nil)()
	if !stopped {
		t.Fatal("in-process run not cancelled")
	}
	if s, ok := msg.(statusMsg); !ok || s.Text != "alpha: cancel sent" {
		t.Fatalf("msg %+v", msg)
	}
}

func TestRunLabels(t *testing.T) {
	cases := map[string]string{
		phaseLabel("connecting", 0):  "connecting",
		phaseLabel("querying", 0):    "querying",
		phaseLabel("fetching", 1200): "fetching 1,200",
		phaseLabel("loading", 0):     "loading",
		phaseLabel("loading", 5000):  "loading 5,000",
		phaseLabel("merging", 0):     "merging",
		phaseLabel("", 0):            "running",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("phaseLabel: %q, want %q", got, want)
		}
	}
	r := replication{Last: map[string]state.Run{
		"done": {Status: "ok"},
		"b":    {Stream: "a\x1bb", Status: "running", Phase: "loading", Rows: 42},
	}}
	if got := runningLabel(r); got != `a\x1bb: loading 42` {
		t.Errorf("runningLabel: %q", got)
	}
	if got := runningLabel(replication{}); got != "starting" {
		t.Errorf("runningLabel empty: %q", got)
	}
	if describeRunErr(lock.ErrBusy) != "already running" ||
		describeRunErr(context.Canceled) != "cancelled" ||
		describeRunErr(state.ErrCancelled) != "cancelled" ||
		describeRunErr(errors.New("first\nsecond")) != "first" {
		t.Error("describeRunErr")
	}
}

func TestDetachedRun(t *testing.T) {
	opts := testHome(t)
	opts.RunCommand = []string{"sh", "-c", `[ "$1" = alpha ] && [ "$2" = --streams ] && [ "$3" = things ] && echo ran >&2`, "tummy"}
	m := loaded(t, opts)
	rep, _ := m.rep("alpha")
	mm, cmd := m.startRun(rep, []string{"things"}, true)
	m = mm.(Model)
	if !m.inflight["alpha"] || len(m.cancels) != 0 {
		t.Fatal("detached run: inflight, nothing to cancel")
	}
	var done *runDoneMsg
	for _, msg := range runBatch(t, cmd) {
		if d, ok := msg.(runDoneMsg); ok {
			done = &d
		}
	}
	if done == nil || done.Err != nil {
		t.Fatalf("done: %+v", done)
	}
	log, _ := os.ReadFile(filepath.Join(filepath.Dir(opts.State.Path), "tui.log"))
	if !strings.Contains(string(log), "ran") {
		t.Fatalf("child output in tui.log: %q", log)
	}
	// quitting with a detached run leaves at once
	if _, cmd := m.quit(""); !isQuit(cmd) {
		t.Fatal("quit should not wait for a detached run")
	}
	// a failing child reports without stopping the TUI
	opts.RunCommand = []string{"sh", "-c", "exit 3", "tummy"}
	m = loaded(t, opts)
	_, cmd = m.startRun(rep, nil, true)
	for _, msg := range runBatch(t, cmd) {
		if d, ok := msg.(runDoneMsg); ok && d.Err == nil {
			t.Fatal("exit 3 should report an error")
		}
	}
}
