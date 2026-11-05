package tui

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	_ "modernc.org/sqlite"

	"tummy/internal/config"
	"tummy/internal/state"
)

func testHome(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	src, _ := sql.Open("sqlite", filepath.Join(dir, "src.db"))
	src.Exec("CREATE TABLE things (id INTEGER PRIMARY KEY, v TEXT)")
	src.Exec("INSERT INTO things VALUES (1,'a'),(2,'b'),(3,'c')")
	src.Close()
	os.MkdirAll(config.ReplicationsDir(dir), 0o755)
	os.WriteFile(filepath.Join(dir, "connections.toml"), []byte(
		"[conn.SRC]\ntype = \"sqlite\"\npath = \""+filepath.Join(dir, "src.db")+"\"\n"+
			"[conn.TGT]\ntype = \"sqlite\"\npath = \""+filepath.Join(dir, "tgt.db")+"\"\n"+
			"[conn.PG]\ntype = \"postgres\"\nurl = \"postgres://u:secret@127.0.0.1:1/db?connect_timeout=1\"\n"), 0o644)
	os.WriteFile(filepath.Join(config.ReplicationsDir(dir), "alpha.toml"), []byte(
		"source = \"SRC\"\ntarget = \"TGT\"\nschedule = \"*/5 * * * *\"\n[streams.things]\n[streams.copy]\nsql = \"select * from things\"\nobject = \"things_copy\"\n"), 0o644)
	os.WriteFile(filepath.Join(config.ReplicationsDir(dir), "beta.toml"), []byte(
		"source = \"PG\"\ntarget = \"TGT\"\nenabled = false\n[streams.\"public.x\"]\n"), 0o644)
	os.WriteFile(filepath.Join(config.ReplicationsDir(dir), "gamma.toml"), []byte("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"nope\"\n[streams.x]\n"), 0o644)
	st, err := state.Open(filepath.Join(dir, "state", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return Options{Home: dir, Connections: filepath.Join(dir, "connections.toml"), State: st, Refresh: 100 * time.Millisecond}
}

// loaded returns a model with a snapshot applied and a terminal size set.
func loaded(t *testing.T, opts Options) Model {
	t.Helper()
	m := New(opts)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = mm.(Model)
	msg := m.loadSnapshot()()
	mm, _ = m.Update(msg)
	return mm.(Model)
}

func press(m Model, keys ...string) Model {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		mm, _ := m.Update(msg)
		m = mm.(Model)
	}
	return m
}

func TestListRendersEveryFile(t *testing.T) {
	m := loaded(t, testHome(t))
	v := m.View()
	for _, want := range []string{"REPLICATION", "alpha", "beta (off)", "gamma", "SRC", "TGT", "*/5 * * * *", "no daemon", "✗ stream x: unknow"} {
		if !strings.Contains(v, want) {
			t.Errorf("list view missing %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "› alpha") {
		t.Errorf("first row should be selected:\n%s", v)
	}
	if strings.Contains(v, "secret") {
		t.Error("password leaked")
	}
}

func TestNavigationAndViews(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "j", "j", "k", "enter") // beta
	if m.view != viewDetail || m.repName != "beta" {
		t.Fatalf("detail: view=%d rep=%s", m.view, m.repName)
	}
	v := m.View()
	if !strings.Contains(v, "public.x") || !strings.Contains(v, "disabled") || !strings.Contains(v, "›  beta") {
		t.Errorf("detail view:\n%s", v)
	}
	m = press(m, "esc")
	if m.view != viewList || m.cursor[viewList] != 1 {
		t.Fatalf("back to list keeps cursor: view=%d cursor=%d", m.view, m.cursor[viewList])
	}
	// broken file shows its error box
	m = press(m, "j", "enter")
	if v := m.View(); !strings.Contains(v, "unknown mode") || strings.Contains(v, "STREAM ") {
		t.Errorf("broken detail:\n%s", v)
	}
	m = press(m, "q")
	if m.view != viewList {
		t.Fatal("q should go back, not quit, from a sub view")
	}
	m = press(m, "c")
	if v := m.View(); m.view != viewConns || !strings.Contains(v, "PG") || !strings.Contains(v, "u:***@") {
		t.Errorf("connections:\n%s", v)
	}
	m = press(m, "esc", "H")
	if v := m.View(); m.view != viewHistory || m.repName != "" {
		t.Errorf("global history: %d %q\n%s", m.view, m.repName, v)
	}
	m = press(m, "esc", "?")
	if v := m.View(); m.help == nil || !strings.Contains(v, "replications") || !strings.Contains(v, "schedule") || !strings.Contains(v, "esc close") {
		t.Errorf("help:\n%s", v)
	}
	m = press(m, "esc")
	if m.help != nil {
		t.Fatal("esc closes help")
	}
	m = press(m, "G")
	if m.cursor[viewList] != 2 {
		t.Fatalf("G: %d", m.cursor[viewList])
	}
	m = press(m, "g")
	if m.cursor[viewList] != 0 {
		t.Fatalf("g: %d", m.cursor[viewList])
	}
}

func TestFilter(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "/", "bet")
	if !m.filtering || !strings.Contains(m.View(), "/bet") {
		t.Fatalf("typing filter:\n%s", m.View())
	}
	if reps := m.visibleReps(); len(reps) != 1 || reps[0].Name != "beta" {
		t.Fatalf("filtered: %+v", reps)
	}
	m = press(m, "enter")
	if m.filtering || len(m.visibleReps()) != 1 {
		t.Fatal("filter should stay applied after enter")
	}
	m = press(m, "esc")
	if len(m.visibleReps()) != 3 {
		t.Fatal("esc clears the filter")
	}
}

func TestToggleEnabledEditsFile(t *testing.T) {
	opts := testHome(t)
	m := loaded(t, opts)
	m = press(m, "j") // beta, disabled
	rep, _ := m.rep("beta")
	msg := m.toggleEnabled(rep)()
	if s, ok := msg.(statusMsg); !ok || s.IsErr || s.Text != "beta: enabled" {
		t.Fatalf("toggle: %+v", msg)
	}
	r, err := config.LoadReplication(rep.Path)
	if err != nil || !r.IsEnabled() {
		t.Fatalf("file not updated: %v", err)
	}
	m = loaded(t, opts)
	if strings.Contains(m.View(), "beta (off)") {
		t.Fatal("list should show beta enabled after refresh")
	}
}

func TestRunInProcessAndHistory(t *testing.T) {
	opts := testHome(t)
	m := loaded(t, opts)
	rep, _ := m.rep("alpha")
	cmd := m.runNow(rep, nil)
	// Batch: status then the run itself. Execute both.
	var done runDoneMsg
	var sawStatus bool
	for _, msg := range runBatch(t, cmd) {
		switch x := msg.(type) {
		case statusMsg:
			sawStatus = true
			if !strings.HasPrefix(x.Text, "running: ") {
				t.Fatalf("status: %+v", x)
			}
		case runDoneMsg:
			done = x
		}
	}
	if !sawStatus || done.Replication != "alpha" || done.Err != nil {
		t.Fatalf("run: %+v", done)
	}
	m = loaded(t, opts)
	v := m.View()
	if !strings.Contains(v, "✓ ok") || !strings.Contains(v, "6 rows") {
		t.Errorf("list after run:\n%s", v)
	}
	m = press(m, "enter")
	v = m.View()
	if !strings.Contains(v, "things_copy") || !strings.Contains(v, "full-refresh") {
		t.Errorf("detail after run:\n%s", v)
	}
	// stream history and run detail
	m = press(m, "H")
	mm, _ := m.Update(m.loadRuns("alpha", "things")())
	m = mm.(Model)
	v = m.View()
	if m.view != viewHistory || !strings.Contains(v, "manual") || !strings.Contains(v, "✓ ok") {
		t.Errorf("history:\n%s", v)
	}
	m = press(m, "enter")
	v = m.View()
	if m.view != viewRun || !strings.Contains(v, "run #") || !strings.Contains(v, "status        ok") {
		t.Errorf("run detail:\n%s", v)
	}

	// a failing replication records an error row that shows everywhere
	rep, _ = m.rep("beta")
	config.SetEnabled(rep.Path, true)
	m = loaded(t, opts)
	rep, _ = m.rep("beta")
	for _, msg := range runBatch(t, m.runNow(rep, nil)) {
		if d, ok := msg.(runDoneMsg); ok && d.Err == nil {
			t.Fatal("expected failure")
		}
	}
	m = loaded(t, opts)
	if v := m.View(); !strings.Contains(v, "✗ failed") {
		t.Errorf("list after failure:\n%s", v)
	}
	m = press(m, "j", "enter")
	if v := m.View(); !strings.Contains(v, "(replication)") || !strings.Contains(v, "✗") {
		t.Errorf("detail after failure:\n%s", v)
	}
	m = press(m, "H")
	mm, _ = m.Update(m.loadRuns("beta", "*")())
	m = mm.(Model)
	m = press(m, "enter")
	if v := m.View(); !strings.Contains(v, "connection refused") && !strings.Contains(v, "failed to connect") {
		t.Errorf("error detail:\n%s", v)
	}
}

func TestRunQueuedWhenDaemonAlive(t *testing.T) {
	opts := testHome(t)
	opts.State.Heartbeat(context.Background(), 1, time.Now(), opts.Home)
	m := loaded(t, opts)
	if !strings.Contains(m.View(), "● daemon") {
		t.Fatalf("badge:\n%s", m.View())
	}
	rep, _ := m.rep("alpha")
	msg := m.runNow(rep, []string{"things"})()
	if s, ok := msg.(statusMsg); !ok || !strings.Contains(s.Text, "queued: alpha (things)") {
		t.Fatalf("%+v", msg)
	}
	p, _ := opts.State.Pending(context.Background())
	if p["alpha"] != 1 {
		t.Fatal("request not enqueued")
	}
	m = loaded(t, opts)
	if !strings.Contains(m.View(), "queued") {
		t.Errorf("list should show queued:\n%s", m.View())
	}
}

func TestConnTest(t *testing.T) {
	opts := testHome(t)
	m := loaded(t, opts)
	ok := m.testConn("SRC")().(connTestMsg)
	bad := m.testConn("PG")().(connTestMsg)
	if ok.Err != nil || bad.Err == nil {
		t.Fatalf("ok=%v bad=%v", ok.Err, bad.Err)
	}
	mm, _ := m.Update(ok)
	mm, _ = mm.(Model).Update(bad)
	m = press(mm.(Model), "c")
	v := m.View()
	if !strings.Contains(v, "✓ ok") || !strings.Contains(v, "✗") {
		t.Errorf("conns view:\n%s", v)
	}
}

func TestNarrowAndSmall(t *testing.T) {
	m := loaded(t, testHome(t))
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 10})
	m = mm.(Model)
	v := m.View()
	for _, line := range strings.Split(v, "\n") {
		if w := widthOf(line); w > 60 {
			t.Errorf("line wider than terminal (%d): %q", w, line)
		}
	}
	if n := strings.Count(v, "\n") + 1; n != 10 {
		t.Errorf("want 10 lines, got %d", n)
	}
}

