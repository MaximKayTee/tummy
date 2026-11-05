package tui

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/steps"
)

func typeInto(e *explorer, s string) {
	for _, r := range s {
		e.update(keyRunes(string(r)))
	}
}

// run feeds the result of cmd back, as the program loop would.
func run(e *explorer, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		switch msg.(type) {
		case previewMsg, prefixMsg, discoverMsg:
			e.update(msg)
		}
	}
}

func press1(e *explorer, msg tea.KeyMsg) {
	_, cmd := e.update(msg)
	run(e, cmd)
}

// openThings opens the explorer on SRC and previews things.
func openThings(t *testing.T, opts Options) *explorer {
	t.Helper()
	e, err := newExplorer(opts, "SRC")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.h.close() })
	run(e, e.start())
	i := -1
	for n, tb := range e.tables {
		if tb == "things" {
			i = n
		}
	}
	e.cur = i
	press1(e, keyEnter)
	if e.page != pagePreview || len(e.cols) == 0 {
		t.Fatalf("preview: page=%d err=%s", e.page, e.fetchErr)
	}
	return e
}

func pg(t *testing.T) db.Dialect {
	d, err := db.DialectFor("postgres")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ctxOf(dl db.Dialect, cols ...db.Column) stepContext { return stepContext{cols: cols, dl: dl} }

func strs(names ...string) []db.Column {
	out := make([]db.Column, len(names))
	for i, n := range names {
		out[i] = db.Column{Name: n}
	}
	return out
}

func readRep(t *testing.T, opts Options, name string) *config.Replication {
	t.Helper()
	r, err := config.LoadReplication(filepath.Join(config.ReplicationsDir(opts.Home), name+".toml"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func streamOf(r *config.Replication, name string) config.Stream {
	for _, s := range r.Resolved {
		if s.Name == name {
			return s.Stream
		}
	}
	return config.Stream{}
}

func TestExplorerDB(t *testing.T) {
	opts := testHome(t)
	e := openThings(t, opts)
	if len(e.tables) != 1 || e.tables[0] != "things" {
		t.Fatalf("tables: %v", e.tables)
	}
	if len(e.cols) != 2 || len(e.rows) != 3 {
		t.Fatalf("preview: cols=%d rows=%d err=%s", len(e.cols), len(e.rows), e.fetchErr)
	}
	v := strings.Join(e.view(80, 20), "\n")
	if !strings.Contains(v, "● id") || !strings.Contains(v, "● v") || !strings.Contains(v, "3 rows") {
		t.Fatalf("grid:\n%s", v)
	}
	e.update(keyRight)
	e.update(keySpace) // untick v
	if ticked := e.tickedCols(); len(ticked) != 1 || ticked[0] != "id" {
		t.Fatalf("ticked: %v", ticked)
	}
	// custom sql, several lines
	e.update(keyRunes("S"))
	if !e.editSQL || !strings.Contains(e.sqlInput.Value(), "things") {
		t.Fatal("sql editor")
	}
	e.sqlInput.SetValue("select count(*) as n -- rows\nfrom things")
	press1(e, keySave)
	if len(e.cols) != 1 || e.cols[0].Name != "n" || e.spec.SQL == "" {
		t.Fatalf("sql preview: %+v %s", e.cols, e.fetchErr)
	}
	// add to a new replication
	e.update(keyRunes("+"))
	if e.page != pageTarget {
		t.Fatal("target page")
	}
	e.repPick.set("name", "counts")
	e.repPick.set("target", "TGT")
	e.update(keySave)
	if e.page != pageStream || e.stream.get("name") != "query" {
		t.Fatalf("stream page: %d %q", e.page, e.stream.get("name"))
	}
	e.stream.set("name", "thing_count")
	if res, _ := e.update(keySave); res != wizContinue || e.page != pagePreview || e.streamName != "thing_count" {
		t.Fatalf("save returns to the preview: page=%d %q", e.page, e.stream.err)
	}
	r := readRep(t, opts, "counts")
	if r.Resolved[0].Name != "thing_count" || !strings.Contains(r.Resolved[0].SQL, "-- rows\nfrom things") {
		t.Fatalf("written: %+v", r.Resolved)
	}

	// a table with a column subset into the existing replication
	e2 := openThings(t, opts)
	e2.update(keyRight)
	e2.update(keySpace)
	e2.update(keyRunes("+"))
	e2.repPick.set("rep", "counts")
	e2.update(keySave)
	e2.stream.set("object", "things_renamed")
	if e2.update(keySave); e2.page != pagePreview {
		t.Fatalf("save 2: %q", e2.stream.err)
	}
	r = readRep(t, opts, "counts")
	if len(r.Resolved) != 2 || r.Resolved[1].Name != "things" || r.Resolved[1].Object != "things_renamed" || strings.Join(r.Resolved[1].Select, ",") != "id" {
		t.Fatalf("appended: %+v", r.Resolved)
	}
	// the same name again is refused instead of replacing it
	e3 := openThings(t, opts)
	e3.update(keyRunes("+"))
	e3.repPick.set("rep", "counts")
	e3.update(keySave)
	if e3.editing != "things" || e3.stream.get("object") != "things_renamed" {
		t.Fatalf("existing stream prefills: %q %q", e3.editing, e3.stream.get("object"))
	}
	e3.stream.set("name", "thing_count")
	if e3.update(keySave); e3.page != pageStream || !strings.Contains(e3.stream.err, "exists") {
		t.Fatal("duplicate name refused")
	}
}

func TestExplorerAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": 1, "name": "a", "meta": map[string]any{"k": true}}, {"id": 2, "name": "b", "meta": map[string]any{"k": false}}}})
	}))
	defer srv.Close()
	opts := testHome(t)
	raw, _ := config.LoadConnectionsRaw(opts.Connections)
	raw.Conn["API"] = &config.Conn{Type: "api", BaseURL: srv.URL}
	config.WriteConnections(opts.Connections, raw)
	e, err := newExplorer(opts, "API")
	if err != nil || e.page != pageEndpoint {
		t.Fatal(err)
	}
	e.endpoint.set("path", "/x")
	e.endpoint.set("records", "data[]")
	press1(e, keySave)
	if e.page != pagePreview || len(e.cols) != 3 || len(e.rows) != 2 {
		t.Fatalf("preview: %d %d %d %s", e.page, len(e.cols), len(e.rows), e.fetchErr)
	}
	v := strings.Join(e.view(80, 20), "\n")
	if !strings.Contains(v, "meta_k") || !strings.Contains(v, "bool") {
		t.Fatalf("api grid:\n%s", v)
	}
	e.update(keyRunes("n"))
	e.update(keySpace) // only "id"
	e.update(keyRunes("+"))
	e.repPick.set("name", "apix")
	e.repPick.set("target", "TGT")
	e.update(keySave)
	e.stream.set("name", "xs")
	if e.update(keySave); e.page != pagePreview {
		t.Fatalf("save: %q", e.stream.err)
	}
	s := streamOf(readRep(t, opts, "apix"), "xs")
	if s.Path != "/x" || s.Records != "data[]" || strings.Join(s.Select, ",") != "id" {
		t.Fatalf("written: %+v", s)
	}
	// a wrong records path lists the keys of the response
	e.page = pageEndpoint
	e.endpoint.set("records", "items[]")
	press1(e, keySave)
	if len(e.rows) != 0 || !strings.Contains(e.note, "keys: data") {
		t.Fatalf("keys: %q", e.note)
	}
}

