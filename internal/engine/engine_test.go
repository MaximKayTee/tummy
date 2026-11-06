package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tummy/internal/api"
	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/state"
)

type fixture struct {
	t     *testing.T
	dir   string
	src   *sql.DB
	tgt   *sql.DB
	conns *config.Connections
	st    *state.Store
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, dir: dir}
	var err error
	f.src, err = sql.Open("sqlite", filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.tgt, err = sql.Open("sqlite", filepath.Join(dir, "tgt.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(f.src, `CREATE TABLE orders (id INTEGER PRIMARY KEY, customer TEXT, total REAL, updated_at TEXT)`)
	f.exec(f.src, `INSERT INTO orders VALUES (1,'ann',10.5,'2026-01-01'),(2,'bob',20,'2026-01-02'),(3,'cy',30,'2026-01-03')`)
	f.exec(f.src, `CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT)`)
	f.exec(f.src, `INSERT INTO customers VALUES (1,'Ann'),(2,'Bob')`)
	f.exec(f.src, `CREATE TABLE tmp_scratch (x INTEGER)`)
	f.conns = &config.Connections{Conn: map[string]*config.Conn{
		"SRC": {Name: "SRC", Type: "sqlite", Path: filepath.Join(dir, "src.db")},
		"TGT": {Name: "TGT", Type: "sqlite", Path: filepath.Join(dir, "tgt.db")},
		"BAD": {Name: "BAD", Type: "postgres", URL: "postgres://x:y@127.0.0.1:1/z?connect_timeout=1"},
	}}
	f.st, err = state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.src.Close(); f.tgt.Close(); f.st.Close() })
	return f
}

func (f *fixture) exec(db *sql.DB, q string, args ...any) {
	f.t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *fixture) count(q string) int {
	f.t.Helper()
	var n int
	if err := f.tgt.QueryRow(q).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return n
}

// run runs r and fails the test on error.
func (f *fixture) run(e *Engine, r *config.Replication, only ...string) []Result {
	f.t.Helper()
	res, err := e.Run(context.Background(), r, only)
	if err != nil {
		f.t.Fatalf("%v %+v", err, res)
	}
	return res
}

func (f *fixture) engine() *Engine {
	return &Engine{Conns: f.conns, State: f.st, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), LockDir: filepath.Join(f.dir, "locks")}
}

func (f *fixture) replication(body string) *config.Replication {
	f.t.Helper()
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

func TestFullRefreshAndWildcard(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
exclude = ["tmp_*"]
[streams."*"]
[streams.orders]
object = "orders_copy"
`)
	results, err := f.engine().Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("explicit stream should replace wildcard match: %+v", results)
	}
	if f.count("SELECT count(*) FROM orders_copy") != 3 || f.count("SELECT count(*) FROM customers") != 2 {
		t.Fatal("counts")
	}
	if f.count("SELECT count(*) FROM sqlite_master WHERE name = 'tmp_scratch'") != 0 {
		t.Fatal("exclude ignored")
	}
	if f.count("SELECT count(*) FROM sqlite_master WHERE name LIKE '_tummy_tmp_%'") != 0 {
		t.Fatal("temp table left behind")
	}
	// second run replaces
	f.exec(f.src, "DELETE FROM orders WHERE id = 3")
	if _, err := f.engine().Run(context.Background(), r, []string{"orders"}); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM orders_copy") != 2 {
		t.Fatal("full refresh did not replace")
	}
	// each run records the target tables' readings
	stats, err := f.st.AllTableStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ts := stats[state.TableKey("TGT", "", "orders_copy")]; ts.Rows != 2 || !ts.Exact {
		t.Fatalf("orders_copy reading: %+v (%d stored)", ts, len(stats))
	}
	if ts := stats[state.TableKey("TGT", "", "customers")]; ts.Rows != 2 {
		t.Fatalf("customers reading: %+v", ts)
	}
}

func TestIncrementalUpsert(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
mode = "incremental"
primary_key = ["id"]
[streams.orders]
update_key = "updated_at"
`)
	e := f.engine()
	if _, err := e.Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	f.exec(f.src, "UPDATE orders SET total = 99, updated_at = '2026-01-05' WHERE id = 2")
	f.exec(f.src, "INSERT INTO orders VALUES (4,'dee',40,'2026-01-04')")
	f.exec(f.src, "UPDATE orders SET total = 11 WHERE id = 1") // not after watermark: must be missed
	res, err := e.Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Rows != 3 { // 2 new plus id 3 at the watermark
		t.Fatalf("want 3 rows read from the watermark, got %d", res[0].Rows)
	}
	if f.count("SELECT count(*) FROM orders") != 4 {
		t.Fatal("row count")
	}
	if f.count("SELECT CAST(total AS INTEGER) FROM orders WHERE id = 2") != 99 {
		t.Fatal("upsert did not update")
	}
	if f.count("SELECT CAST(total AS INTEGER) FROM orders WHERE id = 1") != 10 {
		t.Fatal("row below watermark should be untouched")
	}
}