func TestTeatestFlow(t *testing.T) {
	opts := testHome(t)
	tm := teatest.NewTestModel(t, New(opts), teatest.WithInitialTermSize(100, 24))
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte("alpha")) }, teatest.WithDuration(5*time.Second))
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte("things_copy")) }, teatest.WithDuration(5*time.Second))
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
}

// runBatch executes a tea.Cmd, expanding batches, and returns the messages.
func runBatch(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	switch x := msg.(type) {
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range x {
			out = append(out, runBatch(t, c)...)
		}
		return out
	case nil:
		return nil
	}
	return []tea.Msg{msg}
}

var _ = errors.New

func TestBadConnListedWithError(t *testing.T) {
	opts := testHome(t)
	f, _ := os.OpenFile(opts.Connections, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("\n[conn.BAD]\ntype = \"mssql\"\nurl = \"myhost\"\n")
	f.Close()
	m := loaded(t, opts)
	var bad, good int
	for _, c := range m.snap.Conns {
		switch {
		case c.Name == "BAD" && strings.Contains(c.Err, "sqlserver://"):
			bad++
		case c.Name == "(error)":
			t.Fatalf("whole file reported: %s", c.Target)
		case c.Err == "":
			good++
		}
	}
	if bad != 1 || good == 0 {
		t.Fatalf("rows %+v", m.snap.Conns)
	}
	m.view = viewConns
	if v := strings.Join(m.connsView(), "\n"); !strings.Contains(v, "BAD") || !strings.Contains(v, "sqlserver://") {
		t.Fatalf("view:\n%s", v)
	}
}

func TestDryRunPage(t *testing.T) {
	m := loaded(t, testHome(t))
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	m = mm.(Model)
	if m.view != viewRun || !m.planning || !strings.Contains(m.View(), "resolving") {
		t.Fatalf("t should open the page at once, resolving shown:\n%s", m.View())
	}
	var plan planMsg
	found := false
	for _, msg := range runBatch(t, cmd) {
		if p, ok := msg.(planMsg); ok {
			plan, found = p, true
		}
	}
	if !found {
		t.Fatal("t on the list should plan the replication")
	}
	mm, _ = m.Update(plan)
	m = mm.(Model)
	v := m.View()
	if m.view != viewRun || m.planName == "" || !strings.Contains(v, "dry run") || !strings.Contains(v, "create") || !strings.Contains(v, "things") {
		t.Fatalf("plan page:\n%s", v)
	}
	if runs, _ := m.opts.State.RunsFor(context.Background(), "", "", 10); len(runs) != 0 {
		t.Fatal("dry run wrote a run")
	}
	if strings.Contains(v, "rolled back") {
		t.Fatalf("plan page sampled before t:\n%s", v)
	}
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	m = mm.(Model)
	found = false
	for _, msg := range runBatch(t, cmd) {
		if p, ok := msg.(planMsg); ok {
			plan, found = p, true
		}
	}
	if !found || !m.planSample || len(plan.Rows) == 0 || plan.Rows[0].Sample == nil {
		t.Fatalf("t on the plan page should test load: found %v rows %+v", found, plan.Rows)
	}
	mm, _ = m.Update(plan)
	m = mm.(Model)
	if v = m.View(); !strings.Contains(v, "rolled back") || m.planning {
		t.Fatalf("test load page:\n%s", v)
	}
	if runs, _ := m.opts.State.RunsFor(context.Background(), "", "", 10); len(runs) != 0 {
		t.Fatal("test load wrote a run")
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = mm.(Model)
	if m.view != viewList || m.planName != "" {
		t.Fatalf("esc: view %d plan %q", m.view, m.planName)
	}
	// r on the plan page runs it and shows the detail view, where streams report progress
	mm, _ = m.Update(plan)
	m = mm.(Model)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	m = mm.(Model)
	mm, _ = m.Update(plan)
	m = mm.(Model)
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = mm.(Model)
	if m.view != viewDetail || m.repName != "alpha" || cmd == nil {
		t.Fatalf("r from the plan: view %d rep %q", m.view, m.repName)
	}
}

func TestControlRunesDropped(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "/")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("al\x00pha")})
	m = mm.(Model)
	if got := m.filter.Value(); got != "alpha" {
		t.Fatalf("filter %q", got)
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0}})
	if mm.(Model).filter.Value() != "alpha" {
		t.Fatal("a lone NUL should do nothing")
	}
}

func TestDryRunFromDetailAndBrokenRep(t *testing.T) {
	opts := testHome(t)
	m := loaded(t, opts)
	m = press(m, "enter") // alpha's detail view
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	m = mm.(Model)
	if m.view != viewRun || !m.planning || cmd == nil {
		t.Fatalf("t on the detail view: view %d planning %v", m.view, m.planning)
	}
	// a replication with a config error still gets the page, with the error on it
	os.WriteFile(filepath.Join(config.ReplicationsDir(opts.Home), "broken.toml"), []byte("source = \"SRC\"\ntarget = \"NOPE\"\n[streams.things]\n"), 0o644)
	m = loaded(t, opts)
	for i, r := range m.snap.Reps {
		if r.Name == "broken" {
			m.cursor[viewList] = i
		}
	}
	mm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	m = mm.(Model)
	for _, msg := range runBatch(t, cmd) {
		if p, ok := msg.(planMsg); ok {
			mm, _ = m.Update(p)
			m = mm.(Model)
		}
	}
	if m.view != viewRun || m.planning || !strings.Contains(m.View(), "NOPE") {
		t.Fatalf("broken replication: view %d planning %v\n%s", m.view, m.planning, m.View())
	}
}