func TestExplorerForEach(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "ann"}, {"id": 2, "name": "bob"}})
		case "/users/1/posts":
			json.NewEncoder(w).Encode([]map[string]any{{"pid": 10}})
		case "/users/2/posts":
			json.NewEncoder(w).Encode([]map[string]any{{"pid": 20}, {"pid": 21}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	opts := testHome(t)
	raw, _ := config.LoadConnectionsRaw(opts.Connections)
	raw.Conn["API"] = &config.Conn{Type: "api", BaseURL: srv.URL}
	config.WriteConnections(opts.Connections, raw)
	os.WriteFile(filepath.Join(config.ReplicationsDir(opts.Home), "jp.toml"), []byte("source = \"API\"\ntarget = \"TGT\"\n[streams.users]\npath = \"/users\"\n[streams.posts]\npath = \"/users/{users.id}/posts\"\nfor_each = \"users\"\nadd_columns = { user = \"{users.name}\" }\n"), 0o644)
	r := readRep(t, opts, "jp")
	var posts config.ResolvedStream
	for _, s := range r.Resolved {
		if s.Name == "posts" {
			posts = s
		}
	}
	e, err := newStreamExplorer(opts, "API", posts)
	if err != nil || e.repName != "jp" {
		t.Fatalf("%v %q", err, e.repName)
	}
	run(e, e.start())
	if len(e.rows) != 3 || len(e.cols) != 2 || e.fetchErr != "" {
		t.Fatalf("for_each preview: %d rows %v %s", len(e.rows), e.colNames(), e.fetchErr)
	}
	// on the endpoint form, typed values stand in for the parent
	e2, _ := newExplorer(opts, "API")
	e2.endpoint.set("path", "/users/{users.id}/posts")
	if e2.endpoint.field("vars") == nil || len(e2.endpoint.visible()) != 6 {
		t.Fatalf("values field: %d", len(e2.endpoint.visible()))
	}
	e2.endpoint.set("vars", "users.id = 2")
	press1(e2, keySave)
	if len(e2.rows) != 2 {
		t.Fatalf("typed values: %d %s", len(e2.rows), e2.fetchErr)
	}
}

func TestEnterShowsTargetData(t *testing.T) {
	opts := testHome(t)
	m := loaded(t, opts)
	rep, _ := m.rep("alpha")
	for range runBatch(t, m.runNow(rep, nil, false)) {
	}
	m = loaded(t, opts)
	m = press(m, "enter", "enter") // alpha › things › data
	e, ok := m.wiz.(*explorer)
	if !ok || !e.readOnly || e.table != "things" || e.conn.Name != "TGT" {
		t.Fatalf("enter should open the target table: %+v", m.wiz)
	}
	run(e, e.start())
	if len(e.rows) != 3 || len(e.cols) != 2 {
		t.Fatalf("target rows: %d %s", len(e.rows), e.fetchErr)
	}
	if res, _ := e.update(keyRunes("+")); res != wizContinue || e.page != pagePreview {
		t.Fatal("+ must do nothing on target data")
	}
	if e.update(keyRunes("t")); e.page != pagePreview {
		t.Fatal("no steps on target data")
	}
	m = press(m, "esc", "H")
	if m.view != viewHistory || m.streamName != "things" {
		t.Fatalf("H should open the stream history: %d %q", m.view, m.streamName)
	}
}

func TestPreviewStream(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "enter", "j", "p") // alpha › copy (sql stream)
	if m.view != viewWizard || m.wiz == nil {
		t.Fatalf("p should open the explorer: %d", m.view)
	}
	e := m.wiz.(*explorer)
	run(e, e.start())
	v := strings.Join(e.view(80, 20), "\n")
	if e.page != pagePreview || len(e.cols) != 2 || !strings.Contains(v, "3 rows") || !strings.Contains(v, "sql›") || e.repName != "alpha" {
		t.Fatalf("sql stream preview: %d %d %q\n%s", e.page, len(e.cols), e.repName, v)
	}
	e.update(keyRunes("S"))
	if e.sqlInput.Value() != "select * from things" {
		t.Fatalf("S on a sql stream starts from its sql: %q", e.sqlInput.Value())
	}
	e.update(keyEsc)
	if res, _ := e.update(keyEsc); res != wizCancel {
		t.Fatal("esc should leave the stream preview")
	}
	m = press(m, "esc", "k", "p") // things (table stream)
	e = m.wiz.(*explorer)
	run(e, e.start())
	if e.table != "things" || len(e.rows) != 3 {
		t.Fatalf("table preview: %q %d %s", e.table, len(e.rows), e.fetchErr)
	}
}

func TestPreviewWidthsAndValue(t *testing.T) {
	e := openThings(t, testHome(t))
	base := e.colWidth(1)
	e.update(keyRight)
	e.update(keyRunes(">"))
	if e.colWidth(1) != base+8 {
		t.Fatalf("widen: %d -> %d", base, e.colWidth(1))
	}
	e.update(keyRunes("<"))
	e.update(keyRunes("<"))
	if e.colWidth(1) != max(base-8, 4) {
		t.Fatalf("narrow: %d", e.colWidth(1))
	}
	e.update(keyRunes("="))
	if e.colWidth(1) != base {
		t.Fatal("reset")
	}
	e.update(keyRunes("j"))
	e.update(keyRunes("v"))
	v := strings.Join(e.view(80, 20), "\n")
	if !e.showVal || !strings.Contains(v, "row 2 of 3") || !strings.Contains(v, "\n  b") {
		t.Fatalf("value box:\n%s", v)
	}
	e.update(keyRunes("x"))
	if e.showVal {
		t.Fatal("any key closes the value box")
	}
	v = strings.Join(e.view(80, 20), "\n")
	if !strings.Contains(v, "› ") {
		t.Fatalf("row cursor missing:\n%s", v)
	}
	e.update(keyRunes("w"))
	if !e.wide {
		t.Fatal("wide toggle")
	}
}