func TestIncrementalAddsColumns(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\n[streams.customers]\n")
	e := f.engine()
	if _, err := e.Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	f.exec(f.src, "ALTER TABLE customers ADD email TEXT")
	f.exec(f.src, "UPDATE customers SET email = 'a@x' WHERE id = 1")
	if _, err := e.Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	var email string
	if err := f.tgt.QueryRow("SELECT email FROM customers WHERE id = 1").Scan(&email); err != nil || email != "a@x" {
		t.Fatalf("new column: %q %v", email, err)
	}
}

func TestSnapshotTruncateBackfill(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	snap := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"snapshot\"\n[streams.customers]\nobject = \"cust_snap\"\n")
	e := f.engine()
	f.run(e, snap)
	f.run(e, snap)
	if f.count("SELECT count(*) FROM cust_snap") != 4 || f.count("SELECT count(DISTINCT _tummy_loaded_at) FROM cust_snap") != 2 {
		t.Fatal("snapshot")
	}

	tr := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"truncate\"\n[streams.customers]\nobject = \"cust_tr\"\n")
	f.run(e, tr)
	f.exec(f.tgt, "CREATE INDEX cust_tr_idx ON cust_tr(id)")
	f.exec(f.src, "DELETE FROM customers WHERE id = 2")
	if _, err := e.Run(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM cust_tr") != 1 || f.count("SELECT count(*) FROM sqlite_master WHERE name = 'cust_tr_idx'") != 1 {
		t.Fatal("truncate should keep DDL and replace rows")
	}

	bf := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"backfill\"\nprimary_key = [\"id\"]\nupdate_key = \"updated_at\"\nrange = \"2026-01-02,2026-01-03\"\n[streams.orders]\nobject = \"orders_bf\"\n")
	res, err := e.Run(ctx, bf, nil)
	if err != nil || res[0].Rows != 2 {
		t.Fatalf("backfill: %v %+v", err, res)
	}
}

