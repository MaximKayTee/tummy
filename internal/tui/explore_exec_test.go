package tui

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func execOnce(t *testing.T, e *explorer, stmt string) execMsg {
	t.Helper()
	e.sqlInput.SetValue(stmt)
	_, cmd := e.update(keyCtrlE)
	if cmd == nil || e.busy != "executing" {
		t.Fatalf("no execute for %q", stmt)
	}
	msg, ok := cmd().(execMsg)
	if !ok {
		t.Fatalf("not an execMsg: %T", cmd())
	}
	e.update(msg)
	return msg
}

func TestExecuteStatement(t *testing.T) {
	opts := testHome(t)
	e := openThings(t, opts)
	e.update(keyRunes("S"))
	if !e.editSQL {
		t.Fatal("editor")
	}
	if f, _, _ := e.previewKeys(); !strings.Contains(strings.Join(f, " "), "ctrl+e") {
		t.Fatalf("footer: %v", f)
	}
	execOnce(t, e, "create table t2 (a int)")
	if !e.editSQL || e.currentNote() != "0 rows affected  "+strings.TrimPrefix(e.currentNote(), "0 rows affected  ") || e.busy != "" {
		t.Fatalf("note=%q editSQL=%v", e.currentNote(), e.editSQL)
	}
	execOnce(t, e, "insert into t2 values (1),(2)")
	if !strings.HasPrefix(e.currentNote(), "2 rows affected") {
		t.Fatalf("note: %q", e.currentNote())
	}
	execOnce(t, e, "garbage here")
	if e.fetchErr == "" || !strings.Contains(e.fetchErr, "garbage here") {
		t.Fatalf("error shown in full: %q", e.fetchErr)
	}
	if v := strings.Join(e.view(100, 24), "\n"); !strings.Contains(v, "garbage") {
		t.Fatalf("view:\n%s", v)
	}
	db, _ := sql.Open("sqlite", filepath.Join(opts.Home, "src.db"))
	defer db.Close()
	var n int
	db.QueryRow("select count(*) from t2").Scan(&n)
	if n != 2 {
		t.Fatalf("t2 rows: %d", n)
	}

	// read only: refused before anything runs
	e.conn.ReadOnly = true
	e.sqlInput.SetValue("drop table t2")
	if _, cmd := e.update(keyCtrlE); cmd != nil || e.currentNote() != "SRC: read only" {
		t.Fatalf("read only: cmd=%v note=%q", cmd, e.currentNote())
	}
	if f, _, _ := e.previewKeys(); strings.Contains(strings.Join(f, " "), "ctrl+e") {
		t.Fatalf("footer offers execute on read only: %v", f)
	}
}

func TestDropTableFromList(t *testing.T) {
	opts := testHome(t)
	e, err := newExplorer(opts, "SRC")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.h.close() })
	run(e, e.start())
	e.cur = 0
	e.update(keyRunes("D"))
	if e.page != pageDrop || e.dropTable != "things" {
		t.Fatalf("page=%d table=%q", e.page, e.dropTable)
	}
	// the name must match
	typeInto(e, "thing")
	e.update(keyEnter)
	if e.page != pageDrop || e.dropForm.field("name").err != "type things" {
		t.Fatalf("mismatch accepted: page=%d err=%q", e.page, e.dropForm.field("name").err)
	}
	e.update(keyEsc)
	if e.page != pageTables || e.busy != "" {
		t.Fatal("esc backs out")
	}
	e.update(keyRunes("D"))
	e.dropForm.set("name", "things")
	_, cmd := e.update(keyEnter)
	if e.page != pageTables || cmd == nil || e.statsBusy != "dropping things" {
		t.Fatalf("drop: page=%d busy=%q", e.page, e.statsBusy)
	}
	msg := cmd().(execMsg)
	_, cmd = e.update(msg)
	if msg.err != nil || e.statsNote != "dropped things" || e.statsBusy != "" || cmd == nil {
		t.Fatalf("after drop: err=%v note=%q", msg.err, e.statsNote)
	}
	run(e, cmd) // the list again
	if len(e.tables) != 0 {
		t.Fatalf("tables: %v", e.tables)
	}
	db, _ := sql.Open("sqlite", filepath.Join(opts.Home, "src.db"))
	defer db.Close()
	if _, err := db.Exec("select 1 from things"); err == nil {
		t.Fatal("things still there")
	}
}

func TestDropTableFromPreview(t *testing.T) {
	opts := testHome(t)
	e := openThings(t, opts)
	e.update(keyRunes(":"))
	if !strings.Contains(strings.Join(e.palette.items, "\n"), "drop table things") {
		t.Fatalf("palette: %v", e.palette.items)
	}
	e.update(keyEsc)
	e.runCommand("drop")
	if e.page != pageDrop || e.dropFrom != pagePreview {
		t.Fatalf("page=%d from=%d", e.page, e.dropFrom)
	}
	e.dropForm.set("name", "things")
	_, cmd := e.update(keyEnter)
	if e.page != pagePreview {
		t.Fatalf("page=%d", e.page)
	}
	if v := strings.Join(e.view(100, 24), "\n"); !strings.Contains(v, "◐ dropping things") {
		t.Fatalf("head:\n%s", v)
	}
	_, cmd = e.update(cmd())
	if e.page != pageTables || e.table != "" || cmd == nil {
		t.Fatalf("after drop: page=%d table=%q", e.page, e.table)
	}
	// a read-only connection never offers it
	e.conn.ReadOnly = true
	if cmd := e.askDrop("x"); cmd != nil || e.currentNote() != "SRC: read only" {
		t.Fatalf("read only: %v %q", cmd, e.currentNote())
	}
}
