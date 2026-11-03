package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInserterNamesFailingRow(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, Params{Name: "L", Type: "sqlite", Path: filepath.Join(t.TempDir(), "x.db"), Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Exec(ctx, "CREATE TABLE t (n INTEGER CHECK (n < 5))"); err != nil {
		t.Fatal(err)
	}
	ins := d.NewInserter(Table{Name: "t"}, []Column{{Name: "n", Kind: Int}})
	ins.Label = "target"
	for i := 1; i <= 6; i++ {
		if err := ins.Add(ctx, []any{int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	err = ins.Flush(ctx)
	if err == nil || !strings.Contains(err.Error(), "insert into target: row 5") {
		t.Fatalf("%v", err)
	}
}

func TestSQLiteTypesRoundTrip(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, Params{Name: "L", Type: "sqlite", Path: filepath.Join(t.TempDir(), "x.db"), Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cols := []Column{{Name: "at", Kind: Timestamp}, {Name: "day", Kind: Date}, {Name: "ok", Kind: Bool}}
	if err := d.CreateTable(ctx, Table{Name: "t"}, cols, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.Exec(ctx, "INSERT INTO t VALUES ('2026-07-01 12:00:00+00:00', '2026-07-01', 1)"); err != nil {
		t.Fatal(err)
	}
	got, err := d.Columns(ctx, Table{Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range got {
		if c.Kind != cols[i].Kind {
			t.Errorf("%s: %s", c.Name, c.Kind)
		}
	}
	m, err := d.Max(ctx, Table{Name: "t"}, "ok")
	if err != nil || m != int64(1) {
		t.Fatalf("max: %v %v", m, err)
	}
}

func TestSQLiteNoCreate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.db")
	if _, err := Open(context.Background(), Params{Name: "L", Type: "sqlite", Path: p}); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing file opened: %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("file created")
	}
}

func TestHelpers(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, Params{Name: "L", Type: "sqlite", Path: filepath.Join(t.TempDir(), "x.db"), Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		"CREATE TABLE t (b TEXT, a INTEGER, v TEXT, PRIMARY KEY (a, b))",
		"INSERT INTO t VALUES ('x', 1, 'p'), ('y', 1, 'q'), ('x', 2, NULL), ('z', 3, 'p')",
		"CREATE TABLE nokey (n INTEGER)",
	} {
		if err := d.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if pk, err := d.PrimaryKey(ctx, Table{Name: "t"}); err != nil || strings.Join(pk, ",") != "a,b" {
		t.Fatalf("primary key: %v %v", pk, err)
	}
	if pk, err := d.PrimaryKey(ctx, Table{Name: "nokey"}); err != nil || len(pk) != 0 {
		t.Fatalf("no key: %v %v", pk, err)
	}
	if n, err := d.Count(ctx, `SELECT * FROM "t" WHERE a = ? ORDER BY b`, 1); err != nil || n != 2 {
		t.Fatalf("count: %v %v", n, err)
	}
	vals, err := d.Distinct(ctx, `SELECT * FROM "t"`, "v", 0)
	if err != nil || len(vals) != 3 || vals[0] != nil || vals[1] != "p" || vals[2] != "q" {
		t.Fatalf("distinct: %#v %v", vals, err)
	}
	if vals, err := d.Distinct(ctx, `SELECT * FROM "t" WHERE a > ?`, "b", 1, 1); err != nil || len(vals) != 1 || vals[0] != "x" {
		t.Fatalf("distinct limit: %#v %v", vals, err)
	}
	if s, err := d.ListSchemas(ctx); err != nil || s != nil {
		t.Fatalf("sqlite schemas: %v %v", s, err)
	}
	if ts, err := d.ListAllTables(ctx); err != nil || len(ts) != 2 {
		t.Fatalf("all tables: %v %v", ts, err)
	}
}

func TestPrimaryKeySQL(t *testing.T) {
	for _, typ := range Dialects() {
		dl, _ := DialectFor(typ)
		q, args := dl.PrimaryKey(Table{Schema: "s", Name: "t"}, "")
		if !strings.Contains(q, "SELECT") || len(args) == 0 || args[len(args)-1] != "t" {
			t.Errorf("%s: %s %v", typ, q, args)
		}
	}
	q, args := mysql{}.PrimaryKey(Table{Name: "t"}, "")
	if !strings.Contains(q, "DATABASE()") || len(args) != 1 {
		t.Errorf("mysql default schema: %s %v", q, args)
	}
}
