package steps

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"tummy/internal/db"
)

func dialect(t *testing.T, name string) db.Dialect {
	t.Helper()
	d, err := db.DialectFor(name)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseAndRender(t *testing.T) {
	p := "keep id, name | filter total > 10 and note = 'a|b' | rename name = customer | sort -updated_at | limit 5 | distinct"
	st, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 6 || st[1].Args != "total > 10 and note = 'a|b'" || st[5].Op != "distinct" {
		t.Fatalf("%+v", st)
	}
	if Render(st) != p {
		t.Fatalf("render: %q", Render(st))
	}
	old, _ := Parse("drop a | where b > 1")
	if Render(old) != "remove a | filter b > 1" {
		t.Fatalf("aliases: %q", Render(old))
	}
	for _, bad := range []string{"explode x", "limit 0", "limit x", "rename a", "cast a", "cast a as blob", "distinct now", "keep", "set a"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestCompilePostgres(t *testing.T) {
	pg := dialect(t, "postgres")
	cols := []string{"id", "name", "total", "updated_at", "note"}
	cases := map[string]string{
		"":                                      `SELECT * FROM "sales"."orders"`,
		"keep id, name":                         `SELECT "id", "name" FROM (SELECT * FROM "sales"."orders") AS _t1`,
		"remove note, total":                    `SELECT "id", "name", "updated_at" FROM (SELECT * FROM "sales"."orders") AS _t1`,
		"rename name = customer":                `SELECT "id", "name" AS "customer", "total", "updated_at", "note" FROM (SELECT * FROM "sales"."orders") AS _t1`,
		"filter total > 10":                     `SELECT * FROM (SELECT * FROM "sales"."orders") AS _t1 WHERE total > 10`,
		"sort -updated_at, id | limit 5":        `SELECT * FROM (SELECT * FROM "sales"."orders") AS _t ORDER BY "updated_at" DESC, "id" LIMIT 5`,
		"limit 5":                               `SELECT * FROM "sales"."orders" LIMIT 5`,
		"keep name | distinct":                  `SELECT DISTINCT * FROM (SELECT "name" FROM (SELECT * FROM "sales"."orders") AS _t1) AS _t2`,
		"trim name, note | upper name":          `SELECT "id", TRIM("name") AS "name", "total", "updated_at", TRIM("note") AS "note" FROM (SELECT * FROM "sales"."orders") AS _t1`,
		"cast total as int, updated_at as date": `SELECT "id", "name", CAST("total" AS bigint) AS "total", CAST("updated_at" AS date) AS "updated_at", "note" FROM (SELECT * FROM "sales"."orders") AS _t1`,
		"set spent = total * 2, note = 'x'":     `SELECT "id", "name", "total", "updated_at", 'x' AS "note", total * 2 AS "spent" FROM (SELECT * FROM "sales"."orders") AS _t1`,
		"coalesce note = ''":                    `SELECT "id", "name", "total", "updated_at", COALESCE("note", '') AS "note" FROM (SELECT * FROM "sales"."orders") AS _t1`,
	}
	for pipeline, want := range cases {
		st, err := Parse(pipeline)
		if err != nil {
			t.Fatalf("%s: %v", pipeline, err)
		}
		got, err := Compile("sales.orders", false, cols, st, pg)
		if err != nil {
			t.Fatalf("%s: %v", pipeline, err)
		}
		if pipeline == "trim name, note | upper name" {
			// second step layers on the first
			if !strings.HasPrefix(got, `SELECT "id", UPPER("name") AS "name", "total", "updated_at", "note" FROM (`) || !strings.Contains(got, `TRIM("name")`) {
				t.Errorf("%s:\n%s", pipeline, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%s:\ngot  %s\nwant %s", pipeline, got, want)
		}
	}
	// rename then keep uses the new name; keep of an unknown column fails
	st, _ := Parse("rename name = customer | keep id, customer")
	got, err := Compile("t", false, cols, st, pg)
	if err != nil || !strings.HasPrefix(got, `SELECT "id", "customer" FROM (SELECT "id", "name" AS "customer"`) {
		t.Fatalf("rename then keep: %v %s", err, got)
	}
	st, _ = Parse("keep nope")
	if _, err := Compile("t", false, cols, st, pg); err == nil || !strings.Contains(err.Error(), "unknown column") {
		t.Fatalf("unknown column: %v", err)
	}
	// sql base
	st, _ = Parse("filter n > 1")
	got, _ = Compile("select 1 as n;", true, []string{"n"}, st, pg)
	if got != "SELECT * FROM (SELECT * FROM (select 1 as n) AS _t0) AS _t1 WHERE n > 1" {
		t.Fatalf("sql base: %s", got)
	}
	got, _ = Compile(`SELECT * FROM "sales"."orders"`, true, cols, st, pg)
	if got != `SELECT * FROM (SELECT * FROM "sales"."orders") AS _t1 WHERE n > 1` {
		t.Fatalf("bare scan base: %s", got)
	}
}

func TestCompileOtherDialects(t *testing.T) {
	cols := []string{"id", "name"}
	st, _ := Parse("keep id, name | sort -id | limit 3")
	ms, _ := Compile("dbo.t", false, cols, st, dialect(t, "mssql"))
	if ms != `SELECT TOP 3 * FROM (SELECT [id], [name] FROM (SELECT * FROM [dbo].[t]) AS _t1) AS _t ORDER BY [id] DESC` {
		t.Fatalf("mssql: %s", ms)
	}
	st, _ = Parse("sort id")
	ms, _ = Compile("t", false, cols, st, dialect(t, "mssql"))
	if !strings.HasSuffix(ms, "ORDER BY [id] OFFSET 0 ROWS") {
		t.Fatalf("mssql order without limit: %s", ms)
	}
	my, _ := Compile("t", false, cols, st, dialect(t, "mysql"))
	if my != "SELECT * FROM (SELECT * FROM `t`) AS _t ORDER BY `id`" {
		t.Fatalf("mysql: %s", my)
	}
	st, _ = Parse("cast id as text")
	sl, _ := Compile("t", false, cols, st, dialect(t, "sqlite"))
	if !strings.Contains(sl, `CAST("id" AS TEXT) AS "id"`) {
		t.Fatalf("sqlite cast: %s", sl)
	}
}

func TestCompiledSQLRuns(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.Exec("CREATE TABLE orders (id INTEGER, name TEXT, total REAL, note TEXT)")
	d.Exec("INSERT INTO orders VALUES (1,' ann ',10,NULL),(2,'bob',25,'x'),(3,'cy',40,NULL),(4,'bob',5,'y')")
	st, err := Parse("where total >= 10 | trim name | rename name = who | coalesce note = '-' | set big = total * 10 | drop total | sort -id | limit 2") // old names still parse
	if err != nil {
		t.Fatal(err)
	}
	q, err := Compile("orders", false, []string{"id", "name", "total", "note"}, st, dialect(t, "sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := d.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
	defer rows.Close()
	names, _ := rows.Columns()
	if strings.Join(names, ",") != "id,who,note,big" {
		t.Fatalf("columns: %v\n%s", names, q)
	}
	var got []string
	for rows.Next() {
		var id int
		var who, note string
		var big float64
		rows.Scan(&id, &who, &note, &big)
		got = append(got, who+"/"+note)
	}
	if strings.Join(got, " ") != "cy/- bob/x" {
		t.Fatalf("rows: %v\n%s", got, q)
	}
}

func TestStepOrder(t *testing.T) {
	pg := dialect(t, "postgres")
	cols := []string{"id", "name", "total"}
	cases := map[string]string{
		"sort name | rename name = customer": `SELECT "id", "name" AS "customer", "total" FROM (SELECT * FROM (SELECT * FROM "t") AS _t ORDER BY "name") AS _t1`,
		"limit 10 | filter total > 1":        `SELECT * FROM (SELECT * FROM "t" LIMIT 10) AS _t1 WHERE total > 1`,
		"limit 10 | limit 100":               `SELECT * FROM (SELECT * FROM "t" LIMIT 10) AS _t LIMIT 100`,
		"limit 5 | sort id":                  `SELECT * FROM (SELECT * FROM "t" LIMIT 5) AS _t ORDER BY "id"`,
		"sort -id | remove id":               `SELECT "name", "total" FROM (SELECT * FROM (SELECT * FROM "t") AS _t ORDER BY "id" DESC) AS _t1`,
		"distinct | sort id | limit 3":       `SELECT * FROM (SELECT DISTINCT * FROM (SELECT * FROM "t") AS _t1) AS _t ORDER BY "id" LIMIT 3`,
	}
	for p, want := range cases {
		st, err := Parse(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		got, err := Compile("t", false, cols, st, pg)
		if err != nil || got != want {
			t.Errorf("%s:\ngot  %s %v\nwant %s", p, got, err, want)
		}
	}
}

func TestStepOrderRuns(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{"CREATE TABLE t (id INTEGER, name TEXT, total REAL)", "INSERT INTO t VALUES (1,'a',5),(2,'b',50),(3,'c',500),(4,'d',5000)"} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]int{
		"sort name | rename name = customer": 4,
		"limit 2 | filter total > 10":        1,
		"limit 3 | limit 100":                3,
		"sort -id | remove id":               4,
	}
	for p, want := range cases {
		st, _ := Parse(p)
		q, err := Compile("t", false, []string{"id", "name", "total"}, st, dialect(t, "sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := d.QueryRow("SELECT count(*) FROM (" + q + ")").Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows %v\n%s", p, n, err, q)
		}
	}
}

func TestCastTypes(t *testing.T) {
	cols := []string{"id", "name", "flag"}
	st, _ := Parse("cast id as int, name as text, flag as bool")
	my, err := Compile("t", false, cols, st, dialect(t, "mysql"))
	if err != nil || !strings.Contains(my, "CAST(`id` AS SIGNED) AS `id`, CAST(`name` AS CHAR) AS `name`, CAST(`flag` AS SIGNED) AS `flag`") {
		t.Fatalf("mysql: %s %v", my, err)
	}
	ms, _ := Compile("t", false, cols, st, dialect(t, "mssql"))
	if !strings.Contains(ms, "CAST([id] AS bigint) AS [id], CAST([name] AS nvarchar(max)) AS [name], CAST([flag] AS bit) AS [flag]") {
		t.Fatalf("mssql: %s", ms)
	}
}

func TestColumnCase(t *testing.T) {
	pg := dialect(t, "postgres")
	cols := []string{"id", "name"}
	for p, want := range map[string]string{
		"trim Name":            `TRIM("name") AS "name"`,
		"upper NAME":           `UPPER("name") AS "name"`,
		"cast ID as text":      `CAST("id" AS text) AS "id"`,
		"coalesce Name = 'x'":  `COALESCE("name", 'x') AS "name"`,
		"sort -Name":           `ORDER BY "name" DESC`,
		"rename NAME = who":    `"name" AS "who"`,
		"set Name = upper(id)": `upper(id) AS "name"`,
	} {
		st, err := Parse(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Compile("t", false, cols, st, pg)
		if err != nil || !strings.Contains(got, want) {
			t.Errorf("%s: %s %v", p, got, err)
		}
	}
}

func TestConcatParses(t *testing.T) {
	st, err := Parse("set full = a || ' ' || b | keep full")
	if err != nil || len(st) != 2 || st[0].Args != "full = a || ' ' || b" {
		t.Fatalf("%+v %v", st, err)
	}
	got, err := Compile("t", false, []string{"a", "b"}, st, dialect(t, "postgres"))
	if err != nil || !strings.Contains(got, `a || ' ' || b AS "full"`) {
		t.Fatalf("%s %v", got, err)
	}
}

func TestValidation(t *testing.T) {
	for _, bad := range []string{"rename x =", "rename = y", "keep id, id", "keep id, ID", "set = 1", "set a = 1, a = 2", "coalesce a =", "cast as int"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	cols := []string{"id", "name"}
	for _, bad := range []string{"rename id = name", "rename id = NAME"} {
		st, err := Parse(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Compile("t", false, cols, st, dialect(t, "postgres")); err == nil || !strings.Contains(err.Error(), "exists") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	st, _ := Parse("rename id = name, name = id")
	if _, err := Compile("t", false, cols, st, dialect(t, "postgres")); err != nil {
		t.Errorf("swap: %v", err)
	}
}

func TestCompileWithColumns(t *testing.T) {
	st, _ := Parse("rename name = customer | set spent = total * 2 | remove total | keep customer, spent, id")
	c, err := CompileWithColumns("t", false, []string{"id", "name", "total"}, st, dialect(t, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"id,name,total", "id,customer,total", "id,customer,total,spent", "id,customer,spent", "customer,spent,id"}
	if len(c.Columns) != len(want) {
		t.Fatalf("%v", c.Columns)
	}
	for i, w := range want {
		if strings.Join(c.Columns[i], ",") != w {
			t.Errorf("step %d: %v, want %s", i, c.Columns[i], w)
		}
	}
	if o, ok := c.Origin("customer"); !ok || o != "name" {
		t.Errorf("origin customer: %q %v", o, ok)
	}
	if o, ok := c.Origin("ID"); !ok || o != "id" {
		t.Errorf("origin id: %q %v", o, ok)
	}
	if _, ok := c.Origin("spent"); ok {
		t.Error("spent has no origin")
	}
	st, _ = Parse("trim name | keep nope | keep id")
	c, err = CompileWithColumns("t", false, []string{"id", "name"}, st, dialect(t, "postgres"))
	if err == nil || len(c.Columns) != 2 || strings.Join(c.Columns[1], ",") != "id,name" {
		t.Fatalf("partial: %v %v", c.Columns, err)
	}
	if _, ok := c.Origin("name"); ok {
		t.Error("origin after error")
	}
	st, _ = Parse("upper name")
	c, _ = CompileWithColumns("t", false, []string{"id", "name"}, st, dialect(t, "postgres"))
	if _, ok := c.Origin("name"); ok {
		t.Error("upper keeps no origin")
	}
}

func TestCustomSQLWrapped(t *testing.T) {
	st, _ := Parse("filter n > 1")
	ms, _ := Compile("select n from t order by n", true, []string{"n"}, st, dialect(t, "mssql"))
	if ms != "SELECT * FROM (SELECT * FROM (select n from t order by n OFFSET 0 ROWS) AS _t0) AS _t1 WHERE n > 1" {
		t.Fatalf("order by: %s", ms)
	}
	cte, _ := Compile("with x as (select 1 as n) select n from x", true, []string{"n"}, st, dialect(t, "mssql"))
	if cte != "with x as (select 1 as n) SELECT * FROM (SELECT * FROM (select n from x) AS _t0) AS _t1 WHERE n > 1" {
		t.Fatalf("cte: %s", cte)
	}
	cmt, _ := Compile("select n from t -- note", true, []string{"n"}, st, dialect(t, "postgres"))
	if cmt != "SELECT * FROM (SELECT * FROM (select n from t -- note\n) AS _t0) AS _t1 WHERE n > 1" {
		t.Fatalf("comment: %q", cmt)
	}
}

func TestOpsAreCopies(t *testing.T) {
	Ops[0] = "explode"
	Examples["keep"] = "x"
	defer func() { Ops[0] = "keep"; Examples["keep"] = registry[0].example }()
	if _, err := Parse("keep id"); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("explode id"); err == nil {
		t.Fatal("explode parsed")
	}
}