func TestCustomSQLWithIncrementalPlaceholder(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
mode = "incremental"
primary_key = ["id"]
update_key = "updated_at"
[streams.big]
sql = "select id, total, updated_at from orders where {incremental_where_cond} and total > 0"
object = "big_orders"
`)
	e := f.engine()
	if _, err := e.Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	f.exec(f.src, "INSERT INTO orders VALUES (9,'z',1,'2026-02-01')")
	res, err := e.Run(context.Background(), r, nil)
	if err != nil || res[0].Rows != 2 || f.count("SELECT count(*) FROM big_orders") != 4 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestFailureIsRecorded(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"BAD\"\ntarget = \"TGT\"\n[streams.x]\n")
	e := f.engine()
	e.Trigger = "manual"
	if _, err := e.Run(context.Background(), r, nil); err == nil {
		t.Fatal("expected connection error")
	}
	runs, _ := f.st.Recent(context.Background(), 5)
	if len(runs) != 1 || runs[0].Stream != "*" || runs[0].Status != "error" || runs[0].Trigger != "manual" {
		t.Fatalf("failure row: %+v", runs)
	}
	// a stream-level failure leaves no temp table and keeps going with the others
	r2 := f.replication("source = \"SRC\"\ntarget = \"TGT\"\n[streams.missing_table]\n[streams.customers]\n")
	res, err := e.Run(context.Background(), r2, nil)
	if err == nil || len(res) != 2 || res[0].Err == nil || res[1].Err != nil {
		t.Fatalf("partial failure: %v %+v", err, res)
	}
}

func TestLockPreventsOverlap(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\n[streams.customers]\n")
	e := f.engine()
	// hold the lock as another "process" would
	held, err := acquireFor(e.LockDir, r.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	if _, err := e.Run(context.Background(), r, nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("want busy error, got %v", err)
	}
}

// ---------- API source ----------

type apiServer struct {
	*httptest.Server
	hits atomic.Int32
}

func newAPI(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{}
	mux := http.NewServeMux()
	users := []map[string]any{
		{"id": 1, "name": "Ann", "address": map[string]any{"city": "Oslo", "geo": map[string]any{"lat": 1.5}}, "tags": []any{"a", "b"}},
		{"id": 2, "name": "Bob", "address": map[string]any{"city": "Rome", "geo": map[string]any{"lat": 2}}, "tags": []any{}},
	}
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no auth", 401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": users})
	})
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		id := strings.TrimPrefix(r.URL.Path, "/users/")
		id = strings.TrimSuffix(id, "/posts")
		n, _ := strconv.Atoi(id)
		var posts []map[string]any
		for i := 1; i <= n; i++ {
			posts = append(posts, map[string]any{"id": n*10 + i, "title": fmt.Sprintf("post %d", i)})
		}
		json.NewEncoder(w).Encode(posts)
	})
	// items with a video each, except 4 (404) and 2 which is old
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "created": "2026-03-01"}, {"id": 2, "created": "2025-01-01"},
			{"id": 3, "created": "2026-03-03"}, {"id": 4, "created": "2026-03-04"}, {"id": 5, "created": "2026-03-05"},
		})
	})
	mux.HandleFunc("/items/", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/items/"), "/video")
		if id == "4" {
			http.Error(w, "none", 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"url": "https://v/" + id})
	})
	// page pagination: 3 pages of 2, then a short page
	mux.HandleFunc("/paged", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var items []map[string]any
		for i := 0; i < size && (page-1)*size+i < 5; i++ {
			items = append(items, map[string]any{"n": (page-1)*size + i})
		}
		json.NewEncoder(w).Encode(items)
	})
	// cursor pagination with a full next URL
	mux.HandleFunc("/cursor", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		var next any
		if off+2 < 5 {
			next = s.URL + "/cursor?offset=" + strconv.Itoa(off+2)
		}
		json.NewEncoder(w).Encode(map[string]any{"next": next, "results": []map[string]any{{"n": off}, {"n": off + 1}}})
	})
	// link header pagination
	mux.HandleFunc("/linked", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		p, _ := strconv.Atoi(r.URL.Query().Get("p"))
		if p < 2 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/linked?p=%d>; rel="next", <x>; rel="last"`, s.URL, p+1))
		}
		json.NewEncoder(w).Encode([]map[string]any{{"p": p}})
	})
	// flaky: first call 503 with Retry-After, then ok
	var flaky atomic.Int32
	mux.HandleFunc("/flaky", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if flaky.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "later", 503)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{"ok": true}})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestAPISource(t *testing.T) {
	f := setup(t)
	api := newAPI(t)
	f.conns.Conn["API"] = &config.Conn{Name: "API", Type: "api", BaseURL: api.URL, Auth: "bearer", Token: "tok", RateLimit: "1000/s"}
	r := f.replication(`
source = "API"
target = "TGT"

[streams.users]
path = "/users"
records = "data[]"

[streams.user_posts]
path = "/users/{users.id}/posts"
for_each = "users"
add_columns = { user_id = "{users.id}", user_name = "{users.name}" }

[streams.paged]
path = "/paged"
paginate = "page"
page_size = 2

[streams.cursor]
path = "/cursor"
records = "results[]"
paginate = "cursor"
cursor_field = "next"

[streams.linked]
path = "/linked"
paginate = "link"

[streams.flaky]
path = "/flaky"
`)
	res, err := f.engine().Run(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]int64{}
	for _, x := range res {
		rows[x.Stream] = x.Rows
	}
	want := map[string]int64{"users": 2, "user_posts": 3, "paged": 5, "cursor": 6, "linked": 3, "flaky": 1}
	for k, v := range want {
		if rows[k] != v {
			t.Errorf("%s: want %d rows, got %d", k, v, rows[k])
		}
	}
	// flattened nested object, array as JSON text, number types
	var city, tags string
	var lat float64
	if err := f.tgt.QueryRow("SELECT address_city, tags, address_geo_lat FROM users WHERE id = 1").Scan(&city, &tags, &lat); err != nil {
		t.Fatal(err)
	}
	if city != "Oslo" || tags != `["a","b"]` || lat != 1.5 {
		t.Fatalf("flatten: %q %q %v", city, tags, lat)
	}
	var typ string
	f.tgt.QueryRow("SELECT type FROM pragma_table_info('users') WHERE name = 'address_geo_lat'").Scan(&typ)
	if typ != "REAL" {
		t.Fatalf("int and float should widen to REAL, got %s", typ)
	}
	if f.count("SELECT count(*) FROM user_posts WHERE user_name = 'Bob' AND user_id = 2") != 2 {
		t.Fatal("for_each add_columns")
	}
	// only the child requested still runs its parent
	api.hits.Store(0)
	if _, err := f.engine().Run(context.Background(), r, []string{"user_posts"}); err != nil {
		t.Fatal(err)
	}
	if api.hits.Load() != 3 {
		t.Fatalf("want users + 2 post calls, got %d", api.hits.Load())
	}
}

