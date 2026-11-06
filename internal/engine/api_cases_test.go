package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"tummy/internal/config"
)

// tree serves /a, /a/{id}/b and /b/{id}/c. Parents carry an update key u.
type tree struct {
	*httptest.Server
	mu    sync.Mutex
	a     []map[string]any
	hits  atomic.Int32
	query atomic.Value // last raw query on /a
}

func newTree(t *testing.T) *tree {
	t.Helper()
	tr := &tree{a: []map[string]any{{"id": 1, "u": "2026-01-01"}, {"id": 2, "u": "2026-01-02"}}}
	tr.query.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		tr.hits.Add(1)
		tr.query.Store(r.URL.RawQuery)
		tr.mu.Lock()
		defer tr.mu.Unlock()
		since := r.URL.Query().Get("since")
		since = since[:min(len(since), 10)]
		var out []map[string]any
		for _, x := range tr.a {
			if since == "" || x["u"].(string) >= since {
				out = append(out, x)
			}
		}
		json.NewEncoder(w).Encode(out)
	})
	children := func(w http.ResponseWriter, r *http.Request) {
		tr.hits.Add(1)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			http.Error(w, "bad id", 400)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{"id": n*10 + 1}, {"id": n*10 + 2}})
	}
	mux.HandleFunc("/a/", children)
	mux.HandleFunc("/b/", children)
	tr.Server = httptest.NewServer(mux)
	t.Cleanup(tr.Close)
	return tr
}

func treeFixture(t *testing.T) (*fixture, *tree) {
	f := setup(t)
	tr := newTree(t)
	f.conns.Conn["T"] = &config.Conn{Name: "T", Type: "api", BaseURL: tr.URL}
	return f, tr
}

func TestAPIForEachDepth2Only(t *testing.T) {
	f, tr := treeFixture(t)
	r := f.replication(`
source = "T"
target = "TGT"
[streams.c]
path = "/b/{b.id}/c"
for_each = "b"
[streams.b]
path = "/a/{a.id}/b"
for_each = "a"
[streams.a]
path = "/a"
`)
	// only the child runs; its ancestors are fetched for their records, not loaded
	res := f.run(f.engine(), r, "c")
	if len(res) != 1 || res[0].Stream != "c" || f.count("SELECT count(*) FROM c") != 8 || tr.hits.Load() != 1+2+4 {
		t.Fatalf("%+v hits=%d", res, tr.hits.Load())
	}
	if n := f.count("SELECT count(*) FROM sqlite_master WHERE name IN ('a', 'b')"); n != 0 {
		t.Fatalf("ancestors loaded: %d tables", n)
	}
	if runs, _ := f.st.RunsFor(context.Background(), r.Name, "", 10); len(runs) != 1 {
		t.Fatalf("runs recorded: %d", len(runs))
	}
}

// A loaded parent is read from its target table, not fetched again.
func TestAPIForEachChildOnlyReadsLoadedParent(t *testing.T) {
	f, tr := treeFixture(t)
	r := f.replication(`
source = "T"
target = "TGT"
[streams.a]
path = "/a"
[streams.b]
path = "/a/{a.id}/b"
for_each = "a"
`)
	e := f.engine()
	f.run(e, r, "a")
	tr.mu.Lock()
	tr.a = append(tr.a, map[string]any{"id": 3, "u": "2026-01-03"}) // in the API, not loaded
	tr.mu.Unlock()
	tr.hits.Store(0)
	res := f.run(e, r, "b")
	if len(res) != 1 || res[0].Rows != 4 || tr.hits.Load() != 2 {
		t.Fatalf("%+v hits=%d", res, tr.hits.Load())
	}
}