func TestPreviewMore(t *testing.T) {
	opts := testHome(t)
	src, _ := sql.Open("sqlite", filepath.Join(opts.Home, "src.db"))
	src.Exec("CREATE TABLE big (n INTEGER PRIMARY KEY)")
	src.Exec("WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM s WHERE n < 500) INSERT INTO big SELECT n FROM s")
	src.Close()
	e, _ := newExplorer(opts, "SRC")
	run(e, e.start())
	e.update(keyRunes("/"))
	typeInto(e, "big")
	e.update(keyEnter)
	press1(e, keyEnter)
	if len(e.rows) != 200 {
		t.Fatalf("first fetch: %d rows", len(e.rows))
	}
	e.update(keyRunes("G"))
	press1(e, keyRunes("m"))
	if len(e.rows) != 400 || e.rowCur != 199 {
		t.Fatalf("more: %d rows, cursor %d", len(e.rows), e.rowCur)
	}
	press1(e, keyRunes("m"))
	if len(e.rows) != 500 {
		t.Fatalf("more again: %d rows", len(e.rows))
	}
}

func TestPreviewWildcardOpensFilteredTables(t *testing.T) {
	opts := testHome(t)
	os.WriteFile(filepath.Join(config.ReplicationsDir(opts.Home), "wild.toml"), []byte("source = \"SRC\"\ntarget = \"TGT\"\n[streams.\"main.*\"]\n"), 0o644)
	m := loaded(t, opts)
	m = press(m, "G", "enter", "p")
	e, ok := m.wiz.(*explorer)
	if !ok || e.page != pageTables || e.filter.Value() != "main." {
		t.Fatalf("wildcard preview: %+v", m.wiz)
	}
}

func TestWildcardCarriesSettings(t *testing.T) {
	opts := testHome(t)
	src, _ := sql.Open("sqlite", filepath.Join(opts.Home, "src.db"))
	src.Exec("CREATE TABLE other (id INTEGER, v TEXT)")
	src.Exec("INSERT INTO other VALUES (1,'a'),(2,'b'),(3,'c')")
	src.Close()
	os.WriteFile(filepath.Join(config.ReplicationsDir(opts.Home), "wild.toml"), []byte("source = \"SRC\"\ntarget = \"TGT\"\n[streams.\"o*\"]\nwhere = \"id > 1\"\nsteps = \"limit 1\"\n"), 0o644)
	// the table list of the wildcard
	e, err := newWildcardExplorer(opts, "wild", "SRC", config.ResolvedStream{Name: "o*"})
	if err != nil {
		t.Fatal(err)
	}
	run(e, e.start())
	if e.filter.Value() != "o" || len(e.visibleTables()) != 1 {
		t.Fatalf("filtered: %q %v", e.filter.Value(), e.visibleTables())
	}
	press1(e, keyEnter)
	if len(e.rows) != 1 || len(e.pipeline) != 1 || e.spec.Where != "id > 1" {
		t.Fatalf("wildcard settings: rows=%d %+v %s", len(e.rows), e.pipeline, e.fetchErr)
	}
	e.update(keyRight)
	e.update(keySpace)
	e.update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if s := streamOf(readRep(t, opts, "wild"), "other"); s.Steps != "limit 1 | keep id" || s.Where != "id > 1" {
		t.Fatalf("written as its own stream: %+v (%s)", s, e.note)
	}
	// an expanded stream previews with the wildcard's settings
	e2, _ := newStreamExplorer(opts, "SRC", config.ResolvedStream{Name: "other"})
	defer e2.h.close()
	if e2.repName != "wild" {
		t.Fatalf("rep: %q", e2.repName)
	}
}

