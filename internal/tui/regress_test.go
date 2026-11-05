package tui

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/steps"
)

// Findings of the 2026-11-05 review, kept as regressions.

func TestPasteKeepsLines(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "/")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a\x00b\tc"), Paste: true})
	if got := mm.(Model).filter.Value(); got != "ab c" { // the input turns the tab into a space itself
		t.Fatalf("paste: %q", got)
	}
}

func TestZeroWidthDoesNotPanic(t *testing.T) {
	m := loaded(t, testHome(t))
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 0, Height: 0})
	m = mm.(Model)
	for _, v := range []view{viewList, viewDetail, viewConns} {
		m.view = v
		_ = m.View()
	}
}

func TestBrokenRepIsNotMarkedRunning(t *testing.T) {
	m := loaded(t, testHome(t))
	for i, r := range m.snap.Reps {
		if r.Err != "" || r.ConnErr != "" {
			m.cursor[viewList] = i
			mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
			if mm.(Model).inflight[r.Name] {
				t.Fatalf("%s marked running after a refused run", r.Name)
			}
			return
		}
	}
	t.Skip("no broken replication in the fixture")
}

func TestFilterClearedAcrossViews(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "/")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("alpha")})
	m = press(mm.(Model), "enter", "c")
	if m.filter.Value() != "" || len(m.visibleConns()) == 0 {
		t.Fatalf("filter %q carried into connections", m.filter.Value())
	}
}

func TestHiddenEntraChoiceDoesNotApply(t *testing.T) {
	opts := testHome(t)
	w, _ := newConnWizard(opts, "")
	f := w.form
	f.set("name", "X")
	f.set("type", "mssql")
	f.set("host", "h")
	f.set("fedauth", "ActiveDirectoryDefault")
	f.set("type", "postgres")
	f.set("user", "u")
	if url := w.conn().URL; strings.Contains(url, "fedauth") || !strings.HasPrefix(url, "postgres://u") {
		t.Fatalf("url %s", url)
	}
	if !f.shown("password") {
		t.Fatal("password hidden by a choice that no longer applies")
	}
}

