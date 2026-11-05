package tui

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExplorerStats(t *testing.T) {
	opts := testHome(t)
	src, err := sql.Open("sqlite", filepath.Join(opts.Home, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	src.Exec("CREATE TABLE big (id INTEGER)")
	src.Exec("INSERT INTO big VALUES (1),(2),(3),(4),(5),(6),(7),(8),(9),(10)")

	e, err := newExplorer(opts, "SRC")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.h.close() })
	run(e, e.start())
	step := func(key string) {
		t.Helper()
		_, cmd := e.update(keyRunes(key))
		if cmd == nil {
			t.Fatalf("%s: no command", key)
		}
		e.update(cmd())
		if e.statsErr {
			t.Fatalf("%s: %s", key, e.statsNote)
		}
	}
	if v := strings.Join(e.view(100, 20), "\n"); strings.Contains(v, "ROWS") {
		t.Fatalf("stats shown before any refresh:\n%s", v)
	}
	step("s")
	if s := e.stats["things"]; s.Rows != 3 || !s.Exact || s.Bytes <= 0 {
		t.Fatalf("things after s: %+v", s)
	}
	v := strings.Join(e.view(100, 20), "\n")
	if !strings.Contains(v, "ROWS") || !strings.Contains(v, "2 tables refreshed") {
		t.Fatalf("view after s:\n%s", v)
	}

	e.update(keyRunes("o")) // by rows: big first
	if vis := e.visibleTables(); vis[0] != "big" || e.sortBy != sortRows {
		t.Fatalf("sorted by rows: %v", vis)
	}
	e.update(keyRunes("o"))
	e.update(keyRunes("o"))
	if vis := e.visibleTables(); vis[0] != e.tables[0] || e.sortBy != sortName {
		t.Fatalf("back to names: %v", vis)
	}

	src.Exec("INSERT INTO things VALUES (4,'d'),(5,'e')")
	e.cur = slices.Index(e.visibleTables(), "things")
	step("c")
	s := e.stats["things"]
	if s.Rows != 5 || s.PrevRows != 3 || !s.Exact {
		t.Fatalf("things after c: %+v", s)
	}
	if v := strings.Join(e.view(100, 20), "\n"); !strings.Contains(v, "+2") || !strings.Contains(v, "things: 5 rows") {
		t.Fatalf("view after c:\n%s", v)
	}

	cs, err := opts.State.ConnStats(context.Background())
	if err != nil || cs["SRC"].Tables != 2 || cs["SRC"].Bytes <= 0 {
		t.Fatalf("conn stats: %+v %v", cs, err)
	}
	m := loaded(t, opts)
	m.view = viewConns
	if v := strings.Join(m.connsView(), "\n"); !strings.Contains(v, "SIZE") || !strings.Contains(v, "KiB") {
		t.Fatalf("connections:\n%s", v)
	}
}