func TestStepsPanelAndPalette(t *testing.T) {
	opts := testHome(t)
	e := openThings(t, opts)
	// t opens the step list; pick filter
	e.update(keyRunes("t"))
	if e.page != pagePalette {
		t.Fatal("t opens the step list")
	}
	typeInto(e, "filt")
	e.update(keyEnter)
	if e.page != pageStep || e.stepEd.op != "filter" || e.stepEd.form == nil {
		t.Fatalf("filter editor: page=%d", e.page)
	}
	e.stepEd.form.set("op", ">")
	vf := e.stepEd.form.field("val")
	if strings.Join(vf.Options, ",") != "1,2,3" {
		t.Fatalf("value list from the preview: %v", vf.Options)
	}
	vf.Picked["1"] = true
	_, cmd := e.update(keySave)
	if e.page != pagePreview || len(e.pipeline) != 1 || cmd == nil {
		t.Fatalf("step applied: page=%d steps=%d %s", e.page, len(e.pipeline), e.stepEd.err)
	}
	run(e, cmd)
	if len(e.rows) != 2 || !strings.Contains(e.compiled, `WHERE "id" > 1`) {
		t.Fatalf("after filter: rows=%d sql=%s err=%s", len(e.rows), e.compiled, e.fetchErr)
	}
	if e.focus != focusGrid {
		t.Fatal("focus stays on the grid")
	}
	v := strings.Join(e.view(120, 24), "\n")
	if !strings.Contains(v, `1  filter "id" > 1`) || !strings.Contains(v, "│") || !strings.Contains(v, `WHERE "id" > 1`) {
		t.Fatalf("panel:\n%s", v)
	}
	// palette: real columns in the examples, keys beside commands
	e.update(keyRunes(":"))
	if e.palette.note["keep"] != "id, v" || e.palette.note["more"] != "m" {
		t.Fatalf("palette notes: %v", e.palette.note)
	}
	typeInto(e, "lim")
	if vis := e.palette.visible(); len(vis) != 1 || vis[0] != "limit" {
		t.Fatalf("palette filter: %v", vis)
	}
	e.update(keyEnter)
	if e.page != pageStep || e.stepEd.op != "limit" {
		t.Fatalf("palette picks limit: page=%d", e.page)
	}
	e.stepEd.form.set("n", "x")
	e.update(keySave)
	if e.page != pageStep || e.stepEd.form.field("n").err == "" {
		t.Fatal("bad limit refused")
	}
	e.stepEd.form.set("n", "1")
	press1(e, keySave)
	if len(e.pipeline) != 2 || len(e.rows) != 1 {
		t.Fatalf("limit applied: steps=%d rows=%d", len(e.pipeline), len(e.rows))
	}
	// keep through the column picker: untick v
	e.update(keyRunes("t"))
	typeInto(e, "keep")
	e.update(keyEnter)
	if e.page != pageStep || e.stepEd.pick == nil {
		t.Fatal("keep opens a column picker")
	}
	e.update(keyRunes("a"))
	e.update(keyRunes("j"))
	e.update(keySpace) // untick v
	press1(e, keyEnter)
	if e.pipeline[2].String() != "keep id" || len(e.cols) != 1 {
		t.Fatalf("keep: %+v cols=%d", e.pipeline, len(e.cols))
	}
	// sort: pick id, mark descending
	e.update(keyRunes("t"))
	typeInto(e, "sort")
	e.update(keyEnter)
	e.update(keySpace)
	e.update(keyEnter)
	if e.stepEd.stage != 1 || e.stepEd.form == nil {
		t.Fatal("sort second stage")
	}
	e.update(keySpace) // desc
	press1(e, keySave)
	if e.pipeline[3].String() != "sort -id" {
		t.Fatalf("sort: %+v", e.pipeline)
	}
	// editing the sort keeps the direction
	e.update(keyTab)
	e.update(keyRunes("G"))
	press1(e, keyRunes("e"))
	if e.page != pageStep || e.stepEd.op != "sort" {
		t.Fatalf("edit sort: page=%d", e.page)
	}
	e.update(keyEnter)
	if !e.stepEd.form.on("id") {
		t.Fatal("desc kept")
	}
	e.update(keyEsc)
	e.update(keyEsc)
	e.update(keyTab)
	// distinct needs no input
	e.update(keyRunes(":"))
	typeInto(e, "dist")
	_, cmd = e.update(keyEnter)
	if e.page != pagePreview || len(e.pipeline) != 5 || e.pipeline[4].Op != "distinct" {
		t.Fatalf("distinct: page=%d %+v", e.page, e.pipeline)
	}
	run(e, cmd)
	// steps panel: delete the limit (index 1), asked first
	e.update(keyTab)
	if e.focus != focusSteps {
		t.Fatal("tab focuses the steps")
	}
	e.update(keyRunes("g"))
	e.update(keyRunes("j"))
	e.update(keyRunes("d"))
	if e.ask == nil || len(e.pipeline) != 5 {
		t.Fatal("d asks first")
	}
	e.update(keyRunes("n"))
	if e.ask != nil || len(e.pipeline) != 5 {
		t.Fatal("n keeps the step")
	}
	e.update(keyRunes("d"))
	press1(e, keyRunes("y"))
	if len(e.pipeline) != 4 || len(e.rows) != 2 {
		t.Fatalf("delete step: steps=%d rows=%d", len(e.pipeline), len(e.rows))
	}
	// editing the filter reopens it with the values before it
	e.update(keyRunes("g"))
	_, cmd = e.update(keyRunes("e"))
	if e.stepWait != 0 || cmd == nil {
		t.Fatal("edit loads the preview before the step")
	}
	run(e, cmd)
	if e.page != pageStep || e.stepEd.op != "filter" || e.stepEd.form.get("col") != "id" || strings.Join(e.stepEd.form.picked("val"), ",") != "1" {
		t.Fatalf("edit prefill: %s %q %v", e.stepEd.op, e.stepEd.form.get("col"), e.stepEd.form.picked("val"))
	}
	if got := strings.Join(e.stepEd.form.field("val").Options, ","); got != "1,2,3" {
		t.Fatalf("values before the filter: %s", got)
	}
	e.update(keyEsc)
	// save writes steps
	e.update(keyTab)
	e.update(keyRunes("+"))
	e.repPick.set("name", "stepped")
	e.repPick.set("target", "TGT")
	e.update(keySave)
	if e.update(keySave); e.page != pagePreview {
		t.Fatalf("save: %q", e.stream.err)
	}
	if s := streamOf(readRep(t, opts, "stepped"), "things"); s.Steps != `filter "id" > 1 | keep id | sort -id | distinct` {
		t.Fatalf("steps written: %+v", s)
	}
	// reopening the stream restores the steps
	m := loaded(t, opts)
	m = press(m, "/")
	for _, r := range "stepped" {
		m = press(m, string(r))
	}
	m = press(m, "enter", "enter", "p")
	e2 := m.wiz.(*explorer)
	defer e2.h.close()
	if len(e2.pipeline) != 4 || e2.pipeline[0].Op != "filter" || e2.repName != "stepped" {
		t.Fatalf("restored: %+v", e2.pipeline)
	}
}

// The old crash: a step that narrows the columns while the cursor sits further right.
func TestNarrowingStepKeepsCursorValid(t *testing.T) {
	e := openThings(t, testHome(t))
	e.view(120, 24)
	e.update(keyRight)
	e.stepEdit = -1
	run(e, e.applyStep(steps.Step{Op: "keep", Args: "id"}))
	if e.colCur != 0 || len(e.cols) != 1 {
		t.Fatalf("cursor: %d of %d", e.colCur, len(e.cols))
	}
	e.update(keyTab)
	e.update(keyTab)
	e.update(keyRunes(">"))
	e.view(120, 24)
	e.view(60, 24)
}

func TestPipelineResetsOnAnotherTable(t *testing.T) {
	opts := testHome(t)
	src, _ := sql.Open("sqlite", filepath.Join(opts.Home, "src.db"))
	src.Exec("CREATE TABLE more_things (k INTEGER)")
	src.Close()
	e := openThings(t, opts)
	run(e, e.applyStep(steps.Step{Op: "limit", Args: "1"}))
	if res, _ := e.update(keyEsc); res != wizContinue || e.ask == nil {
		t.Fatal("esc with steps asks")
	}
	e.update(keyRunes("y"))
	if e.page != pageTables || len(e.pipeline) != 0 {
		t.Fatalf("back on the tables without steps: %d %v", e.page, e.pipeline)
	}
	e.update(keyRunes("G"))
	press1(e, keyEnter)
	if len(e.pipeline) != 0 {
		t.Fatalf("steps carried over: %+v", e.pipeline)
	}
}