// sqliteDB runs stmts in a sqlite file under the test home.
func sqliteDB(t *testing.T, opts Options, file string, stmts ...string) string {
	t.Helper()
	path := filepath.Join(opts.Home, file)
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func addConn(t *testing.T, opts Options, name, path string) {
	t.Helper()
	f, err := os.OpenFile(opts.Connections, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("[conn." + name + "]\ntype = \"sqlite\"\npath = \"" + path + "\"\n")
}

func TestPreviewOfAClosedExplorerIsDropped(t *testing.T) {
	opts := testHome(t)
	sqliteDB(t, opts, "src.db", "CREATE TABLE other (x INTEGER, y TEXT)", "INSERT INTO other VALUES (1, 'a')")
	e1, err := newRepStreamExplorer(opts, "", "SRC", config.ResolvedStream{Name: "things"})
	if err != nil {
		t.Fatal(err)
	}
	old := e1.start()()
	e1.update(keyEsc)
	e2, err := newRepStreamExplorer(opts, "", "SRC", config.ResolvedStream{Name: "other", Stream: config.Stream{Select: []string{"y"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e2.h.close)
	cmd := e2.start()
	e2.update(old)
	if len(e2.cols) != 0 || !e2.fetching {
		t.Fatalf("a closed explorer's preview applied: %v", e2.cols)
	}
	e2.update(cmd())
	if got := strings.Join(e2.tickedCols(), ","); got != "y" {
		t.Fatalf("ticks %q, want the stream's select", got)
	}
}

func TestTableListsReachTheirOwnScreen(t *testing.T) {
	opts := testHome(t)
	conns, _ := config.LoadConnections(opts.Connections)
	w, err := newRepWizard(opts, conns, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	w.start()
	_, lists := w.jump(stepStreams)
	_, open := w.update(keyEnter) // a stream's data while the lists load
	if w.explore == nil {
		t.Fatal("no explorer")
	}
	feed(t, w, open)
	feed(t, w, lists)
	if w.explore.tables != nil {
		t.Fatalf("explorer took the wizard's list: %v", w.explore.tables)
	}
	if w.srcLoading || w.tgtLoading || !slices.Contains(w.srcTables, "things") {
		t.Fatalf("wizard lists: loading %v/%v, source %v", w.srcLoading, w.tgtLoading, w.srcTables)
	}

	// an explorer keeps its list when another one's arrives
	a, _ := newExplorer(opts, "SRC")
	t.Cleanup(a.h.close)
	foreign := a.start()()
	b, _ := newExplorer(opts, "SRC")
	t.Cleanup(b.h.close)
	b.tables = []string{"cached"}
	b.update(foreign)
	c, _ := newExplorer(opts, "SRC")
	t.Cleanup(c.h.close)
	own := c.start()
	c.update(foreign)
	if len(b.tables) != 1 || b.tables[0] != "cached" || !c.loading {
		t.Fatalf("foreign list taken: b %v, c loading %v", b.tables, c.loading)
	}
	c.update(own())
	if c.loading || !slices.Contains(c.tables, "things") {
		t.Fatalf("own list: %v", c.tables)
	}
}

func TestSourceChangeDropsTheOldList(t *testing.T) {
	opts := testHome(t)
	addConn(t, opts, "SRC2", sqliteDB(t, opts, "src2.db", "CREATE TABLE only_in_src2 (a INTEGER)"))
	conns, _ := config.LoadConnections(opts.Connections)
	w, _ := newRepWizard(opts, conns, "")
	w.start()
	w.basics.set("name", "fresh")
	w.basics.set("source", "SRC")
	w.basics.set("target", "TGT")
	_, old := w.jump(stepDefaults)
	w.jump(stepBasics)
	w.basics.set("source", "SRC2")
	_, lists := w.jump(stepStreams)
	if !w.srcLoading {
		t.Fatal("no list asked for the new source")
	}
	feed(t, w, old)
	if w.srcTables != nil || !w.srcLoading {
		t.Fatalf("old source's list taken: %v", w.srcTables)
	}
	feed(t, w, lists)
	if len(w.srcTables) != 1 || w.srcTables[0] != "only_in_src2" {
		t.Fatalf("source list %v", w.srcTables)
	}
}

func TestConnTestResultStaysWithItsForm(t *testing.T) {
	opts := testHome(t)
	pg, _ := newConnWizard(opts, "PG")
	_, cmd := pg.update(keyTest)
	result := cmd()
	src, _ := newConnWizard(opts, "SRC")
	src.update(result)
	if src.probe != nil {
		t.Fatalf("PG's test landed in SRC's form: %+v", src.probe)
	}

	// a connection opened for a form that is gone is closed
	conns, _ := config.LoadConnections(opts.Connections)
	c, _ := conns.Get("SRC")
	d, err := db.Open(context.Background(), c.DB())
	if err != nil {
		t.Fatal(err)
	}
	m := loaded(t, opts)
	_, cmd = m.Update(connOpenMsg{Name: "SRC", DB: d, seq: nextSeq()})
	runBatch(t, cmd)
	if err := d.Ping(); err == nil {
		t.Fatal("dropped test connection left open")
	}
}

func TestEditStepKeepsFetchInFlight(t *testing.T) {
	e := openThings(t, testHome(t))
	e.pipeline = []steps.Step{{Op: "keep", Args: "id, v"}}
	inFlight := e.fetch()
	e.focus = focusSteps
	_, prefix := e.update(keyEnter)
	e.update(prefix())
	e.update(keyEsc)
	e.update(inFlight())
	if e.fetching || e.page != pagePreview {
		t.Fatalf("fetch orphaned: fetching %v page %d", e.fetching, e.page)
	}
}

func TestPreviewSaveFromTableListAddsOnce(t *testing.T) {
	opts := testHome(t)
	sqliteDB(t, opts, "src.db", "CREATE TABLE other (x INTEGER)", "INSERT INTO other VALUES (1)")
	conns, _ := config.LoadConnections(opts.Connections)
	open := func() *repWizard {
		w, err := newAddStreamWizard(opts, conns, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		feed(t, w, w.start())
		if w.pick == nil {
			t.Fatal("no table list")
		}
		return w
	}
	count := func(w *repWizard) int {
		n := 0
		for _, s := range w.draft.Streams {
			if s.Name == "other" {
				n++
			}
		}
		return n
	}
	if res := send(t, open(), keyEnter); res != wizCancel {
		t.Fatalf("enter with nothing ticked or saved: %d", res)
	}
	// typed filter: enter would take the row under the cursor
	w := open()
	for _, r := range "other" {
		send(t, w, keyRunes(string(r)))
	}
	send(t, w, tea.KeyMsg{Type: tea.KeyCtrlE}, keySave, keyEsc)
	if res := send(t, w, keyEnter); res != wizContinue || w.step != stepReview || w.reviewErr != "" || count(w) != 1 {
		t.Fatalf("filtered: res %d step %d err %q, %d streams named other", res, w.step, w.reviewErr, count(w))
	}
	// cursor moved, nothing ticked: the saved stream still goes to review
	w = open()
	for i := 0; i < 10; i++ {
		if c, _ := w.pick.current(); c == "other" {
			break
		}
		send(t, w, keyDown)
	}
	send(t, w, tea.KeyMsg{Type: tea.KeyCtrlE}, keySave, keyEsc)
	if res := send(t, w, keyEnter); res != wizContinue || w.step != stepReview || count(w) != 1 {
		t.Fatalf("unfiltered: res %d step %d, %d streams named other", res, w.step, count(w))
	}
}

func TestSecretKeepsSpaces(t *testing.T) {
	w, _ := newConnWizard(testHome(t), "")
	f := w.form
	f.set("password", " pw ")
	if got := f.get("password"); got != " pw " {
		t.Fatalf("password %q", got)
	}
	f.set("password", "")
	f.focused = f.field("password")
	f.focus()
	f.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s3cret\r\n"), Paste: true})
	if got := f.get("password"); got != "s3cret" {
		t.Fatalf("pasted password %q", got)
	}
}