// The child's run reports each parent fetch as its own phase, then its own fetch.
func TestAPIForEachChildOnlyProgress(t *testing.T) {
	f, _ := treeFixture(t)
	r := f.replication(`
source = "T"
target = "TGT"
[streams.c]
path = "/b/{b.id}/c"
for_each = "b"
[streams.b]
path = "/a/{a.id}/b"
for_each = "a"
[streams.a]
path = "/a"
`)
	e := f.engine()
	var seen []string // distinct phase+object in order; counts are throttled
	var last Event
	e.OnEvent = func(ev Event) {
		if ev.Stream == "c" && (ev.Phase == PhaseParents || ev.Phase == PhaseFetching) {
			last = ev
			if k := ev.Phase + " " + ev.Object; len(seen) == 0 || seen[len(seen)-1] != k {
				seen = append(seen, k)
			}
		}
	}
	f.run(e, r, "c")
	if got := strings.Join(seen, "|"); got != "parents a|parents b|fetching c" {
		t.Fatalf("phases: %s", got)
	}
	if last.Of != 4 || last.Done != 4 || last.Rows != 8 {
		t.Fatalf("last fetching event: %+v", last)
	}
}

func TestAPIForEachIncrementalParent(t *testing.T) {
	f, tr := treeFixture(t)
	r := f.replication(`
source = "T"
target = "TGT"
[streams.a]
path = "/a"
mode = "incremental"
primary_key = ["id"]
update_key = "u"
since_param = "since"
[streams.b]
path = "/a/{a.id}/b"
for_each = "a"
`)
	e := f.engine()
	f.run(e, r)
	// nothing new: the full-refresh child still gets every parent
	res := f.run(e, r)
	if res[1].Rows != 4 || f.count("SELECT count(*) FROM b") != 4 {
		t.Fatalf("no change: %+v", res)
	}
	if q := tr.query.Load().(string); strings.Contains(q, "since") {
		t.Fatalf("parent narrowed for a full-refresh child: %q", q)
	}
	tr.mu.Lock()
	tr.a[0]["u"] = "2026-02-01"
	tr.mu.Unlock()
	res = f.run(e, r)
	if res[0].Rows != 2 || res[1].Rows != 4 || f.count("SELECT count(*) FROM b") != 4 {
		t.Fatalf("one changed: %+v", res)
	}
	// an incremental child iterates only changed parents, and the parent request is narrowed
	r2 := f.replication(`
source = "T"
target = "TGT"
[streams.a]
path = "/a"
mode = "incremental"
primary_key = ["id"]
update_key = "u"
since_param = "since"
object = "a2"
[streams.b]
path = "/a/{a.id}/b"
for_each = "a"
mode = "incremental"
primary_key = ["id"]
object = "b2"
`)
	f.run(e, r2)
	res = f.run(e, r2)
	if q := tr.query.Load().(string); q != "since=2026-02-01T00%3A00%3A00Z" {
		t.Fatalf("since: %q", q)
	}
	if res[0].Rows != 1 || res[1].Rows != 2 {
		t.Fatalf("incremental child: %+v", res)
	}
}

func TestAPIForEachSkipsMissingParent(t *testing.T) {
	f, tr := treeFixture(t)
	tr.a = append(tr.a, map[string]any{"id": nil, "u": "2026-01-03"}, map[string]any{"u": "2026-01-04"})
	r := f.replication(`
source = "T"
target = "TGT"
[streams.a]
path = "/a"
[streams.b]
path = "/a/{a.id}/b"
for_each = "a"
add_columns = { parent = "{a.id}" }
`)
	res := f.run(f.engine(), r)
	if res[1].Rows != 4 || tr.hits.Load() != 3 {
		t.Fatalf("%+v hits=%d", res, tr.hits.Load())
	}
}