func TestSaveBackToStream(t *testing.T) {
	opts := testHome(t)
	os.WriteFile(filepath.Join(config.ReplicationsDir(opts.Home), "keep.toml"), []byte(
		"source = \"SRC\"\ntarget = \"TGT\"\n[streams.things]\nprimary_key = [\"id\"]\nlimit = 5\ndisabled = true\nquery_timeout = \"30s\"\nselect = [\"id\"]\n"), 0o644)
	r := readRep(t, opts, "keep")
	e, err := newRepStreamExplorer(opts, "keep", "SRC", r.Resolved[0])
	if err != nil {
		t.Fatal(err)
	}
	defer e.h.close()
	run(e, e.start())
	if len(e.cols) != 2 || strings.Join(e.tickedCols(), ",") != "id" {
		t.Fatalf("select as ticks: %v %v", e.colNames(), e.tickedCols())
	}
	if e.dirty() {
		t.Fatal("clean after opening")
	}
	run(e, e.applyStep(steps.Step{Op: "limit", Args: "2"}))
	if !e.dirty() {
		t.Fatal("dirty after a step")
	}
	if res, _ := e.update(keyEsc); res != wizContinue || e.ask == nil {
		t.Fatal("esc asks with unsaved steps")
	}
	e.update(keyRunes("n"))
	e.update(tea.KeyMsg{Type: tea.KeyCtrlS})
	s := streamOf(readRep(t, opts, "keep"), "things")
	if s.Steps != "limit 2 | keep id" || len(s.Select) != 0 || s.Limit != 5 || !s.Disabled || s.QueryTimeout != "30s" {
		t.Fatalf("saved back: %+v (%s)", s, e.note)
	}
	if e.dirty() {
		t.Fatal("clean after saving")
	}
	// + on the stream edits it in place and keeps every key
	e.update(keyRunes("+"))
	if e.repPick.get("rep") != "keep" {
		t.Fatalf("+ defaults to the stream's replication: %q", e.repPick.get("rep"))
	}
	e.update(keySave)
	if e.editing != "things" || e.stream.get("limit") != "5" || !e.stream.on("disabled") || strings.Join(e.stream.picked("primary_key"), ",") != "id" {
		t.Fatalf("prefilled from the stream: %q %q", e.editing, e.stream.get("limit"))
	}
	e.stream.set("object", "things_out")
	if e.update(keySave); e.page != pagePreview {
		t.Fatalf("save: %q", e.stream.err)
	}
	s = streamOf(readRep(t, opts, "keep"), "things")
	if s.Object != "things_out" || s.Limit != 5 || !s.Disabled || s.QueryTimeout != "30s" || s.Steps != "limit 2 | keep id" {
		t.Fatalf("kept: %+v", s)
	}
}

func TestFocusAndNarrowPanel(t *testing.T) {
	e := openThings(t, testHome(t))
	run(e, e.applyStep(steps.Step{Op: "limit", Args: "2"}))
	// narrow: tab shows the steps and the sql in place of the grid
	v := strings.Join(e.view(80, 20), "\n")
	if !strings.Contains(v, "● id") {
		t.Fatalf("grid first:\n%s", v)
	}
	if l, _ := e.footer(); !strings.Contains(l, "tab") {
		t.Fatalf("footer keeps tab steps: %s", l)
	}
	e.update(keyTab)
	if e.focus != focusSteps {
		t.Fatal("tab focuses the steps when narrow")
	}
	v = strings.Join(e.view(80, 20), "\n")
	if !strings.Contains(v, "1  limit 2") || !strings.Contains(v, "LIMIT 2") || strings.Contains(v, "● id") {
		t.Fatalf("steps in place of the grid:\n%s", v)
	}
	e.update(keyRunes("d"))
	if e.ask == nil {
		t.Fatal("steps can be deleted when narrow")
	}
	e.update(keyRunes("n"))
	e.update(keyTab)
	if e.focus != focusGrid {
		t.Fatal("tab back to the grid")
	}
	e.view(120, 20)
	e.update(keyTab)
	if e.focus != focusSteps {
		t.Fatal("tab with the panel")
	}
	// a failing step keeps the panel up
	e.pipeline = append(e.pipeline, steps.Step{Op: "keep", Args: "nope"})
	run(e, e.fetch())
	v = strings.Join(e.view(120, 20), "\n")
	if e.fetchErr == "" || !strings.Contains(v, "2  keep nope") {
		t.Fatalf("panel with an error:\n%s", v)
	}
}

func TestStepEditsUseColumnsBeforeTheStep(t *testing.T) {
	e := openThings(t, testHome(t))
	run(e, e.applyStep(steps.Step{Op: "rename", Args: "v = w"}))
	run(e, e.applyStep(steps.Step{Op: "limit", Args: "5"}))
	if e.colNames()[1] != "w" {
		t.Fatalf("renamed: %v", e.colNames())
	}
	e.stepCur = 0
	e.focus = focusSteps
	press1(e, keyRunes("e"))
	if e.page != pageStep || e.stepEd.form.field("v") == nil || e.stepEd.form.get("v") != "w" {
		t.Fatalf("rename edits the names before it: %v", e.stepEd.cols)
	}
	press1(e, keySave)
	if e.pipeline[0].String() != "rename v = w" {
		t.Fatalf("unchanged save: %+v (%s)", e.pipeline, e.fetchErr)
	}
	// ticks after steps: renamed columns ticked, unticks become a keep
	if !e.ticked["w"] {
		t.Fatal("renamed column ticked")
	}
	e.focus = focusGrid
	e.colCur = 1
	e.update(keySpace)
	v := strings.Join(e.view(120, 24), "\n")
	if !strings.Contains(v, "keep id  ticks") {
		t.Fatalf("ticks shown as a keep:\n%s", v)
	}
}

