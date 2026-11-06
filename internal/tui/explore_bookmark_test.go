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

	// B asks for a name
	e.update(keyRunes("B"))
	if e.page != pageBookmark || e.markForm.get("name") != "things" {
		t.Fatalf("page=%d name=%q", e.page, e.markForm.get("name"))
	}
	e.update(keyEnter)
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

	// a second B saves in place; the name is taken for a new bookmark
	e.pipeline = append(e.pipeline, parseOne(t, "sort -id"))
	e.update(keyRunes("B"))
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
	e.update(keyRunes("B"))
	if e.page != pageBookmark || e.markForm.get("name") != "query" {
		t.Fatalf("page=%d name=%q", e.page, e.markForm.get("name"))
	}
	e.markForm.set("name", "taken")
	e.update(keyEnter)
	if e.page != pageBookmark || e.markForm.field("name").err != "exists" {
		t.Fatalf("taken: page=%d err=%q", e.page, e.markForm.field("name").err)
	}
	e.markForm.set("name", "bad name")
	e.update(keyEnter)
	if e.page != pageBookmark {
		t.Fatal("bad name accepted")
	}
	e.markForm.set("name", "small")
	e.update(keyEnter)
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
	e.update(keyRunes("B"))
	e.update(keyEnter)
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
	if cmd == nil || e.busy != "full run" {
		t.Fatal("b runs")
	}
	if v := strings.Join(e.view(100, 20), "\n"); !strings.Contains(v, "◐ full run") {
		t.Fatalf("head:\n%s", v)
	}
	if _, again := e.update(keyRunes("b")); again != nil {
		t.Fatal("one at a time")
	}
	msg := cmd().(execMsg)
	e.update(msg)
	if e.busy != "" || !strings.HasPrefix(e.benchText, "full 2 rows  ") {
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
	e.update(execMsg{seq: e.busySeq, kind: "bench", err: os.ErrClosed})
	if e.currentNote() == "" {
		t.Fatal("error note")
	}
}

func TestSourcePreviewNeverSavesTheStream(t *testing.T) {
	opts := testHome(t)
	m := loaded(t, opts)
	m = press(m, "enter", "p") // alpha › things, source preview
	e, ok := m.wiz.(*explorer)
	if !ok || e.streamName != "things" || e.canSaveBack() {
		t.Fatalf("p: %+v", m.wiz)
	}
	run(e, e.start())
	before, _ := os.ReadFile(filepath.Join(opts.Home, "replications", "alpha.toml"))
	e.pipeline = append(e.pipeline, parseOne(t, "filter id > 1"))
	press1(e, keyRunes("R"))
	if _, cmd := e.update(keySave); cmd != nil || e.page != pagePreview || !strings.HasPrefix(e.currentNote(), "B: bookmark") {
		t.Fatalf("ctrl+s on a source preview only points at B: %q", e.currentNote())
	}
	after, _ := os.ReadFile(filepath.Join(opts.Home, "replications", "alpha.toml"))
	if string(before) != string(after) {
		t.Fatalf("alpha.toml changed:\n%s", after)
	}
	if f, _, _ := e.previewKeys(); strings.Contains(strings.Join(f, " "), "ctrl+s") || !strings.Contains(strings.Join(f, " "), "B") {
		t.Fatalf("footer: %v", f)
	}
	// B bookmarks it under the stream
	e.update(keyRunes("B"))
	e.update(keyEnter)
	b := readMarks(t, opts).Bookmark["things"]
	if b == nil || b.Conn != "SRC" || b.Rep != "alpha" || b.Stream != "things" || b.Steps != "filter id > 1" {
		t.Fatalf("bookmark: %+v", b)
	}
	// a stream with where cannot be bookmarked from the source
	e.spec.Where = "id > 0"
	e.bookmark = ""
	if e.update(keyRunes("B")); e.page != pagePreview || !strings.Contains(e.currentNote(), "where") {
		t.Fatalf("where: page=%d note=%q", e.page, e.currentNote())
	}
}

func TestDetailListsBookmarksUnderTheStream(t *testing.T) {
	opts := testHome(t)
	os.WriteFile(config.BookmarksPath(opts.Home), []byte(
		"[bookmark.big]\nconn = \"TGT\"\ntable = \"things\"\nsteps = \"filter id > 1\"\nrep = \"alpha\"\nstream = \"things\"\n"+
			"[bookmark.lost]\nconn = \"SRC\"\ntable = \"things\"\nrep = \"alpha\"\nstream = \"gone\"\n"), 0o600)
	m := loaded(t, opts)
	m = press(m, "enter")
	rep, _ := m.rep("alpha")
	rows := rep.detailRows()
	if len(rows) != 4 || rows[0].stream != "things" || rows[1].mark == nil || rows[1].mark.Name != "big" || rows[2].stream != "copy" || rows[3].mark == nil || rows[3].mark.Name != "lost" {
		t.Fatalf("rows: %+v", rows)
	}
	v := m.View()
	if !strings.Contains(v, "› big") || !strings.Contains(v, "bookmark") || !strings.Contains(v, "› lost") {
		t.Fatalf("view:\n%s", v)
	}
	m.cursor[viewDetail] = 1
	if w := m.detailEnterWord(); w != "bookmark" {
		t.Fatalf("enter word: %q", w)
	}
	// the target table does not exist yet: the explorer opens on TGT and reports the error
	mm, cmd := m.Update(keyEnter)
	m = mm.(Model)
	e, ok := m.wiz.(*explorer)
	if !ok || e.conn.Name != "TGT" || e.bookmark != "big" || len(e.pipeline) != 1 || e.page != pagePreview {
		t.Fatalf("enter on a bookmark: %+v", m.wiz)
	}
	_ = cmd
	// d deletes it
	m = loaded(t, opts)
	m = press(m, "enter")
	m.cursor[viewDetail] = 1
	m = press(m, "d")
	if m.ask == nil || m.ask.text != "delete bookmark big? y/n" {
		t.Fatalf("ask: %+v", m.ask)
	}
	_, cmd = m.Update(keyRunes("y"))
	if d, ok := cmd().(deletedMsg); !ok || d.IsErr {
		t.Fatalf("delete: %+v", cmd())
	}
	if all := readMarks(t, opts); all.Bookmark["big"] != nil || all.Bookmark["lost"] == nil {
		t.Fatalf("file: %v", all.Names())
	}
}