func TestAPILimitSelectDedupe(t *testing.T) {
	f, tr := treeFixture(t)
	r := f.replication(`
source = "T"
target = "TGT"
[streams.a]
path = "/a"
limit = 1
select = ["ID", "missing"]
[streams.b]
path = "/a/{a.id}/b"
for_each = "a"
`)
	res := f.run(f.engine(), r)
	if res[0].Rows != 1 || res[1].Rows != 2 || tr.hits.Load() != 2 {
		t.Fatalf("limit on a parent limits its children: %+v hits=%d", res, tr.hits.Load())
	}
	if f.count("SELECT count(*) FROM pragma_table_info('a')") != 2 || f.count("SELECT count(*) FROM a WHERE missing IS NULL AND id = 1") != 1 {
		t.Fatal("select")
	}

	fx := setup(t)
	fd := newFeed(t)
	fx.conns.Conn["FEED"] = &config.Conn{Name: "FEED", Type: "api", BaseURL: fd.URL}
	lim := fx.replication("source = \"FEED\"\ntarget = \"TGT\"\n[streams.items]\npath = \"/items\"\npaginate = \"page\"\npage_size = 3\nlimit = 4\n")
	if res := fx.run(fx.engine(), lim); res[0].Rows != 4 || fd.hits.Load() != 2 {
		t.Fatalf("limit stops paging: %+v hits=%d", res, fd.hits.Load())
	}
}

func TestAPIDedupe(t *testing.T) {
	fx := setup(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// offset paging over data that shifted: id 2 comes twice, newer the second time
		if r.URL.Query().Get("offset") == "0" {
			calls.Add(1)
			json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "v": "a"}, {"id": 2, "v": "old"}})
			return
		}
		if r.URL.Query().Get("offset") == "2" {
			json.NewEncoder(w).Encode([]map[string]any{{"id": 2, "v": "new"}, {"id": 3, "v": "c"}})
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()
	fx.conns.Conn["P"] = &config.Conn{Name: "P", Type: "api", BaseURL: srv.URL}
	r := fx.replication("source = \"P\"\ntarget = \"TGT\"\nprimary_key = [\"id\"]\n[streams.p]\npath = \"/p\"\npaginate = \"offset\"\npage_size = 2\n")
	fx.run(fx.engine(), r)
	var v string
	if err := fx.tgt.QueryRow("SELECT v FROM p WHERE id = 2").Scan(&v); err != nil || v != "new" || fx.count("SELECT count(*) FROM p") != 3 {
		t.Fatalf("%q %v", v, err)
	}
}

func TestAPIWatermarkPlaceholderAndNumbers(t *testing.T) {
	fx := setup(t)
	var got atomic.Value
	got.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.RawQuery)
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 123456789, "big": json.Number("12345678901234567890.5"), "tags": []string{"x"}},
			{"id": 123456790, "big": 1, "tags": nil},
		})
	}))
	defer srv.Close()
	fx.conns.Conn["N"] = &config.Conn{Name: "N", Type: "api", BaseURL: srv.URL}
	r := fx.replication(`
source = "N"
target = "TGT"
mode = "incremental"
primary_key = ["id"]
update_key = "id"
[streams.n]
path = "/n"
query = { q = "id>{watermark}" }
`)
	e := fx.engine()
	fx.run(e, r)
	if got.Load().(string) != "" {
		t.Fatalf("first run sent %q", got.Load())
	}
	fx.run(e, r)
	if q := got.Load().(string); q != "q=id%3E123456790" {
		t.Fatalf("numeric watermark sent as %q", q)
	}
	var tags string
	if err := fx.tgt.QueryRow("SELECT tags FROM n WHERE id = 123456789").Scan(&tags); err != nil || tags != `["x"]` {
		t.Fatalf("%q %v", tags, err)
	}
	if fx.count("SELECT count(*) FROM pragma_table_info('n') WHERE name = 'big' AND type = 'NUMERIC'") != 1 {
		t.Fatal("json.Number and int should widen to decimal")
	}
}

func TestAPIIncrementalParentNothingNew(t *testing.T) {
	// incremental parent and child, nothing new: both succeed with no rows
	f, _ := treeFixture(t)
	r := f.replication(`
source = "T"
target = "TGT"
mode = "incremental"
primary_key = ["id"]
[streams.a]
path = "/a"
update_key = "u"
[streams.b]
path = "/a/{a.id}/b"
for_each = "a"
`)
	e := f.engine()
	f.run(e, r)
	if _, err := e.Run(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
}