func TestGuidedFilterSQL(t *testing.T) {
	d := pg(t)
	cases := []struct {
		f    filterSpec
		want string
	}{
		{filterSpec{col: "status", op: "=", vals: []string{"paid"}}, `"status" = 'paid'`},
		{filterSpec{col: "total", kind: db.Float, op: ">=", vals: []string{"10.5"}}, `"total" >= 10.5`},
		{filterSpec{col: "zip", op: "=", vals: []string{"00123"}}, `"zip" = '00123'`},
		{filterSpec{col: "total", kind: db.Float, op: "=", vals: []string{"NaN"}}, `"total" = 'NaN'`},
		{filterSpec{col: "status", op: "contains", vals: []string{"pai"}}, `"status" LIKE '%pai%'`},
		{filterSpec{col: "status", op: "starts with", vals: []string{"50%"}}, `"status" LIKE '50!%%' ESCAPE '!'`},
		{filterSpec{col: "status", op: "ends with", vals: []string{"a!"}}, `"status" LIKE '%a!!' ESCAPE '!'`},
		{filterSpec{col: "name", op: "in", vals: []string{"Smith, John", "Doe"}}, `"name" IN ('Smith, John', 'Doe')`},
		{filterSpec{col: "id", kind: db.Int, op: "in", vals: []string{"1", "2"}}, `"id" IN (1, 2)`},
		{filterSpec{col: "total", op: "is null"}, `"total" IS NULL`},
		{filterSpec{col: "placed on", kind: db.Date, op: "<", vals: []string{"2026-01-01"}}, `"placed on" < '2026-01-01'`},
		{filterSpec{col: "status", op: "!=", vals: []string{"o'neil"}}, `"status" != 'o''neil'`},
		{filterSpec{col: "status", op: "!=", vals: []string{"x"}, keepNulls: true}, `("status" != 'x' OR "status" IS NULL)`},
		{filterSpec{col: "status", op: "not in", vals: []string{"a", "b"}}, `"status" NOT IN ('a', 'b')`},
		{filterSpec{col: "status", op: "=", vals: []string{"x", nullItem}}, `("status" = 'x' OR "status" IS NULL)`},
		{filterSpec{col: "user", op: "=", vals: []string{"bob"}}, `"user" = 'bob'`},
		{filterSpec{col: "active", kind: db.Bool, op: "=", vals: []string{"true"}}, `"active" = TRUE`},
		{filterSpec{col: "c", op: "=", vals: []string{"'x' OR 'a'='a'"}}, `"c" = '''x'' OR ''a''=''a'''`},
		{filterSpec{col: "c", op: "=", vals: []string{"a | set x = 1"}}, `"c" = 'a | set x = 1'`},
	}
	cols := []string{"status", "total", "zip", "name", "id", "placed on", "user", "active", "c"}
	kinds := map[string]db.Kind{"total": db.Float, "id": db.Int, "placed on": db.Date, "active": db.Bool}
	for _, c := range cases {
		got, err := c.f.sql(d)
		if err != nil || got != c.want {
			t.Errorf("%+v:\ngot  %s %v\nwant %s", c.f, got, err, c.want)
			continue
		}
		p, ok := parseFilter(got, cols, kinds, d)
		if !ok || p.col != c.f.col || strings.Join(p.vals, "|") != strings.Join(c.f.vals, "|") || p.keepNulls != c.f.keepNulls {
			t.Errorf("parse %s: %+v %v", got, p, ok)
		}
		// the step survives the pipeline parser as one step
		if st, err := steps.Parse("filter " + got + " | limit 1"); err != nil || len(st) != 2 {
			t.Errorf("pipeline %s: %v %+v", got, err, st)
		}
	}
	for _, bad := range []filterSpec{
		{col: "t", op: ">", vals: []string{"1", "2"}},
		{col: "t", op: "contains", vals: []string{"a", "b"}},
		{col: "t", op: "=", vals: []string{"1", "2"}},
		{col: "t", op: "!=", vals: []string{"1", "2"}},
	} {
		if _, err := bad.sql(d); err == nil {
			t.Errorf("%+v should need one value", bad)
		}
	}
	if _, ok := parseFilter(`"total" > 10 and "status" = 'paid'`, cols, kinds, d); ok {
		t.Error("compound condition is not guided")
	}
	// older unquoted conditions still open guided
	if p, ok := parseFilter("status IN ('paid', 'pending')", cols, kinds, d); !ok || p.op != "in" || len(p.vals) != 2 {
		t.Errorf("legacy: %+v", p)
	}
	if p, ok := parseFilter("NOT status IN ('a')", cols, kinds, d); !ok || p.op != "not in" {
		t.Errorf("legacy not in: %+v", p)
	}
}

func TestFilterDialects(t *testing.T) {
	my, _ := db.DialectFor("mysql")
	ms, _ := db.DialectFor("mssql")
	lite, _ := db.DialectFor("sqlite")
	f := filterSpec{col: "Order Date", op: "=", vals: []string{`\' OR 1=1 -- `}}
	got, _ := f.sql(my)
	if got != "`Order Date` = '\\\\'' OR 1=1 -- '" {
		t.Errorf("mysql: %s", got)
	}
	if p, ok := parseFilter(got, []string{"Order Date"}, nil, my); !ok || p.vals[0] != `\' OR 1=1 -- ` {
		t.Errorf("mysql parse: %+v", p)
	}
	b := filterSpec{col: "on", kind: db.Bool, op: "=", vals: []string{"true"}}
	if got, _ := b.sql(ms); got != "[on] = 1" {
		t.Errorf("mssql bool: %s", got)
	}
	if got, _ := b.sql(lite); got != `"on" = 1` {
		t.Errorf("sqlite bool: %s", got)
	}
	if p, ok := parseFilter("[on] = 1", []string{"on"}, map[string]db.Kind{"on": db.Bool}, ms); !ok || p.vals[0] != "true" {
		t.Errorf("mssql bool parse: %+v", p)
	}
	l := filterSpec{col: "p", op: "contains", vals: []string{`a\b`}}
	if got, _ := l.sql(my); got != "`p` LIKE '%a\\\\b%' ESCAPE '!'" {
		t.Errorf("mysql like: %s", got)
	}
}

