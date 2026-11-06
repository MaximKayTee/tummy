package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tummy/internal/config"
	"tummy/internal/steps"
)

func parseOne(t *testing.T, s string) steps.Step {
	t.Helper()
	p, err := steps.Parse(s)
	if err != nil || len(p) != 1 {
		t.Fatalf("step %q: %v", s, err)
	}
	return p[0]
}

func readMarks(t *testing.T, opts Options) *config.Bookmarks {
	t.Helper()
	b, err := config.LoadBookmarks(config.BookmarksPath(opts.Home))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBookmarkSaveOpenDelete(t *testing.T) {
	opts := testHome(t)
	e := openThings(t, opts)
	e.pipeline = append(e.pipeline, parseOne(t, "filter id > 1"))
	press1(e, keyRunes("R"))
	if len(e.rows) != 2 || !e.dirty() {
		t.Fatalf("rows=%d dirty=%v", len(e.rows), e.dirty())
	}
	e.update(keyRight)
	e.update(keySpace) // untick v

	// ctrl+s off a stream asks for a name
	e.update(keySave)
	if e.page != pageBookmark || e.markForm.get("name") != "things" {
		t.Fatalf("page=%d name=%q", e.page, e.markForm.get("name"))
	}
	e.update(keySave)
	if e.page != pagePreview || e.bookmark != "things" || e.dirty() {
		t.Fatalf("saved: page=%d bookmark=%q err=%q dirty=%v", e.page, e.bookmark, e.markForm.err, e.dirty())
	}
	b := readMarks(t, opts).Bookmark["things"]
	if b == nil || b.Conn != "SRC" || b.Table != "things" || b.Steps != "filter id > 1 | keep id" || b.Rep != "" {
		t.Fatalf("bookmark: %+v", b)
	}
	if v := strings.Join(e.view(100, 20), "\n"); !strings.Contains(v, "bookmark things") || !strings.Contains(v, "saved bookmarks.toml › things") {
		t.Fatalf("head:\n%s", v)
	}

	// a second ctrl+s saves in place; the name is taken for a new bookmark
	e.pipeline = append(e.pipeline, parseOne(t, "sort -id"))
	e.update(keySave)
	if e.page != pagePreview || readMarks(t, opts).Bookmark["things"].Steps != "filter id > 1 | sort -id | keep id" {
		t.Fatalf("resave: %+v", readMarks(t, opts).Bookmark["things"])
	}

	// the table list shows it first; enter opens it with its steps
	e2, err := newExplorer(opts, "SRC")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2.h.close() })
	run(e2, e2.start())
	items := e2.visibleItems()
	if len(items) != 2 || items[0].mark == nil || items[0].name != "things" || items[1].mark != nil {
		t.Fatalf("items: %+v", items)
	}
	if v := strings.Join(e2.view(100, 20), "\n"); !strings.Contains(v, "things  |  filter id > 1 | sort -id | keep id") {
		t.Fatalf("list:\n%s", v)
	}
	press1(e2, keyEnter)
	if e2.page != pagePreview || e2.bookmark != "things" || len(e2.pipeline) != 3 || len(e2.rows) != 2 || len(e2.cols) != 1 || e2.dirty() {
		t.Fatalf("open: page=%d mark=%q steps=%d rows=%d cols=%d err=%s", e2.page, e2.bookmark, len(e2.pipeline), len(e2.rows), len(e2.cols), e2.fetchErr)
	}
	e2.pipeline = e2.pipeline[:1]
	if !e2.dirty() {
		t.Fatal("changed steps are dirty")
	}
	// esc back to the list: the bookmark is left behind; d deletes it
	e2.update(keyEsc)
	e2.update(keyRunes("y"))
	if e2.page != pageTables || e2.bookmark != "" {
		t.Fatalf("back: page=%d mark=%q", e2.page, e2.bookmark)
	}
	e2.update(keyRunes("d"))
	if e2.ask == nil {
		t.Fatal("d asks")
	}
	e2.update(keyRunes("y"))
	if len(e2.marks) != 0 || len(readMarks(t, opts).Bookmark) != 0 || len(e2.visibleItems()) != 1 {
		t.Fatalf("delete: %d %d", len(e2.marks), len(readMarks(t, opts).Bookmark))
	}
	// d on a table does nothing
	e2.update(keyRunes("d"))
	if e2.ask != nil {
		t.Fatal("d on a table")
	}
}

