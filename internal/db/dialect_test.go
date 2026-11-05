package db

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

// TestDialectGoldenSQL renders every statement tummy issues, per dialect, into testdata/<dialect>.sql.
func TestDialectGoldenSQL(t *testing.T) {
	for _, name := range Dialects() {
		t.Run(name, func(t *testing.T) {
			dl, _ := DialectFor(name)
			got := render(dl)
			path := filepath.Join("testdata", name+".sql")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s differs; go test ./internal/db -update, then review the diff\n%s", path, got)
			}
		})
	}
}

func render(dl Dialect) string {
	tgt := Table{Schema: "s", Name: "orders"}
	tmp := Table{Schema: "s", Name: "_tummy_tmp_orders"}
	old := Table{Schema: "s", Name: "_tummy_old_orders"}
	if !dl.HasSchemas() {
		tgt.Schema, tmp.Schema, old.Schema = "", "", ""
	}
	cols := []Column{
		{Name: "id", Kind: Int}, {Name: "code", Kind: String}, {Name: "note", Kind: String},
		{Name: "total", Kind: Decimal, Precision: 10, Scale: 2}, {Name: "rate", Kind: Decimal},
		{Name: "ok", Kind: Bool}, {Name: "at", Kind: Timestamp}, {Name: "day", Kind: Date},
		{Name: "meta", Kind: JSON}, {Name: "blob", Kind: Bytes}, {Name: "f", Kind: Float},
	}
	keys := []string{"id", "code"}
	var b strings.Builder
	line := func(label, sql string) { fmt.Fprintf(&b, "-- %s\n%s\n\n", label, sql) }
	line("create schema", dl.CreateSchema("out"))
	line("create", CreateTableSQL(dl, tmp, cols, keys))
	line("insert 2 rows", InsertSQL(dl, tmp, cols[:3], 2))
	line("insert select", InsertSelectSQL(dl, tgt, tmp, cols[:3]))
	line("alter add", AddColumnSQL(dl, tgt, cols[3]))
	line("drop column", DropColumnSQL(dl, tmp, "_tummy_seq"))
	line("drop table", DropTableSQL(dl, tmp))
	line("create index", CreateIndexSQL(dl, "_tummy_ix_orders", tgt, keys))
	line("max", MaxSQL(dl, tgt, "at"))
	line("duplicates", DuplicatesSQL(dl, tmp, keys))
	line("rename", dl.Rename(tmp, tgt))
	line("truncate", dl.Truncate(tgt))
	line("delete matching", dl.DeleteMatching(tgt, tmp, keys))
	line("dedupe by update key", dl.Dedupe(tmp, keys, "at", "_tummy_seq"))
	line("dedupe by load order", dl.Dedupe(tmp, []string{"id"}, "", "_tummy_seq"))
	for _, exists := range []bool{true, false} {
		stmts, tx := dl.Swap(tmp, tgt, old, exists)
		line(fmt.Sprintf("swap exists=%v tx=%v", exists, tx), strings.Join(stmts, ";\n"))
	}
	q, args := dl.TableExists(tgt, "")
	line(fmt.Sprintf("table exists %v", args), q)
	q, args = dl.ListTables("", "")
	line(fmt.Sprintf("list tables %v", args), q)
	base := "SELECT * FROM " + QuoteTable(dl, tgt)
	line("limit", dl.OrderLimit(base, "", 5))
	line("order", dl.OrderLimit(base, dl.Quote("id")+" DESC", 0))
	line("order limit", dl.OrderLimit(base, dl.Quote("id"), 5))
	line("limit over limit", dl.OrderLimit("select id from t limit 10", "", 5))
	line("limit over order by", dl.OrderLimit("select id from t order by id", "", 5))
	line("wrap cte", Wrap(dl, "WITH x AS (SELECT 1 AS n) SELECT n FROM x ORDER BY n", "*", "_w")+" WHERE n > 0")
	line("wrap comment", Wrap(dl, "select n from t -- trailing", "*", "_w"))
	var casts []string
	for k := String; k <= Bytes; k++ {
		casts = append(casts, k.String()+"="+dl.CastType(k))
	}
	line("cast types", strings.Join(casts, " "))
	var types []string
	for _, c := range cols {
		types = append(types, c.Name+"="+dl.TypeName(c, false))
	}
	line("column types", strings.Join(types, " ")+"\nkey string="+dl.TypeName(Column{Kind: String}, true))
	for i := 1; i <= 2; i++ {
		b.WriteString(dl.Placeholder(i) + " ")
	}
	b.WriteString("\n")
	return b.String()
}

func TestSplitWith(t *testing.T) {
	cases := []struct{ in, with, body string }{
		{"select 1", "", "select 1"},
		{"WITH a AS (select 1), b(x) AS (select 2) select * from a, b", "WITH a AS (select 1), b(x) AS (select 2)", "select * from a, b"},
		{" with recursive r(n) as (select 1 union all select n+1 from r where n < 3) select n from r", "with recursive r(n) as (select 1 union all select n+1 from r where n < 3)", "select n from r"},
		{`WITH "select" AS (select 'select') SELECT * FROM "select"`, `WITH "select" AS (select 'select')`, `SELECT * FROM "select"`},
		{"withdrawals", "", "withdrawals"},
	}
	for _, c := range cases {
		w, b := SplitWith(c.in)
		if w != c.with || b != c.body {
			t.Errorf("%q: %q | %q", c.in, w, b)
		}
	}
}

func TestDerivedMSSQL(t *testing.T) {
	dl, _ := DialectFor("mssql")
	for in, want := range map[string]string{
		"select a from t order by a":                    "select a from t order by a OFFSET 0 ROWS",
		"select top 5 a from t order by a":              "select top 5 a from t order by a",
		"select a from t order by a offset 2 rows":      "select a from t order by a offset 2 rows",
		"select a from (select b from u order by b) x":  "select a from (select b from u order by b) x",
		"select 'order by' as a from t":                 "select 'order by' as a from t",
		"select a from t order by a -- newest\n":        "select a from t order by a -- newest\n OFFSET 0 ROWS",
		"select a from t order by a -- newest":          "select a from t order by a -- newest\n OFFSET 0 ROWS",
		"select distinct top 1 a from t order by a asc": "select distinct top 1 a from t order by a asc",
	} {
		if got := dl.Derived(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestInserterStripsNULForRejectingTargets(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, Params{Name: "L", Type: "sqlite", Path: filepath.Join(t.TempDir(), "x.db"), Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.Dialect = nulRejecting{d.Dialect}
	if err := d.Exec(ctx, "CREATE TABLE t (id INTEGER, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	ins := d.NewInserter(Table{Name: "t"}, []Column{{Name: "id", Kind: Int}, {Name: "v", Kind: String}})
	if err := ins.Add(ctx, []any{1, "a\x00b"}); err != nil {
		t.Fatal(err)
	}
	if err := ins.Add(ctx, []any{2, "plain"}); err != nil {
		t.Fatal(err)
	}
	if err := ins.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := d.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil || v != "ab" || ins.Stripped != 1 {
		t.Fatalf("v=%q stripped=%d err=%v", v, ins.Stripped, err)
	}
}

// nulRejecting makes a dialect behave like postgres for NUL bytes.
type nulRejecting struct{ Dialect }

func (nulRejecting) RejectsNUL() bool { return true }