func TestGuidedFilterEditor(t *testing.T) {
	d := pg(t)
	ctx := stepContext{dl: d, cols: []db.Column{{Name: "id", Kind: db.Int}, {Name: "city"}, {Name: "total", Kind: db.Float}, {Name: "bad|name"}},
		rows: [][]any{{int64(1), "Oslo", 2.5, "x"}, {int64(2), nil, 3.0, "y"}, {int64(3), "Rome", nil, "z"}}}
	ed := newStepEditor("filter", ctx, nil)
	if ed.form.get("mode") != "pick" || ed.form.get("col") != "id" {
		t.Fatal("default is guided on the first column")
	}
	if opts := ed.form.field("col").Options; len(opts) != 3 {
		t.Fatalf("unsafe column names not offered: %v", opts)
	}
	if ops := strings.Join(ed.form.field("op").Options, ","); strings.Contains(ops, "contains") {
		t.Fatalf("no LIKE on numbers: %s", ops)
	}
	ed.update(keyDown)  // col
	ed.update(keyRight) // city: lists follow the column
	if got := strings.Join(ed.form.field("val").Options, ","); got != nullItem+",Oslo,Rome" {
		t.Fatalf("values: %s", got)
	}
	if ops := strings.Join(ed.form.field("op").Options, ","); !strings.Contains(ops, "contains") {
		t.Fatalf("LIKE on text: %s", ops)
	}
	// a typed value with a space
	ed.update(keyDown) // op
	ed.update(keyDown) // value
	ed.update(keySpace)
	for _, r := range "New" {
		ed.update(keyRunes(string(r)))
	}
	ed.update(keySpace)
	for _, r := range "York" {
		ed.update(keyRunes(string(r)))
	}
	ed.update(keyTab) // tick the typed value
	ed.update(keyUp)
	ed.update(keyUp)
	ed.update(keyTab) // and Oslo
	ed.update(keyEnter)
	ed.update(keySave)
	if ed.result.String() != `filter "city" IN ('Oslo', 'New York')` && ed.result.String() != `filter "city" IN ('New York', 'Oslo')` {
		t.Fatalf("typed value with a space: %s (%v)", ed.result, ed.form.picked("val"))
	}
	// reopening keeps the values as a list
	ed = newStepEditor("filter", ctx, &ed.result)
	if got := ed.form.picked("val"); len(got) != 2 {
		t.Fatalf("prefill: %v", got)
	}
	// > needs one value
	ed = newStepEditor("filter", ctx, nil)
	ed.form.set("op", ">")
	ed.form.field("val").Picked["1"], ed.form.field("val").Picked["2"] = true, true
	if res, _ := ed.update(keySave); res != formContinue || ed.form.field("val").err != "one value" {
		t.Fatalf("one value: %q", ed.form.field("val").err)
	}
	// is null applies without a value
	ed = newStepEditor("filter", ctx, &steps.Step{Op: "filter", Args: `"total" IS NULL`})
	if ed.form.get("op") != "is null" || len(ed.form.visible()) != 3 {
		t.Fatalf("is null prefill: %q %d", ed.form.get("op"), len(ed.form.visible()))
	}
	ed.update(keySave)
	if ed.result.String() != `filter "total" IS NULL` {
		t.Fatalf("is null: %s", ed.result)
	}
	ed = newStepEditor("filter", ctx, &steps.Step{Op: "filter", Args: `"total" > 10 and "id" = 1`})
	if ed.form.get("mode") != "sql" || ed.form.get("expr") != `"total" > 10 and "id" = 1` {
		t.Fatal("compound condition opens raw")
	}
	// != keeps nulls unless switched off
	ed = newStepEditor("filter", ctx, nil)
	ed.form.set("col", "city")
	ed.follow()
	ed.form.set("op", "!=")
	ed.form.field("val").Picked["Oslo"] = true
	ed.update(keySave)
	if ed.result.String() != `filter ("city" != 'Oslo' OR "city" IS NULL)` {
		t.Fatalf("keep nulls: %s %q %v", ed.result, ed.err, func() []string {
			var out []string
			for _, f := range ed.form.visible() {
				out = append(out, f.Key+"="+f.err)
			}
			return out
		}())
	}
}

