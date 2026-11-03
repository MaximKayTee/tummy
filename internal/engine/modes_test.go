package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"tummy/internal/config"
	"tummy/internal/db"
)

func TestIncrementalWithoutKeyReadsNothingTwice(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"incremental\"\n[streams.orders]\nupdate_key = \"updated_at\"\nobject = \"log\"\n")
	e := f.engine()
	if res := f.run(e, r); res[0].Rows != 3 {
		t.Fatalf("first: %+v", res)
	}
	if res := f.run(e, r); res[0].Rows != 0 || f.count("SELECT count(*) FROM log") != 3 {
		t.Fatalf("second: %+v", res)
	}
}

func TestIncrementalIntoEmptyTargetAndSecondBackfill(t *testing.T) {
	f := setup(t)
	f.exec(f.tgt, "CREATE TABLE orders (id INTEGER, customer TEXT, total REAL, updated_at TEXT)")
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\n[streams.orders]\nupdate_key = \"updated_at\"\n")
	e := f.engine()
	if res := f.run(e, r); res[0].Rows != 3 || f.count("SELECT count(*) FROM orders") != 3 {
		t.Fatalf("empty target: %+v", res)
	}
	bf := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"backfill\"\nprimary_key = [\"id\"]\nupdate_key = \"updated_at\"\nrange = \"2026-01-01,2026-01-02\"\n[streams.orders]\nobject = \"bf\"\n")
	f.run(e, bf)
	if res := f.run(e, bf); res[0].Rows != 2 || f.count("SELECT count(*) FROM bf") != 2 {
		t.Fatalf("second backfill: %+v", res)
	}
}

func TestPlaceholderTwice(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
mode = "incremental"
update_key = "updated_at"
[streams.u]
sql = "select id, updated_at from orders where {incremental_where_cond} union all select id + 100, updated_at from orders where {incremental_where_cond}"
object = "twice"
`)
	e := f.engine()
	f.run(e, r)
	f.exec(f.src, "INSERT INTO orders VALUES (4,'dee',1,'2026-02-01')")
	if res := f.run(e, r); res[0].Rows != 2 || f.count("SELECT count(*) FROM twice") != 8 {
		t.Fatalf("%+v", res)
	}
}

func TestOnlyUnknownStream(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\n[streams.\"*\"]\n[streams.orders]\n")
	if _, err := f.engine().Run(context.Background(), r, []string{"orders", "nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want unknown stream error, got %v", err)
	}
	// a wildcard's name selects its matches
	res := f.run(f.engine(), r, "*")
	if len(res) != 2 { // customers, tmp_scratch; orders is explicit
		t.Fatalf("%+v", res)
	}
}

func TestDedupeByKey(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
primary_key = ["id"]
update_key = "updated_at"
[streams.d]
sql = "select id, customer, updated_at from orders union all select id, 'newer', '2026-12-01' from orders where id = 2 union all select id, 'older', '2025-01-01' from orders where id = 3"
object = "dedup"
`)
	e := f.engine()
	if res := f.run(e, r); res[0].Rows != 5 {
		t.Fatalf("%+v", res)
	}
	if f.count("SELECT count(*) FROM dedup") != 3 || f.count("SELECT count(*) FROM dedup WHERE customer IN ('newer','cy')") != 2 {
		t.Fatal("highest update key should win")
	}
	if f.count("SELECT count(*) FROM pragma_table_info('dedup') WHERE name = '_tummy_seq'") != 0 {
		t.Fatal("seq column left in target")
	}
	// no update key: the last loaded row wins
	r2 := f.replication(`
source = "SRC"
target = "TGT"
mode = "incremental"
primary_key = ["id"]
[streams.d]
sql = "select 1 as id, 'first' as v union all select 1, 'second'"
object = "lastwins"
`)
	f.run(e, r2)
	f.run(e, r2)
	var v string
	if err := f.tgt.QueryRow("SELECT v FROM lastwins").Scan(&v); err != nil || v != "second" || f.count("SELECT count(*) FROM lastwins") != 1 {
		t.Fatalf("%q %v", v, err)
	}
}