func TestAPIForEachSkipExisting(t *testing.T) {
	f := setup(t)
	api := newAPI(t)
	f.conns.Conn["API"] = &config.Conn{Name: "API", Type: "api", BaseURL: api.URL, Auth: "bearer", Token: "tok", RateLimit: "1000/s"}
	r := f.replication(`
source = "API"
target = "TGT"
mode = "incremental"

[streams.items]
path = "/items"
primary_key = ["id"]
update_key = "id"

[streams.videos]
path = "/items/{items.id}/video"
for_each = "items"
primary_key = ["item"]
add_columns = { item = "{items.id}" }
skip_existing = "item"
for_each_since = "created, 2026-01-01"
skip_on = [404]
`)
	if _, err := f.engine().Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	// 2 is too old, 4 has no video
	if n := f.count("SELECT count(*) FROM videos"); n != 3 {
		t.Fatalf("want 3 videos, got %d", n)
	}
	if api.hits.Load() != 5 {
		t.Fatalf("want items + 4 video calls, got %d", api.hits.Load())
	}
	// second run: items is re-read in full, only 4 is tried again
	api.hits.Store(0)
	if _, err := f.engine().Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	if api.hits.Load() != 2 {
		t.Fatalf("want items + 1 video call, got %d", api.hits.Load())
	}
	if n := f.count("SELECT count(*) FROM videos"); n != 3 {
		t.Fatalf("want 3 videos after re-run, got %d", n)
	}
	// 30d keeps nothing from 2026
	r.Resolved[1].ForEachSince = "created, 30d"
	api.hits.Store(0)
	if _, err := f.engine().Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	if api.hits.Load() != 1 {
		t.Fatalf("want items only, got %d", api.hits.Load())
	}
}

