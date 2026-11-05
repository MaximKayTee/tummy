package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/state"
)

// These run only against a live Postgres: TUMMY_TEST_PG=postgres://... go test ./internal/engine/
// scripts/devpg.sh start prints a DSN for a local cluster.

type pgFixture struct {
	t      *testing.T
	dsn    string
	schema string
	pg     *sql.DB
	conns  *config.Connections
	st     *state.Store
	dir    string
}

func pgSetup(t *testing.T) *pgFixture {
	t.Helper()
	dsn := os.Getenv("TUMMY_TEST_PG")
	if dsn == "" {
		t.Skip("TUMMY_TEST_PG not set")
	}
	pg, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.Ping(); err != nil {
		t.Fatalf("postgres: %v", err)
	}
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano()%1_000_000_000)
	f := &pgFixture{t: t, dsn: dsn, schema: schema, pg: pg, dir: t.TempDir()}
	f.exec("CREATE SCHEMA " + schema)
	f.exec(fmt.Sprintf(`CREATE TABLE %s.orders (id serial primary key, customer text, total numeric(10,2), placed date, updated_at timestamp, meta jsonb, flag boolean)`, schema))
	f.exec(fmt.Sprintf(`INSERT INTO %s.orders (customer, total, placed, updated_at, meta, flag) VALUES
		('ann', 10.50, '2026-01-01', '2026-01-01 10:00', '{"a":1}', true),
		('bob', 20.00, '2026-01-02', '2026-01-02 10:00', null, false),
		('cy', 30.25, '2026-01-03', '2026-01-03 10:00', '{"b":[1,2]}', null)`, schema))
	f.exec(fmt.Sprintf(`CREATE TABLE %s.customers (id int primary key, name text)`, schema))
	f.exec(fmt.Sprintf(`INSERT INTO %s.customers VALUES (1,'Ann'),(2,'Bob')`, schema))
	f.exec(fmt.Sprintf(`CREATE VIEW %s.v_totals AS SELECT customer, sum(total) AS spent FROM %s.orders GROUP BY 1`, schema, schema))
	f.conns = &config.Connections{Conn: map[string]*config.Conn{
		"PG":   {Name: "PG", Type: "postgres", URL: dsn, Schema: schema + "_out"},
		"SRC":  {Name: "SRC", Type: "postgres", URL: dsn},
		"LITE": {Name: "LITE", Type: "sqlite", Path: filepath.Join(f.dir, "lite.db")},
	}}
	f.st, err = state.Open(filepath.Join(f.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.st.Close()
		f.exec("DROP SCHEMA " + schema + " CASCADE")
		f.exec("DROP SCHEMA IF EXISTS " + schema + "_out CASCADE")
		pg.Close()
	})
	return f
}