func TestStepsLimitUnderWatermark(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
mode = "incremental"
primary_key = ["id"]
[streams.orders]
update_key = "updated_at"
steps = "sort updated_at | limit 2"
object = "batches"
`)
	e := f.engine()
	f.run(e, r)
	f.run(e, r)
	if f.count("SELECT count(*) FROM batches") != 3 {
		t.Fatal("limit ran before the watermark condition")
	}
	// update key changed by a step: the condition stays outside
	r2 := f.replication(`
source = "SRC"
target = "TGT"
mode = "incremental"
primary_key = ["id"]
[streams.orders]
update_key = "updated_at"
steps = "set updated_at = updated_at || 'x'"
object = "changed"
`)
	f.run(e, r2)
	if res := f.run(e, r2); res[0].Rows != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestWildcardTableNames(t *testing.T) {
	f := setup(t)
	f.exec(f.src, `CREATE TABLE """customers""" (x INTEGER)`) // named "customers", quotes included
	f.exec(f.src, `CREATE TABLE "a.b" (x INTEGER)`)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nexclude = [\"copy_orders\"]\nobject = \"copy_{stream_table}\"\n[streams.\"*\"]\n")
	res := f.run(f.engine(), r)
	var names []string
	for _, x := range res {
		names = append(names, x.Object)
	}
	if strings.Join(names, ",") != "copy_customers,copy_tmp_scratch" {
		t.Fatalf("%v", names)
	}
	if f.count("SELECT count(*) FROM copy_customers") != 2 {
		t.Fatal("quoted table replaced customers")
	}
}

func TestTruncateAddsColumns(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"truncate\"\n[streams.customers]\n")
	e := f.engine()
	f.run(e, r)
	f.exec(f.src, "ALTER TABLE customers ADD email TEXT")
	f.run(e, r)
	if f.count("SELECT count(*) FROM pragma_table_info('customers') WHERE name = 'email'") != 1 {
		t.Fatal("column not added")
	}
}

func TestIndexesOnNewTarget(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\n[streams.orders]\nupdate_key = \"updated_at\"\n")
	f.run(f.engine(), r)
	if f.count("SELECT count(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'orders'") != 2 {
		t.Fatal("want indexes on id and updated_at")
	}
	f.run(f.engine(), r)
}

func TestLongTargetName(t *testing.T) {
	f := setup(t)
	long := strings.Repeat("x", 70)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\n[streams.customers]\nobject = \"" + long + "\"\n")
	f.run(f.engine(), r)
	f.run(f.engine(), r)
	if f.count("SELECT count(*) FROM "+long) != 2 {
		t.Fatal("rows")
	}
}

func TestQueryTimeout(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
[streams.slow]
sql = "with recursive c(n) as (select 1 union all select n + 1 from c where n < 100000000) select count(*) as n from c"
object = "slow"
query_timeout = "50ms"
`)
	res, err := f.engine().Run(context.Background(), r, nil)
	if err == nil || !strings.Contains(res[0].Err.Error(), "query_timeout") {
		t.Fatalf("%v %+v", err, res)
	}
	// the timeout covers the wait for the first row, not the read
	ctx := context.Background()
	src, err := db.Open(ctx, f.conns.Conn["SRC"].DB())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	rs, err := f.engine().openDB(ctx, src, src, plain(config.ResolvedStream{Name: "orders", Stream: config.Stream{Mode: "full-refresh"}}), db.Table{Name: "x"}, false, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.close()
	if _, err := rs.next(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	for {
		row, err := rs.next()
		if err != nil {
			t.Fatalf("read after the first row: %v", err)
		}
		if row == nil {
			break
		}
	}
}