func TestAPIErrors(t *testing.T) {
	f := setup(t)
	api := newAPI(t)
	f.conns.Conn["API"] = &config.Conn{Name: "API", Type: "api", BaseURL: api.URL, Auth: "bearer", Token: "wrong"}
	r := f.replication("source = \"API\"\ntarget = \"TGT\"\n[streams.users]\npath = \"/users\"\nrecords = \"data[]\"\n")
	res, err := f.engine().Run(context.Background(), r, nil)
	if err == nil || !strings.Contains(res[0].Err.Error(), "HTTP 401") {
		t.Fatalf("want 401, got %v", res)
	}
	cyc := f.replication("source = \"API\"\ntarget = \"TGT\"\n[streams.a]\npath = \"/x\"\nfor_each = \"b\"\n[streams.b]\npath = \"/y\"\nfor_each = \"a\"\n")
	if _, err := f.engine().Run(context.Background(), cyc, nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
	unk := f.replication("source = \"API\"\ntarget = \"TGT\"\n[streams.a]\npath = \"/x\"\nfor_each = \"nope\"\n")
	if _, err := f.engine().Run(context.Background(), unk, nil); err == nil || !strings.Contains(err.Error(), "unknown stream") {
		t.Fatalf("want unknown stream error, got %v", err)
	}
}

// ---------- API incremental: filtering, early stop, since formats ----------

type feed struct {
	*httptest.Server
	hits   atomic.Int32
	since  atomic.Value // last since value received
	pageOf func(page int) []map[string]any
}

// newFeed serves /items newest-first in pages of 3 with page/limit params, 9 items,
// updated_at descending from 2026-01-09 to 2026-01-01.
func newFeed(t *testing.T) *feed {
	t.Helper()
	f := &feed{}
	f.since.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if v := r.URL.Query().Get("since"); v != "" {
			f.since.Store(v)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		var items []map[string]any
		for i := 0; i < 3; i++ {
			n := 9 - (page-1)*3 - i
			if n < 1 {
				break
			}
			items = append(items, map[string]any{"id": n, "updatedAt": fmt.Sprintf("2026-01-%02dT10:00:00Z", n), "v": "x"})
		}
		json.NewEncoder(w).Encode(items)
	})
	// epoch seconds, ascending, no key field overlap test
	mux.HandleFunc("/epoch", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if v := r.URL.Query().Get("from"); v != "" {
			f.since.Store(v)
		}
		json.NewEncoder(w).Encode([]map[string]any{{"ts": 1700000000, "n": 1}, {"ts": 1700000100, "n": 2}, {"ts": 1700000200, "n": 3}})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestAPIIncrementalFiltersAndStopsEarly(t *testing.T) {
	fx := setup(t)
	fd := newFeed(t)
	fx.conns.Conn["FEED"] = &config.Conn{Name: "FEED", Type: "api", BaseURL: fd.URL}
	r := fx.replication(`
source = "FEED"
target = "TGT"
mode = "incremental"
primary_key = ["id"]
update_key = "updatedAt"
[streams.items]
path = "/items"
paginate = "page"
page_size = 3
sorted = "desc"
`)
	e := fx.engine()
	res, err := e.Run(context.Background(), r, nil)
	if err != nil || res[0].Rows != 9 || fd.hits.Load() != 4 {
		t.Fatalf("first run: %v rows=%d hits=%d", err, res[0].Rows, fd.hits.Load())
	}
	// nothing new: the first page reaches the watermark, so one request and only the record at it
	fd.hits.Store(0)
	res, err = e.Run(context.Background(), r, nil)
	if err != nil || res[0].Rows != 1 || fd.hits.Load() != 1 {
		t.Fatalf("no-op run: %v rows=%d hits=%d", err, res[0].Rows, fd.hits.Load())
	}
	if fx.count("SELECT count(*) FROM items") != 9 {
		t.Fatal("rows changed")
	}
	// pretend the target only knows up to day 5: pages 1 and 2 are needed, page 3 is not
	fx.exec(fx.tgt, "DELETE FROM items WHERE id > 5")
	fd.hits.Store(0)
	res, err = e.Run(context.Background(), r, nil)
	if err != nil || res[0].Rows != 5 || fd.hits.Load() != 2 {
		t.Fatalf("partial run: %v rows=%d hits=%d", err, res[0].Rows, fd.hits.Load())
	}
	if fx.count("SELECT count(*) FROM items") != 9 || fx.count("SELECT count(DISTINCT id) FROM items") != 9 {
		t.Fatal("after partial run")
	}

	// append-only (no primary key): overlapping pages must not duplicate
	r2 := fx.replication(`
source = "FEED"
target = "TGT"
mode = "incremental"
update_key = "updatedAt"
[streams.items]
path = "/items"
paginate = "page"
page_size = 3
object = "items_append"
`)
	fx.run(e, r2)
	fx.exec(fx.tgt, "DELETE FROM items_append WHERE id > 7")
	res, err = e.Run(context.Background(), r2, nil)
	if err != nil || res[0].Rows != 2 || fx.count("SELECT count(*) FROM items_append") != 9 {
		t.Fatalf("append-only: %v rows=%d count=%d", err, res[0].Rows, fx.count("SELECT count(*) FROM items_append"))
	}
}

func TestAPISinceFormats(t *testing.T) {
	fx := setup(t)
	fd := newFeed(t)
	fx.conns.Conn["FEED"] = &config.Conn{Name: "FEED", Type: "api", BaseURL: fd.URL}
	run := func(format, path, param, key string) string {
		r := fx.replication(fmt.Sprintf(`
source = "FEED"
target = "TGT"
mode = "incremental"
primary_key = ["%s"]
update_key = "%s"
[streams.s]
path = "%s"
since_param = "%s"
since_format = "%s"
object = "fmt_%s"
`, key, key, path, param, format, format))
		e := fx.engine()
		fd.since.Store("")
		if _, err := e.Run(context.Background(), r, nil); err != nil {
			t.Fatal(err)
		}
		if fd.since.Load().(string) != "" {
			t.Fatalf("%s: since sent on the first run", format)
		}
		if _, err := e.Run(context.Background(), r, nil); err != nil {
			t.Fatal(err)
		}
		return fd.since.Load().(string)
	}
	if got := run("rfc3339", "/items", "since", "updatedAt"); got != "2026-01-09T10:00:00Z" {
		t.Errorf("rfc3339: %q", got)
	}
	if got := run("date", "/items", "since", "updatedAt"); got != "2026-01-09" {
		t.Errorf("date: %q", got)
	}
	if got := run("unix", "/items", "since", "updatedAt"); got != "1767952800" {
		t.Errorf("unix: %q", got)
	}
	if got := run("unix_ms", "/epoch", "from", "ts"); got != "1700000200000" {
		t.Errorf("unix_ms from epoch seconds: %q", got)
	}
	if got := run("raw", "/epoch", "from", "ts"); got != "1700000200" {
		t.Errorf("raw: %q", got)
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b any
		want int
	}{
		{"2026-01-02T10:00:00Z", "2026-01-01T10:00:00Z", 1},
		{"2026-01-01", "2026-01-01T00:00:00Z", 0},
		{time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), "2026-01-02 00:00:00+00:00", 1},
		{int64(5), "7", -1},
		{"1700000100", int64(1700000000), 1},
		{"b", "a", 1},
		{1700000000000.0, "2023-11-14T22:13:20Z", 0},
		{int64(99_999_999_999), int64(100_000_000_001), -1}, // ids across 1e11 stay numbers
		{"99999999999", int64(100_000_000_001), -1},
		{int64(9007199254740993), int64(9007199254740992), 1}, // beyond float64
	}
	for _, c := range cases {
		got, ok := compare(c.a, c.b)
		if !ok || got != c.want {
			t.Errorf("compare(%v, %v) = %d %v, want %d", c.a, c.b, got, ok, c.want)
		}
	}
	if _, ok := compare(nil, 1); ok {
		t.Error("nil should not compare")
	}
	recs := []api.Record{{"k": nil}, {"k": int64(5)}, {"k": int64(6)}, {}}
	if n := len(newerThan(recs, "k", int64(5), true)); n != 2 {
		t.Errorf("inclusive: %d", n)
	}
	if n := len(newerThan(recs, "k", int64(5), false)); n != 1 {
		t.Errorf("strict, null keys left out: %d", n)
	}
	if recs[0]["k"] != nil || recs[1]["k"] != int64(5) {
		t.Error("newerThan changed its input")
	}
}

func TestStepsInReplication(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
mode = "incremental"
primary_key = ["id"]
[streams.orders]
update_key = "updated_at"
steps = "filter total >= 20 | rename customer = who | set doubled = total * 2 | remove total"
object = "orders_steps"
`)
	e := f.engine()
	res, err := e.Run(context.Background(), r, nil)
	if err != nil || res[0].Rows != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	var who string
	var doubled float64
	if err := f.tgt.QueryRow("SELECT who, doubled FROM orders_steps WHERE id = 3").Scan(&who, &doubled); err != nil || who != "cy" || doubled != 60 {
		t.Fatalf("%q %v %v", who, doubled, err)
	}
	if f.count("SELECT count(*) FROM pragma_table_info('orders_steps') WHERE name = 'total'") != 0 {
		t.Fatal("dropped column present")
	}
	// incremental on top of steps: the new row and id 3 at the watermark cross; id 10 fails the filter
	f.exec(f.src, "INSERT INTO orders VALUES (9,'zed',100,'2026-03-01')")
	f.exec(f.src, "INSERT INTO orders VALUES (10,'low',1,'2026-03-02')")
	res, err = e.Run(context.Background(), r, nil)
	if err != nil || res[0].Rows != 2 || f.count("SELECT count(*) FROM orders_steps") != 3 {
		t.Fatalf("incremental with steps: %v %+v", err, res)
	}
	bad := f.replication("source = \"SRC\"\ntarget = \"TGT\"\n[streams.orders]\nsteps = \"keep nope\"\n")
	if _, err := e.Run(context.Background(), bad, nil); err == nil {
		t.Fatal("unknown column should fail")
	}
}

func TestSelectExclusion(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
[streams.orders]
select = ["-total", "-updated_at"]
`)
	f.run(f.engine(), r)
	if f.count("SELECT count(*) FROM pragma_table_info('orders')") != 2 || f.count("SELECT count(*) FROM pragma_table_info('orders') WHERE name = 'total'") != 0 {
		t.Fatal("excluded columns copied")
	}
	if f.count("SELECT count(*) FROM orders") != 3 {
		t.Fatal("rows")
	}
	src, err := db.Open(context.Background(), f.conns.Conn["SRC"].DB())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	streams, err := r.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	q, err := BaseQuery(context.Background(), src, streams[0].Stream, "orders")
	if err != nil || !strings.Contains(q, `"id", "customer"`) || strings.Contains(q, "-total") {
		t.Fatalf("base query: %v %s", err, q)
	}
}