func TestBookmarkQueryAndNameCheck(t *testing.T) {
	opts := testHome(t)
	os.WriteFile(config.BookmarksPath(opts.Home), []byte("# kept\n[bookmark.taken]\nconn = \"SRC\"\ntable = \"things\"\n"), 0o600)
	e, err := newExplorer(opts, "SRC")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.h.close() })
	run(e, e.start())
	e.update(keyRunes("S"))
	e.sqlInput.SetValue("select id from things where id < 3")
	press1(e, keySave)
	if len(e.rows) != 2 {
		t.Fatalf("sql preview: %d %s", len(e.rows), e.fetchErr)
	}
	e.update(keySave)
	if e.page != pageBookmark || e.markForm.get("name") != "query" {
		t.Fatalf("page=%d name=%q", e.page, e.markForm.get("name"))
	}
	e.markForm.set("name", "taken")
	e.update(keySave)
	if e.page != pageBookmark || e.markForm.field("name").err != "exists" {
		t.Fatalf("taken: page=%d err=%q", e.page, e.markForm.field("name").err)
	}
	e.markForm.set("name", "bad name")
	e.update(keySave)
	if e.page != pageBookmark {
		t.Fatal("bad name accepted")
	}
	e.markForm.set("name", "small")
	e.update(keySave)
	if e.page != pagePreview || e.bookmark != "small" {
		t.Fatalf("save: page=%d err=%q", e.page, e.markForm.err)
	}
	raw, _ := os.ReadFile(config.BookmarksPath(opts.Home))
	if !strings.HasPrefix(string(raw), "# kept\n") || !strings.Contains(string(raw), "[bookmark.small]\nconn = \"SRC\"\nsql = \"select id from things where id < 3\"\n") {
		t.Fatalf("file:\n%s", raw)
	}
	if v := strings.Join(e.view(100, 20), "\n"); !strings.Contains(v, "bookmark small") {
		t.Fatalf("head:\n%s", v)
	}
}

func TestTargetBookmarkNamesStream(t *testing.T) {
	opts := testHome(t)
	m := loaded(t, opts)
	rep, _ := m.rep("alpha")
	for range runBatch(t, m.runNow(rep, nil, false)) {
	}
	m = loaded(t, opts)
	m = press(m, "enter", "enter")
	e := m.wiz.(*explorer)
	run(e, e.start())
	e.pipeline = append(e.pipeline, parseOne(t, "filter id = 1"))
	press1(e, keyRunes("R"))
	if len(e.rows) != 1 {
		t.Fatalf("rows=%d %s", len(e.rows), e.fetchErr)
	}
	e.update(keySave)
	e.update(keySave)
	b := readMarks(t, opts).Bookmark["things"]
	if b == nil || b.Conn != "TGT" || b.Rep != "alpha" || b.Stream != "things" || b.Steps != "filter id = 1" {
		t.Fatalf("bookmark: %+v", b)
	}
	if _, err := config.LoadConnections(opts.Connections); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(opts.Home, "replications", "alpha.toml")); err != nil || fi.Size() == 0 {
		t.Fatal("replication file untouched")
	}
}

func TestCopySQL(t *testing.T) {
	var buf bytes.Buffer
	old := clipboardOut
	clipboardOut = &buf
	t.Cleanup(func() { clipboardOut = old })
	e := openThings(t, testHome(t))
	_, cmd := e.update(keyRunes("y"))
	if cmd == nil {
		t.Fatal("no copy")
	}
	cmd()
	if got := buf.String(); !strings.HasPrefix(got, "\x1b]52;c;") || !strings.HasSuffix(got, "\a") {
		t.Fatalf("osc52: %q", got)
	}
	if e.currentNote() != "sql copied" {
		t.Fatalf("note: %q", e.currentNote())
	}
	// from the steps panel too
	e.update(keyRunes("\t"))
	e.focus = focusSteps
	buf.Reset()
	if _, cmd := e.update(keyRunes("y")); cmd == nil {
		t.Fatal("no copy from steps")
	}
}

func TestBenchRunsFull(t *testing.T) {
	e := openThings(t, testHome(t))
	e.pipeline = append(e.pipeline, parseOne(t, "filter id > 1"))
	press1(e, keyRunes("R"))
	_, cmd := e.update(keyRunes("b"))
	if cmd == nil || !e.benching {
		t.Fatal("b runs")
	}
	if v := strings.Join(e.view(100, 20), "\n"); !strings.Contains(v, "◐ full run") {
		t.Fatalf("head:\n%s", v)
	}
	if _, again := e.update(keyRunes("b")); again != nil {
		t.Fatal("one at a time")
	}
	msg := cmd().(benchMsg)
	e.update(msg)
	if e.benching || !strings.HasPrefix(e.benchText, "full 2 rows  ") {
		t.Fatalf("bench: %q", e.benchText)
	}
	if v := strings.Join(e.view(100, 20), "\n"); !strings.Contains(v, "full 2 rows") {
		t.Fatalf("head:\n%s", v)
	}
	// a new query drops the old timing
	press1(e, keyRunes("R"))
	if e.benchText != "" {
		t.Fatal("stale bench kept")
	}
	// an error lands as a note
	e.update(benchMsg{seq: e.benchSeq, err: os.ErrClosed})
	if e.currentNote() == "" {
		t.Fatal("error note")
	}
}