func TestStepEditorForms(t *testing.T) {
	d := pg(t)
	ctx := ctxOf(d, db.Column{Name: "id", Kind: db.Int}, db.Column{Name: "Name"}, db.Column{Name: "total", Kind: db.Float},
		db.Column{Name: "placed", Kind: db.Date}, db.Column{Name: "x | set y = 1"})
	ed := newStepEditor("rename", ctx, nil)
	if ed.form.field("x | set y = 1") != nil {
		t.Fatal("unsafe names are not offered")
	}
	ed.form.set("Name", "customer")
	ed.update(keySave)
	if ed.result.String() != "rename Name = customer" {
		t.Fatalf("rename: %s", ed.result)
	}
	ed = newStepEditor("rename", ctx, nil)
	ed.form.set("Name", "total")
	if res, _ := ed.update(keySave); res == formSubmit || ed.form.field("Name").err != "exists" {
		t.Fatal("rename onto an existing name")
	}
	ed.form.set("Name", "a|b")
	if res, _ := ed.update(keySave); res == formSubmit {
		t.Fatal("rename to an unsafe name")
	}
	// cast: several pairs, case as the column has it
	ed = newStepEditor("cast", ctx, &steps.Step{Op: "cast", Args: "total as float, name as int"})
	if got := strings.Join(ed.pick.chosenInOrder(), ","); got != "total,Name" {
		t.Fatalf("cast prefill: %s", got)
	}
	ed.update(keyEnter)
	if ed.form.get("total") != "float" || ed.form.get("Name") != "int" {
		t.Fatal("cast types prefill")
	}
	ed.form.set("total", "int")
	ed.update(keySave)
	if ed.result.String() != "cast total as int, Name as int" {
		t.Fatalf("cast: %s", ed.result)
	}
	ed = newStepEditor("coalesce", ctx, &steps.Step{Op: "coalesce", Args: "Name = '', total = 0"})
	ed.update(keyEnter)
	ed.form.set("total", "1")
	ed.update(keySave)
	if ed.result.String() != "coalesce Name = '', total = 1" {
		t.Fatalf("coalesce: %s", ed.result)
	}
	ed = newStepEditor("remove", ctx, &steps.Step{Op: "remove", Args: "name"})
	if got := ed.pick.chosen(); len(got) != 1 || got[0] != "Name" {
		t.Fatal("remove prefill")
	}
	// set: expression, concat, case, date part
	ed = newStepEditor("set", ctx, nil)
	ed.form.set("name", "double")
	ed.form.set("expr", "total * 2")
	ed.update(keySave)
	if ed.result.String() != "set double = total * 2" {
		t.Fatalf("set: %s", ed.result)
	}
	ed = newStepEditor("set", ctx, nil)
	ed.form.set("build", "concat")
	ed.form.set("name", "label")
	ed.form.field("ccols").Picked["id"], ed.form.field("ccols").Picked["Name"] = true, true
	ed.form.set("sep", "dash")
	ed.update(keySave)
	if ed.result.String() != `set label = CONCAT_WS('-', "id", "Name")` {
		t.Fatalf("concat: %s", ed.result)
	}
	ed = newStepEditor("set", ctx, nil)
	ed.form.set("build", "case")
	ed.form.set("name", "big")
	ed.form.set("wcol", "total")
	ed.form.set("wop", ">")
	ed.form.set("wval", "100")
	ed.form.set("then", "yes")
	ed.form.set("else", "0")
	ed.update(keySave)
	if ed.result.String() != `set big = CASE WHEN "total" > 100 THEN 'yes' ELSE 0 END` {
		t.Fatalf("case: %s", ed.result)
	}
	ed = newStepEditor("set", ctx, nil)
	ed.form.set("build", "date part")
	ed.form.set("name", "year")
	ed.form.set("part", "year")
	ed.update(keySave)
	if ed.result.String() != `set year = CAST(EXTRACT(YEAR FROM "placed") AS integer)` {
		t.Fatalf("date part: %s", ed.result)
	}
	// editing a set keeps every pair
	ed = newStepEditor("set", ctx, &steps.Step{Op: "set", Args: "a = 1, b = CONCAT_WS(' ', \"id\", \"Name\")"})
	if ed.form.get("build") != "none" || ed.form.get("expr1") != `CONCAT_WS(' ', "id", "Name")` {
		t.Fatalf("set pairs: %q", ed.form.get("expr1"))
	}
	ed.update(keySave)
	if ed.result.String() != `set a = 1, b = CONCAT_WS(' ', "id", "Name")` {
		t.Fatalf("set edit: %s", ed.result)
	}
	for _, r := range []steps.Step{ed.result} {
		if _, err := steps.Parse(r.String()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetectTypes(t *testing.T) {
	opts := testHome(t)
	src, _ := sql.Open("sqlite", filepath.Join(opts.Home, "src.db"))
	src.Exec("CREATE TABLE typed (n TEXT, d TEXT, z TEXT, f TEXT)")
	src.Exec("INSERT INTO typed VALUES ('1','2026-01-02','007','1.5'),('22','2026-02-03','010','2')")
	src.Close()
	e, _ := newExplorer(opts, "SRC")
	defer e.h.close()
	run(e, e.start())
	for i, tb := range e.tables {
		if tb == "typed" {
			e.cur = i
		}
	}
	press1(e, keyEnter)
	if e.table != "typed" {
		t.Fatalf("table: %s", e.table)
	}
	e.update(keyRunes("t"))
	typeInto(e, "detect")
	e.update(keyEnter)
	if e.page != pageStep || e.stepEd.op != "cast" || e.stepEd.stage != 1 {
		t.Fatalf("detect opens the cast: %d", e.page)
	}
	press1(e, keySave)
	if len(e.pipeline) != 1 || e.pipeline[0].String() != "cast n as int, d as date, f as float" {
		t.Fatalf("detected: %+v", e.pipeline)
	}
}

func TestCellsEscapeControls(t *testing.T) {
	evil := "a\x1b]52;c;eA==\x07b\x1b[8mc\u0085"
	for _, s := range []string{cell(evil, db.String), cellFull(evil, db.String), txt.Clean(evil)} {
		if strings.ContainsAny(s, "\x1b\x07\u0085") {
			t.Errorf("control characters left: %q", s)
		}
	}
	if cellFull("a\nb", db.String) != "a\nb" || cell("a\nb", db.String) != "a↵b" {
		t.Error("line breaks")
	}
	long := strings.Repeat("é", 10000)
	if n := len([]rune(cell(long, db.String))); n != cellMax {
		t.Errorf("cut: %d", n)
	}
	e := openThings(t, testHome(t))
	e.rows[0][1] = strings.Repeat("ü", 300)
	e.colCur = 1
	e.showVal = true
	for _, l := range e.view(60, 10) {
		if !strings.Contains(l, "\ufffd") {
			continue
		}
		t.Fatalf("value box split a character: %q", l)
	}
}

func TestEnterOnUnloadedStreamPreviews(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "enter", "enter")
	if e, ok := m.wiz.(*explorer); !ok || e.readOnly || e.table != "things" {
		t.Fatalf("enter on an unloaded stream should preview the source: %+v", m.wiz)
	}
}

func TestLeftStaysInPreview(t *testing.T) {
	opts := testHome(t)
	e := openThings(t, opts)
	e.update(keyRight)
	if res, _ := e.update(tea.KeyMsg{Type: tea.KeyLeft}); res != wizContinue || e.colCur != 0 {
		t.Fatal("left moves one column first")
	}
	if res, _ := e.update(tea.KeyMsg{Type: tea.KeyLeft}); res != wizContinue || e.page != pagePreview || e.colCur != 0 {
		t.Fatalf("left on the first column stays: page %d col %d", e.page, e.colCur)
	}
	if res, _ := e.update(tea.KeyMsg{Type: tea.KeyEsc}); res != wizContinue || e.page != pageTables {
		t.Fatalf("esc goes back to tables: %d", e.page)
	}
	if res, _ := e.update(tea.KeyMsg{Type: tea.KeyLeft}); res != wizCancel {
		t.Fatal("left on the table list leaves the explorer")
	}
}

func TestAddStreamFormUsesPickers(t *testing.T) {
	opts := testHome(t)
	e := openThings(t, opts)
	e.update(keyRight)
	e.update(keySpace) // untick v
	e.update(keyRunes("+"))
	e.repPick.set("name", "picked")
	e.repPick.set("target", "TGT")
	e.update(keySave)
	f := e.stream
	if f.field("primary_key").Kind != kindMulti || f.field("update_key").Kind != kindSelect {
		t.Fatal("field kinds")
	}
	// sql, columns and steps are the preview's: not on the form
	for _, k := range []string{"sql", "select", "steps"} {
		if f.field(k) != nil {
			t.Fatalf("%s on the form", k)
		}
	}
	for _, k := range []string{"where", "object"} {
		if f.field(k) == nil {
			t.Fatalf("no %s", k)
		}
	}
	if got := strings.Join(f.field("primary_key").Options, ","); got != "id" {
		t.Fatalf("keys from the loaded columns: %s", got)
	}
	if got := strings.Join(f.picked("primary_key"), ","); got != "id" {
		t.Fatalf("primary key from the source: %s", got)
	}
	f.field("primary_key").Picked["id"] = true
	f.set("update_key", "v")
	f.set("mode", "incremental")
	f.set("limit", "10")
	if e.update(keySave); e.page != pagePreview {
		t.Fatalf("save: %q", f.err)
	}
	s := streamOf(readRep(t, opts, "picked"), "things")
	if strings.Join(s.PrimaryKey, ",") != "id" || s.UpdateKey != "v" || strings.Join(s.Select, ",") != "id" || s.Limit != 10 {
		t.Fatalf("written: %+v", s)
	}
	// update keys are taken whole: "none" is only blank on its own
	e2 := openThings(t, opts)
	e2.update(keyRunes("+"))
	e2.repPick.set("rep", "picked")
	e2.update(keySave)
	e2.stream.set("name", "things2")
	e2.stream.field("update_key").Options = append(e2.stream.field("update_key").Options, "nonempty_at")
	e2.stream.set("update_key", "nonempty_at")
	e2.update(keySave)
	if s := streamOf(readRep(t, opts, "picked"), "things2"); s.UpdateKey != "nonempty_at" {
		t.Fatalf("update key: %q", s.UpdateKey)
	}
}