func TestReadOnlyTargetRefused(t *testing.T) {
	f := setup(t)
	f.conns.Conn["TGT"].ReadOnly = true
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\n[streams.orders]\n")
	if _, err := f.engine().Run(context.Background(), r, nil); err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Fatalf("read-only target accepted: %v", err)
	}
}

func TestPlanDoesNotWrite(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\nupdate_key = \"updated_at\"\n[streams.orders]\n[streams.customers]\nmode = \"full-refresh\"\n[streams.tmp_scratch]\ndisabled = true\n")
	e := f.engine()
	rows, err := e.Plan(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]PlanRow{}
	for _, p := range rows {
		by[p.Stream] = p
	}
	if p := by["orders"]; p.Action != "create" || p.Exists || !strings.Contains(p.SQL, "orders") || p.Err != "" {
		t.Fatalf("orders before any run: %+v", p)
	}
	if p := by["tmp_scratch"]; p.Action != "skip" || !p.Disabled {
		t.Fatalf("disabled: %+v", p)
	}
	if n := f.count("select count(*) from sqlite_master where name = 'orders'"); n != 0 {
		t.Fatal("plan created the target table")
	}
	f.run(e, r, "orders")
	rows, _ = e.Plan(context.Background(), r)
	for _, p := range rows {
		by[p.Stream] = p
	}
	if p := by["orders"]; p.Action != "upsert" || !p.Exists || p.Watermark == nil || !strings.Contains(p.SQL, "updated_at") {
		t.Fatalf("orders after a run: %+v", p)
	}
	if p := by["customers"]; p.Action != "create" {
		t.Fatalf("customers: %+v", p)
	}
}