func (f *pgFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pg.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *pgFixture) count(q string) int {
	f.t.Helper()
	var n int
	if err := f.pg.QueryRow(q).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (f *pgFixture) engine() *Engine {
	return &Engine{Conns: f.conns, State: f.st, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
}

func (f *pgFixture) replication(body string) *config.Replication {
	f.t.Helper()
	body = strings.ReplaceAll(body, "{s}", f.schema)
	p := filepath.Join(f.dir, "rep.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	r, err := config.LoadReplication(p)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestPostgresToPostgres(t *testing.T) {
	f := pgSetup(t)
	ctx := context.Background()
	out := f.schema + "_out"
	r := f.replication(`
source = "SRC"
target = "PG"
mode = "incremental"
primary_key = ["id"]
exclude = ["{s}.v_*"]
object = "{target_schema}.{stream_table}"

[streams."{s}.*"]

[streams."{s}.orders"]
update_key = "updated_at"
`)
	e := f.engine()
	res, err := e.Run(ctx, r, nil)
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if len(res) != 2 {
		t.Fatalf("wildcard + exclude: %+v", res)
	}
	if f.count("SELECT count(*) FROM "+out+".orders") != 3 || f.count("SELECT count(*) FROM "+out+".customers") != 2 {
		t.Fatal("counts")
	}
	// types survived: numeric, date, timestamp, jsonb, boolean
	var typ string
	for col, want := range map[string]string{"total": "numeric", "placed": "date", "updated_at": "timestamp without time zone", "meta": "jsonb", "flag": "boolean"} {
		f.pg.QueryRow("SELECT data_type FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'orders' AND column_name = $2", out, col).Scan(&typ)
		if typ != want {
			t.Errorf("%s: %s, want %s", col, typ, want)
		}
	}
	// incremental: change one, add one, add a column
	f.exec(fmt.Sprintf("UPDATE %s.orders SET total = 99, updated_at = '2026-02-01 10:00' WHERE id = 2", f.schema))
	f.exec(fmt.Sprintf("INSERT INTO %s.orders (customer, total, placed, updated_at) VALUES ('dee', 40, '2026-01-04', '2026-01-04 10:00')", f.schema))
	f.exec(fmt.Sprintf("ALTER TABLE %s.orders ADD note text", f.schema))
	f.exec(fmt.Sprintf("UPDATE %s.orders SET note = 'late' WHERE id = 4", f.schema))
	res, err = e.Run(ctx, r, []string{f.schema + ".orders"})
	if err != nil || res[0].Rows != 3 { // 2 changed plus id 3 at the watermark
		t.Fatalf("incremental: %v %+v", err, res)
	}
	if f.count("SELECT count(*) FROM "+out+".orders") != 4 || f.count("SELECT total::int FROM "+out+".orders WHERE id = 2") != 99 {
		t.Fatal("upsert")
	}
	var note sql.NullString
	f.pg.QueryRow("SELECT note FROM " + out + ".orders WHERE id = 4").Scan(&note)
	if note.String != "late" {
		t.Fatalf("new column: %v", note)
	}
	// temp tables are gone
	if f.count("SELECT count(*) FROM information_schema.tables WHERE table_schema = '"+out+"' AND table_name LIKE '\\_tummy\\_tmp%'") != 0 {
		t.Fatal("temp table left")
	}
}

func TestPostgresModesAndSQL(t *testing.T) {
	f := pgSetup(t)
	ctx := context.Background()
	out := f.schema + "_out"
	e := f.engine()

	// truncate keeps an index
	tr := f.replication("source = \"SRC\"\ntarget = \"PG\"\nmode = \"truncate\"\nobject = \"{target_schema}.{stream_table}\"\n[streams.\"{s}.customers\"]\n")
	if _, err := e.Run(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	f.exec("CREATE INDEX cust_name_idx ON " + out + ".customers(name)")
	f.exec(fmt.Sprintf("DELETE FROM %s.customers WHERE id = 2", f.schema))
	if _, err := e.Run(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM "+out+".customers") != 1 || f.count("SELECT count(*) FROM pg_indexes WHERE schemaname = '"+out+"' AND indexname = 'cust_name_idx'") != 1 {
		t.Fatal("truncate should keep the index")
	}

	// snapshot of a view, twice
	snap := f.replication("source = \"SRC\"\ntarget = \"PG\"\nmode = \"snapshot\"\nobject = \"{target_schema}.totals_snap\"\n[streams.\"{s}.v_totals\"]\n")
	if _, err := e.Run(ctx, snap, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(ctx, snap, nil); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(DISTINCT _tummy_loaded_at) FROM "+out+".totals_snap") != 2 {
		t.Fatal("snapshot")
	}

	// backfill by date range, custom sql with placeholder, where + select
	bf := f.replication("source = \"SRC\"\ntarget = \"PG\"\nmode = \"backfill\"\nprimary_key = [\"id\"]\nupdate_key = \"placed\"\nrange = \"2026-01-02,2026-01-03\"\nobject = \"{target_schema}.orders_bf\"\n[streams.\"{s}.orders\"]\n")
	res, err := e.Run(ctx, bf, nil)
	if err != nil || res[0].Rows != 2 {
		t.Fatalf("backfill: %v %+v", err, res)
	}
	q := f.replication("source = \"SRC\"\ntarget = \"PG\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\nupdate_key = \"updated_at\"\n[streams.big]\nsql = \"select id, customer, updated_at from {s}.orders where {incremental_where_cond} and total > 15\"\nobject = \"{target_schema}.big\"\n")
	if _, err := e.Run(ctx, q, nil); err != nil {
		t.Fatal(err)
	}
	f.exec(fmt.Sprintf("INSERT INTO %s.orders (customer, total, placed, updated_at) VALUES ('zed', 100, '2026-03-01', '2026-03-01 10:00')", f.schema))
	res, err = e.Run(ctx, q, nil)
	if err != nil || res[0].Rows != 2 || f.count("SELECT count(*) FROM "+out+".big") != 3 {
		t.Fatalf("sql incremental: %v %+v", err, res)
	}
	sel := f.replication("source = \"SRC\"\ntarget = \"PG\"\nobject = \"{target_schema}.some_orders\"\n[streams.\"{s}.orders\"]\nselect = [\"id\", \"customer\"]\nwhere = \"total >= 20\"\n")
	if _, err := e.Run(ctx, sel, nil); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM "+out+".some_orders") != 3 || f.count("SELECT count(*) FROM information_schema.columns WHERE table_schema = '"+out+"' AND table_name = 'some_orders'") != 2 {
		t.Fatal("select/where")
	}
}

func TestPostgresToSQLiteAndBack(t *testing.T) {
	f := pgSetup(t)
	ctx := context.Background()
	e := f.engine()
	down := f.replication("source = \"SRC\"\ntarget = \"LITE\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\n[streams.\"{s}.orders\"]\nupdate_key = \"updated_at\"\n[streams.\"{s}.customers\"]\n")
	res, err := e.Run(ctx, down, nil)
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	lite, _ := sql.Open("sqlite", filepath.Join(f.dir, "lite.db"))
	defer lite.Close()
	var n int
	lite.QueryRow("SELECT count(*) FROM " + f.schema + "_orders").Scan(&n)
	if n != 3 {
		t.Fatalf("sqlite rows: %d", n)
	}
	var meta string
	lite.QueryRow("SELECT meta FROM " + f.schema + "_orders WHERE id = 1").Scan(&meta)
	if meta != `{"a": 1}` && meta != `{"a":1}` {
		t.Fatalf("jsonb as text: %q", meta)
	}
	var ts string
	lite.QueryRow("SELECT CAST(updated_at AS TEXT) FROM " + f.schema + "_orders WHERE id = 1").Scan(&ts)
	if ts != "2026-01-01 10:00:00+00:00" {
		t.Fatalf("timestamp stored as %q", ts)
	}
	// second incremental pass: the watermark read from sqlite must parse on the postgres side
	f.exec(fmt.Sprintf("INSERT INTO %s.orders (customer, total, placed, updated_at) VALUES ('new', 1, '2026-02-01', '2026-02-01 10:00')", f.schema))
	res, err = e.Run(ctx, down, []string{f.schema + ".orders"})
	if err != nil || res[0].Rows != 2 {
		t.Fatalf("second pass: %v %+v", err, res)
	}
	lite.QueryRow("SELECT count(*) FROM " + f.schema + "_orders").Scan(&n)
	if n != 4 {
		t.Fatalf("after second pass: %d", n)
	}
	// and up again into a new postgres schema, keys applied
	f.conns.Conn["LITE"].Schema = ""
	up := f.replication("source = \"LITE\"\ntarget = \"PG\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\nobject = \"{target_schema}.roundtrip\"\n[streams.\"{s}_orders\"]\nupdate_key = \"updated_at\"\n")
	if _, err := e.Run(ctx, up, nil); err != nil {
		t.Fatal(err)
	}
	// second run reads only the row at the watermark, not a full reload
	res, err = e.Run(ctx, up, nil)
	if err != nil || res[0].Rows != 1 {
		t.Fatalf("roundtrip watermark: %v %+v", err, res)
	}
	if f.count("SELECT count(*) FROM "+f.schema+"_out.roundtrip") != 4 {
		t.Fatal("roundtrip")
	}
	var typ string
	f.pg.QueryRow("SELECT data_type FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'roundtrip' AND column_name = 'updated_at'", f.schema+"_out").Scan(&typ)
	if typ != "timestamp without time zone" {
		t.Fatalf("sqlite timestamp came back as %s", typ)
	}
}

// TestTimestamptzStoredAsUTC needs a non-UTC local zone to mean anything:
// TZ=Europe/London TUMMY_TEST_PG=... go test -run Timestamptz ./internal/engine/
func TestTimestamptzStoredAsUTC(t *testing.T) {
	f := pgSetup(t)
	ctx := context.Background()
	f.exec(fmt.Sprintf("CREATE TABLE %s.events (id int primary key, at timestamptz)", f.schema))
	f.exec(fmt.Sprintf("INSERT INTO %s.events VALUES (1, '2026-07-01 12:00:00+00'), (2, '2026-07-01 12:30:00+00')", f.schema))
	r := f.replication("source = \"SRC\"\ntarget = \"PG\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\nobject = \"{target_schema}.events\"\n[streams.\"{s}.events\"]\nupdate_key = \"at\"\n")
	e := f.engine()
	if _, err := e.Run(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	var stored string
	f.pg.QueryRow("SELECT to_char(at, 'YYYY-MM-DD HH24:MI') FROM " + f.schema + "_out.events WHERE id = 1").Scan(&stored)
	if stored != "2026-07-01 12:00" {
		t.Fatalf("12:00Z stored as %s (local zone %s)", stored, time.Local)
	}
	// a row 30 minutes after the max is picked up: the watermark is not an hour ahead
	f.exec(fmt.Sprintf("INSERT INTO %s.events VALUES (3, '2026-07-01 13:00:00+00')", f.schema))
	res, err := e.Run(ctx, r, nil)
	if err != nil || res[0].Rows != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	lite := f.replication("source = \"SRC\"\ntarget = \"LITE\"\n[streams.\"{s}.events\"]\n")
	if _, err := e.Run(ctx, lite, nil); err != nil {
		t.Fatal(err)
	}
	l, _ := sql.Open("sqlite", filepath.Join(f.dir, "lite.db"))
	defer l.Close()
	l.QueryRow("SELECT CAST(at AS TEXT) FROM " + f.schema + "_events WHERE id = 1").Scan(&stored)
	if stored != "2026-07-01 12:00:00+00:00" {
		t.Fatalf("sqlite: %s", stored)
	}
}

func TestPostgresDBHelpers(t *testing.T) {
	f := pgSetup(t)
	ctx := context.Background()
	d, err := db.Open(ctx, db.Params{Name: "SRC", Type: "postgres", URL: f.dsn, Schema: f.schema})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if pk, err := d.PrimaryKey(ctx, db.Table{Schema: f.schema, Name: "orders"}); err != nil || len(pk) != 1 || pk[0] != "id" {
		t.Fatalf("orders: %v %v", pk, err)
	}
	if pk, err := d.PrimaryKey(ctx, db.Table{Name: "customers"}); err != nil || len(pk) != 1 {
		t.Fatalf("default schema: %v %v", pk, err)
	}
	if pk, err := d.PrimaryKey(ctx, db.Table{Schema: f.schema, Name: "v_totals"}); err != nil || len(pk) != 0 {
		t.Fatalf("view: %v %v", pk, err)
	}
	schemas, err := d.ListSchemas(ctx)
	if err != nil || len(schemas) == 0 || schemas[0] != f.schema {
		t.Fatalf("schemas: %v %v", schemas, err)
	}
	for _, s := range schemas {
		if s == "information_schema" || strings.HasPrefix(s, "pg_") {
			t.Fatalf("system schema listed: %v", schemas)
		}
	}
	all, err := d.ListAllTables(ctx)
	if err != nil || len(all) < 3 || all[0].Schema != f.schema {
		t.Fatalf("all tables: %v %v", all, err)
	}
	q := fmt.Sprintf(`SELECT * FROM %s.orders WHERE total > $1 ORDER BY id`, f.schema)
	if n, err := d.Count(ctx, q, 15); err != nil || n != 2 {
		t.Fatalf("count: %d %v", n, err)
	}
	vals, err := d.Distinct(ctx, q, "flag", 10, 0)
	if err != nil || len(vals) != 3 || vals[0] != false || vals[1] != true || vals[2] != nil {
		t.Fatalf("distinct: %#v %v", vals, err)
	}
}

// A test load into postgres reports a value it cannot store, by row and column, and
// leaves no schema or table behind.
func TestPostgresPlanSample(t *testing.T) {
	f := pgSetup(t)
	lite, err := sql.Open("sqlite", filepath.Join(f.dir, "lite.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Close()
	for _, q := range []string{
		`CREATE TABLE notes (id INTEGER, v TEXT)`,
		`INSERT INTO notes VALUES (1, 'fine'), (2, CAST(X'346cf28e6c1c' AS BLOB))`,
		`CREATE TABLE good (id INTEGER, v TEXT)`,
		`INSERT INTO good VALUES (1, 'a'), (2, 'b')`,
	} {
		if _, err := lite.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := f.replication("source = \"LITE\"\ntarget = \"PG\"\nmode = \"full-refresh\"\n[streams.notes]\n[streams.good]\n")
	e := f.engine()
	rows, err := e.PlanSample(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]PlanRow{}
	for _, p := range rows {
		by[p.Stream] = p
	}
	if s := by["notes"].Sample; s == nil || !strings.Contains(s.Err, "row 2, column v") || !strings.Contains(s.Err, "invalid byte sequence") || s.Rows != 1 {
		t.Fatalf("notes sample: %+v", s)
	}
	if s := by["good"].Sample; s == nil || s.Err != "" || s.Rows != 2 {
		t.Fatalf("good sample: %+v", s)
	}
	if n := f.count(fmt.Sprintf("SELECT count(*) FROM information_schema.schemata WHERE schema_name = '%s_out'", f.schema)); n != 0 {
		t.Fatal("test load left the target schema behind")
	}
	if _, err := e.Run(context.Background(), r, []string{"good"}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(fmt.Sprintf("SELECT count(*) FROM information_schema.tables WHERE table_schema = '%s_out'", f.schema)); n != 1 {
		t.Fatalf("after the run: %d tables, want only good", n)
	}
}

func TestPostgresTableStats(t *testing.T) {
	f := pgSetup(t)
	f.exec("ANALYZE " + f.schema + ".orders")
	d, err := db.Open(context.Background(), f.conns.Conn["SRC"].DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stats, err := d.TableStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]db.TableStat{}
	for _, s := range stats {
		by[s.Table.String()] = s
	}
	o := by[f.schema+".orders"]
	if o.Rows != 3 || o.Bytes <= 0 || o.Exact {
		t.Fatalf("orders: %+v", o)
	}
	if c, ok := by[f.schema+".customers"]; !ok || c.Rows != -1 { // never analyzed
		t.Fatalf("customers: %+v %v", c, ok)
	}
	if _, ok := by[f.schema+".v_totals"]; ok {
		t.Fatal("a view has stats")
	}
	if n, err := d.CountTable(context.Background(), db.Table{Schema: f.schema, Name: "customers"}); err != nil || n != 2 {
		t.Fatalf("count: %d %v", n, err)
	}
}