func TestRunErrorNamesTheStream(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\n[streams.nope]\n")
	_, err := f.engine().Run(context.Background(), r, nil)
	if err == nil || !strings.Contains(err.Error(), "1 of 1 streams failed: nope: ") {
		t.Fatalf("summary should carry the stream's error: %v", err)
	}
}

func TestPlanSampleRollsBack(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\nupdate_key = \"updated_at\"\n[streams.orders]\n[streams.customers]\nmode = \"snapshot\"\n")
	e := f.engine()
	ctx := context.Background()
	plan := func(sample bool) map[string]PlanRow {
		t.Helper()
		rows, err := e.Plan(ctx, r)
		if sample {
			rows, err = e.PlanSample(ctx, r)
		}
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]PlanRow{}
		for _, p := range rows {
			by[p.Stream] = p
		}
		return by
	}
	names := func(cols []PlanColumn) string {
		var out []string
		for _, c := range cols {
			out = append(out, c.Name+":"+c.Target)
		}
		return strings.Join(out, " ")
	}

	by := plan(false)
	if p := by["orders"]; p.Sample != nil || names(p.Columns) != "id:INTEGER customer:TEXT total:REAL updated_at:TEXT" {
		t.Fatalf("plain plan: sample %+v columns %s", p.Sample, names(p.Columns))
	}
	by = plan(true)
	if s := by["orders"].Sample; s == nil || s.Err != "" || s.Rows != 3 {
		t.Fatalf("orders sample: %+v", s)
	}
	p := by["customers"]
	if s := p.Sample; s == nil || s.Err != "" || s.Rows != 2 {
		t.Fatalf("customers sample: %+v", s)
	}
	if last := p.Columns[len(p.Columns)-1]; last.Name != loadedAtColumn || last.Note == "" {
		t.Fatalf("snapshot load time column: %+v", last)
	}
	if n := f.count("SELECT count(*) FROM sqlite_master"); n != 0 {
		t.Fatalf("test load left %d objects in the target", n)
	}

	f.run(e, r, "orders")
	f.exec(f.src, `ALTER TABLE orders ADD COLUMN note TEXT`)
	by = plan(true)
	var note string
	for _, c := range by["orders"].Columns {
		if c.Name == "note" {
			note = c.Note
		}
	}
	if !strings.Contains(note, "the run adds it") || by["orders"].Sample.Err != "" {
		t.Fatalf("new source column: note %q sample %+v", note, by["orders"].Sample)
	}
	if n := f.count("SELECT count(*) FROM sqlite_master WHERE type = 'table'"); n != 1 {
		t.Fatal("scratch table left behind")
	}
}
