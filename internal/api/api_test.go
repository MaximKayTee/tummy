package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tummy/internal/config"
)

// clock records waits instead of sleeping and moves its time forward.
type clock struct {
	mu  sync.Mutex
	at  time.Time
	all []time.Duration
}

func (c *clock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.all = append(c.all, d)
	c.at = c.at.Add(d)
	return nil
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.all...)
}

func testClient(t *testing.T, c *config.Conn) (*Client, *clock) {
	t.Helper()
	if c.Name == "" {
		c.Name = t.Name()
	}
	cl, err := NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	ck := &clock{at: time.Now()}
	cl.sleep, cl.now = ck.sleep, ck.now
	return cl, ck
}

func writeJSON(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }

func items(from, to int) []map[string]any {
	out := []map[string]any{}
	for i := from; i < to; i++ {
		out = append(out, map[string]any{"n": i})
	}
	return out
}

func ns(recs []Record) string {
	var parts []string
	for _, r := range recs {
		parts = append(parts, fmt.Sprint(r["n"]))
	}
	return strings.Join(parts, ",")
}

func TestParseRate(t *testing.T) {
	for in, want := range map[string]float64{"60/m": 1, "10/s": 10, "3600/h": 1} {
		got, err := ParseRate(in)
		if err != nil || float64(got) != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "10", "x/m", "0/s", "5/d"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestFill(t *testing.T) {
	vars := map[string]string{"d.id": "7", "d.name": "a b/c?d#e", "d.q": `x","admin":true,"y":"`, "d.null": ""}
	out, err := fillPath("/devices/{d.id}/x/{d.name}?tag={d.name}", vars)
	if err != nil || out != "/devices/7/x/a%20b%2Fc%3Fd%23e?tag=a+b%2Fc%3Fd%23e" {
		t.Fatalf("%q %v", out, err)
	}
	body, err := fill(`{"id":"{d.q}"}`, vars, jsonEscape, false)
	var m map[string]any
	if err != nil || json.Unmarshal([]byte(body), &m) != nil || m["id"] != vars["d.q"] || len(m) != 1 {
		t.Fatalf("body %q %v", body, err)
	}
	if _, err := fill("/{nope.id}", vars, nil, true); err == nil || errors.Is(err, ErrNoValue) {
		t.Fatalf("unknown prefix: %v", err)
	}
	if _, err := fill("/{d.missing}", vars, nil, true); !errors.Is(err, ErrNoValue) {
		t.Fatalf("missing field: %v", err)
	}
	if _, err := fillPath("/users/{d.null}/orders", vars); !errors.Is(err, ErrNoValue) {
		t.Fatalf("null in path: %v", err)
	}
	if out, err := fill("q={d.null}", vars, nil, false); err != nil || out != "q=" {
		t.Fatalf("null in query: %q %v", out, err)
	}
	if out, err := fill(`{"since":"{watermark}"}`, nil, jsonEscape, false); err != nil || out != `{"since":""}` {
		t.Fatalf("absent watermark: %q %v", out, err)
	}
	if out, _ := fill("/plain", nil, nil, true); out != "/plain" {
		t.Fatal("no placeholders")
	}
}

func decodeBody(t *testing.T, s string) any {
	t.Helper()
	var body any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestRecordsAndFlatten(t *testing.T) {
	body := decodeBody(t, `{"data":{"items":[{"id":1,"a":{"b":{"c":"deep"}},"list":[1,{"x":"<&>"}],"empty":{}},{"id":2.5}]},"one":{"x":1}}`)
	recs, _, err := toRecords(body.(map[string]any)["data"].(map[string]any)["items"], nil)
	if err != nil || len(recs) != 2 {
		t.Fatalf("%v %v", recs, err)
	}
	if recs[0]["a_b_c"] != "deep" || recs[0]["list"] != JSONText(`[1,{"x":"<&>"}]`) || recs[0]["id"] != int64(1) {
		t.Fatalf("flatten: %+v", recs[0])
	}
	var back []any
	if b, err := json.Marshal(recs[0]["list"]); err != nil || json.Unmarshal(b, &back) != nil || len(back) != 2 {
		t.Fatalf("JSONText marshals as JSON: %s %v", b, err)
	}
	if v, ok := recs[0]["empty"]; !ok || v != nil {
		t.Fatalf("empty object should be a null column: %+v", recs[0])
	}
	if recs[1]["id"] != 2.5 {
		t.Fatalf("float: %+v", recs[1])
	}
	whole, _, _ := toRecords(body, nil)
	if len(whole) != 1 {
		t.Fatal("an object is one record")
	}
	scalar, _, _ := toRecords("hi", nil)
	if scalar[0]["value"] != "hi" {
		t.Fatalf("scalar: %+v", scalar)
	}
	nested, _, _ := toRecords(decodeBody(t, `[[1,2]]`), nil)
	if nested[0]["value"] != JSONText("[1,2]") {
		t.Fatalf("array element: %+v", nested)
	}
	if _, _, err := toRecords(decodeBody(t, `{"a_b":1,"a":{"b":2}}`), nil); err == nil || !strings.Contains(err.Error(), `"a_b"`) {
		t.Fatalf("collision: %v", err)
	}
}

func TestKeyOrder(t *testing.T) {
	data, order, err := decode([]byte(`{"results":[{"name":"a","id":1,"geo":{"lng":2,"lat":3},"name":"b"},{"zz":1,"id":2,"extra":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	recs, keys, err := toRecords(data.(map[string]any)["results"], order)
	if err != nil || len(recs) != 2 || recs[0]["name"] != "b" {
		t.Fatalf("%v %v", recs, err)
	}
	if got := strings.Join(keys, ","); got != "name,id,geo_lng,geo_lat,zz,extra" {
		t.Fatalf("keys: %s", got)
	}
	_, sorted, _ := toRecords(decodeBody(t, `[{"b":1,"a":2}]`), nil)
	if strings.Join(sorted, ",") != "a,b" {
		t.Fatalf("without order: %v", sorted)
	}
	merged := mergeKeys([]string{"x", "y"}, []string{"y", "z"})
	if strings.Join(merged, ",") != "x,y,z" {
		t.Fatal(merged)
	}
	if _, _, err := decode([]byte(strings.Repeat("[", maxDepth+2))); err == nil {
		t.Fatal("deep nesting accepted")
	}
}

func TestNumbers(t *testing.T) {
	for in, want := range map[string]any{
		"1":                         int64(1),
		"-9223372036854775808":      int64(-9223372036854775808),
		"9.99":                      9.99,
		"1.50":                      1.5,
		"1e2":                       100.0,
		"18446744073709551616":      json.Number("18446744073709551616"),
		"12345678901234567890.123":  json.Number("12345678901234567890.123"),
		"0.1000000000000000000001":  json.Number("0.1000000000000000000001"),
		"1e400":                     json.Number("1e400"),
		"1e-999999999":              json.Number("1e-999999999"),
		"123456789.123456789123456": json.Number("123456789.123456789123456"),
	} {
		if got := number(json.Number(in)); got != want {
			t.Errorf("%s: got %#v want %#v", in, got, want)
		}
	}
}

func TestLinkNext(t *testing.T) {
	for _, tc := range []struct {
		h    []string
		want string
	}{
		{[]string{`<https://x/api?page=3>; rel="next", <https://x/api?page=9>; rel="last"`}, "https://x/api?page=3"},
		{[]string{`<https://s/p.json?fields=id,title&page_info=abc>; rel="next"`}, "https://s/p.json?fields=id,title&page_info=abc"},
		{[]string{`<https://s/a?x=1,2>; rel="previous", <https://s/b?y=3,4>; rel="next"`}, "https://s/b?y=3,4"},
		{[]string{`</items?page=2>; title="a, b"; rel="prefetch next"`}, "/items?page=2"},
		{[]string{`<https://x/1>; rel="prev"`, `<https://x/2>; REL=next`}, "https://x/2"},
		{[]string{`<https://x>; rel="prev"`}, ""},
		{nil, ""},
	} {
		if got := linkNext(tc.h); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.h, got, tc.want)
		}
	}
}

// pagingServer serves 7 items, n=0..6, under several paging styles.
// Pages hold at most 3 items whatever size is asked for.
func pagingServer(t *testing.T) *httptest.Server {
	t.Helper()
	const total, most = 7, 3
	size := func(r *http.Request) int {
		n, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || n > most {
			return most
		}
		return n
	}
	slice := func(off, n int) []map[string]any { return items(min(off, total), min(off+n, total)) }
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		writeJSON(w, map[string]any{"data": slice((p-1)*size(r), size(r))})
	})
	mux.HandleFunc("/offset", func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("start"))
		writeJSON(w, slice(off, size(r)))
	})
	mux.HandleFunc("/cursor", func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("after"))
		resp := map[string]any{"items": slice(off, size(r))}
		if off+size(r) < total {
			resp["next"] = strconv.Itoa(off + size(r))
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/cursor-url", func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("o"))
		resp := map[string]any{"items": slice(off, most)}
		if off+most < total {
			resp["next"] = fmt.Sprintf("%s/cursor-url?o=%d", srv.URL, off+most)
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/link", func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("o"))
		if off+most < total {
			w.Header().Add("Link", `<https://elsewhere/first>; rel="first"`)
			w.Header().Add("Link", fmt.Sprintf(`<link?o=%d&fields=id,title>; rel="next"`, off+most))
		}
		writeJSON(w, slice(off, most))
	})
	// has_more, isLast and total, with offset paging by 2
	mux.HandleFunc("/flags", func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		writeJSON(w, map[string]any{"values": slice(off, 2), "has_more": off+2 < total, "isLast": off+2 >= total, "total": total})
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Query != "{ q }" {
			http.Error(w, "bad body", 400)
			return
		}
		off := 0
		if a, ok := req.Variables["after"].(string); ok {
			off, _ = strconv.Atoi(a)
		}
		n := int(req.Variables["first"].(float64))
		info := map[string]any{"hasNextPage": off+n < total, "endCursor": strconv.Itoa(off + n)}
		writeJSON(w, map[string]any{"data": map[string]any{"nodes": slice(off, n), "pageInfo": info}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchKinds(t *testing.T) {
	srv := pagingServer(t)
	all := "0,1,2,3,4,5,6"
	for _, tc := range []struct {
		name  string
		s     config.Stream
		o     Options
		want  string
		pages int
	}{
		{"page capped quietly", config.Stream{Path: "/page", Records: "data", Paginate: "page", PageSize: 500}, Options{}, all, 3},
		{"page exact size", config.Stream{Path: "/page", Records: "data", Paginate: "page", PageSize: 2}, Options{}, all, 4},
		{"offset", config.Stream{Path: "/offset", Paginate: "offset", PageParam: "start", PageSize: 3}, Options{}, all, 3},
		{"cursor token", config.Stream{Path: "/cursor", Records: "items", Paginate: "cursor", CursorField: "next", CursorParam: "after", PageSize: 2}, Options{}, all, 4},
		{"cursor url", config.Stream{Path: "/cursor-url", Records: "items", Paginate: "cursor", CursorField: "next"}, Options{}, all, 3},
		{"relative link", config.Stream{Path: "/link", Paginate: "link"}, Options{}, all, 3},
		{"max pages", config.Stream{Path: "/offset", Paginate: "offset", PageParam: "start"}, Options{MaxPages: 2}, "0,1,2,3,4,5", 2},
		{"stop after", config.Stream{Path: "/offset", Paginate: "offset", PageParam: "start"}, Options{StopAfter: func(p []Record) bool { return p[0]["n"] == int64(3) }}, "0,1,2,3,4,5", 2},
		{"has_more", config.Stream{Path: "/flags", Records: "values", Paginate: "offset", StopWhen: "!has_more"}, Options{}, all, 4},
		{"isLast", config.Stream{Path: "/flags", Records: "values", Paginate: "offset", StopWhen: "isLast"}, Options{}, all, 4},
		{"total", config.Stream{Path: "/flags", Records: "values", Paginate: "offset", TotalField: "total"}, Options{}, all, 4},
		{"numeric stop_when", config.Stream{Path: "/flags", Records: "values", Paginate: "offset", StopWhen: "total >= `4`"}, Options{}, "0,1", 1},
		{"graphql cursor in body", config.Stream{
			Path: "/graphql", Method: "post", Body: `{"query": "{ q }"}`, Records: "data.nodes",
			Paginate: "cursor", CursorField: "data.pageInfo.endCursor", CursorParam: "variables.after",
			SizeParam: "variables.first", PageSize: 3, ParamsIn: "body", StopWhen: "!(data.pageInfo.hasNextPage)",
		}, Options{}, all, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := testClient(t, &config.Conn{Type: "api", BaseURL: srv.URL})
			pages := 0
			tc.o.OnPage = func(int) { pages++ }
			recs, err := c.FetchWith(context.Background(), &tc.s, tc.o)
			if err != nil {
				t.Fatal(err)
			}
			if got := ns(recs); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
			if pages != tc.pages {
				t.Fatalf("%d pages, want %d", pages, tc.pages)
			}
		})
	}
}

func TestFetchClientDefaults(t *testing.T) {
	srv := pagingServer(t)
	c, _ := testClient(t, &config.Conn{Type: "api", BaseURL: srv.URL + "/"})
	c.MaxPages = 1
	recs, err := c.Fetch(context.Background(), &config.Stream{Path: "/offset", Paginate: "offset", PageParam: "start"}, nil)
	if err != nil || ns(recs) != "0,1,2" {
		t.Fatalf("%v %v", ns(recs), err)
	}
}

func TestFetchStops(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/same-cursor", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeJSON(w, map[string]any{"results": items(0, 2), "paging": map[string]any{"after": "C1"}})
	})
	mux.HandleFunc("/ignores-page", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeJSON(w, items(0, 3))
	})
	mux.HandleFunc("/clamps", func(w http.ResponseWriter, r *http.Request) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		writeJSON(w, items(min(p, 2)*3-3, min(p, 2)*3))
	})
	mux.HandleFunc("/envelope", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"results": items(0, 2), "count": 2})
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "deleted", 404) })
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, _ := testClient(t, &config.Conn{Type: "api", BaseURL: srv.URL})
	ctx := context.Background()

	_, err := c.Fetch(ctx, &config.Stream{Path: "/same-cursor", Records: "results", Paginate: "cursor", CursorField: "paging.after"}, nil)
	if err == nil || !strings.Contains(err.Error(), "came back twice") || hits.Load() != 2 {
		t.Fatalf("repeated cursor: %v after %d hits", err, hits.Load())
	}
	hits.Store(0)
	_, err = c.Fetch(ctx, &config.Stream{Path: "/ignores-page", Paginate: "page", PageParam: "pg"}, nil)
	if err == nil || !strings.Contains(err.Error(), "ignores pg") || hits.Load() != 2 {
		t.Fatalf("repeated page: %v after %d hits", err, hits.Load())
	}
	recs, err := c.Fetch(ctx, &config.Stream{Path: "/clamps", Paginate: "page"}, nil)
	if err != nil || ns(recs) != "0,1,2,3,4,5" {
		t.Fatalf("clamped last page: %s %v", ns(recs), err)
	}
	if _, err := c.Fetch(ctx, &config.Stream{Path: "/envelope", Paginate: "page"}, nil); err == nil || !strings.Contains(err.Error(), "records") {
		t.Fatalf("envelope object with paginate: %v", err)
	}
	if recs, err := c.Fetch(ctx, &config.Stream{Path: "/envelope"}, nil); err != nil || len(recs) != 1 {
		t.Fatalf("envelope without paginate is one record: %v %v", recs, err)
	}
	var he *HTTPError
	if _, err := c.Fetch(ctx, &config.Stream{Path: "/gone"}, nil); !errors.As(err, &he) || he.Status != 404 || he.Body != "deleted " {
		t.Fatalf("404: %#v", err)
	}
	if recs, err := c.Fetch(ctx, &config.Stream{Path: "/gone", SkipOn: []int{404, 409}}, nil); err != nil || len(recs) != 0 {
		t.Fatalf("skip_on: %v %v", recs, err)
	}
	if recs, err := c.Fetch(ctx, &config.Stream{Path: "/empty", Paginate: "page"}, nil); err != nil || len(recs) != 0 {
		t.Fatalf("empty body: %v %v", recs, err)
	}
	for _, s := range []config.Stream{
		{},
		{Path: "/x", Paginate: "cursor"},
		{Path: "/x", Paginate: "seek"},
		{Path: "/x", ParamsIn: "header"},
		{Path: "/x", Records: "data.["},
		{Path: "/x", StopWhen: "a.["},
		{Path: "/x", Paginate: "page", ParamsIn: "body", Body: "[1]"},
	} {
		if _, err := c.Fetch(ctx, &s, nil); err == nil {
			t.Errorf("%+v should fail", s)
		}
	}
}

// echo returns the request as one record.
func echo(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	writeJSON(w, map[string]any{
		"method": r.Method, "path": r.URL.EscapedPath(), "query": r.URL.RawQuery, "body": string(b),
		"auth": r.Header.Get("Authorization"), "key": r.Header.Get("X-Api-Key"), "custom": r.Header.Get("X-Token"),
		"version": r.Header.Get("X-Api-Version"), "accept": r.Header.Get("Accept"), "ctype": r.Header.Get("Content-Type"),
	})
}

func TestAuthAndRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(echo))
	defer srv.Close()
	ctx := context.Background()
	get := func(c *config.Conn, s *config.Stream, vars map[string]string) Record {
		t.Helper()
		cl, _ := testClient(t, c)
		recs, err := cl.Fetch(ctx, s, vars)
		if err != nil || len(recs) != 1 {
			t.Fatalf("%v %v", recs, err)
		}
		return recs[0]
	}
	plain := &config.Stream{Path: "/x"}
	if r := get(&config.Conn{Type: "api", BaseURL: srv.URL, Auth: "bearer", Token: "T1"}, plain, nil); r["auth"] != "Bearer T1" {
		t.Errorf("bearer: %v", r)
	}
	if r := get(&config.Conn{Type: "api", BaseURL: srv.URL, Auth: "basic", User: "u", Password: "p"}, plain, nil); r["auth"] != "Basic dTpw" {
		t.Errorf("basic: %v", r)
	}
	if r := get(&config.Conn{Type: "api", BaseURL: srv.URL, Auth: "header", Token: "K"}, plain, nil); r["key"] != "K" {
		t.Errorf("header default: %v", r)
	}
	if r := get(&config.Conn{Type: "api", BaseURL: srv.URL, Auth: "header", Header: "X-Token", Token: "K"}, plain, nil); r["custom"] != "K" {
		t.Errorf("header custom: %v", r)
	}
	r := get(&config.Conn{Type: "api", BaseURL: srv.URL, Auth: "query", Param: "api_key", Token: "Q"},
		&config.Stream{Path: "/x?expand[]=a&expand[]=b", Query: map[string]string{"q": "{watermark}", "f": "1"}}, nil)
	if r["query"] != "api_key=Q&expand%5B%5D=a&expand%5B%5D=b&f=1" {
		t.Errorf("query auth, repeated keys, absent watermark: %v", r["query"])
	}
	r = get(&config.Conn{Type: "api", BaseURL: srv.URL, Headers: map[string]string{"X-Api-Version": "2022-11-28", "Accept": "application/vnd.x+json"}}, plain, nil)
	if r["version"] != "2022-11-28" || r["accept"] != "application/vnd.x+json" {
		t.Errorf("static headers: %v", r)
	}
	r = get(&config.Conn{Type: "api", BaseURL: srv.URL},
		&config.Stream{Path: "/p/{p.id}", Method: "POST", Body: `{"id": "{p.name}", "since": "{watermark}"}`, Query: map[string]string{"s": "{watermark}"}},
		map[string]string{"p.id": "a/b c", "p.name": `x"y`, "watermark": "2026-01-01T00:00:00Z"})
	if r["method"] != "POST" || r["path"] != "/p/a%2Fb%20c" || r["ctype"] != "application/json" || r["query"] != "s=2026-01-01T00%3A00%3A00Z" {
		t.Errorf("post: %v", r)
	}
	var body map[string]string
	if json.Unmarshal([]byte(r["body"].(string)), &body) != nil || body["id"] != `x"y` || body["since"] != "2026-01-01T00:00:00Z" {
		t.Errorf("post body: %v", r["body"])
	}
	cl, _ := testClient(t, &config.Conn{Type: "api", BaseURL: srv.URL})
	if _, err := cl.Fetch(ctx, &config.Stream{Path: "/p/{p.id}"}, map[string]string{"p.name": "x"}); !errors.Is(err, ErrNoValue) {
		t.Errorf("missing field: %v", err)
	}
}

func TestOtherHostGetsNoCredentials(t *testing.T) {
	var evilSaw atomic.Value
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilSaw.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("X-Api-Version") + "|" + r.URL.RawQuery)
		writeJSON(w, map[string]any{"items": items(5, 6)})
	}))
	defer evil.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"items": items(0, 1), "next": evil.URL + "/steal?x=1"})
	})
	mux.HandleFunc("/link", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "<"+evil.URL+"/steal>; rel=next")
		writeJSON(w, map[string]any{"items": items(0, 1)})
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	})
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/landed", http.StatusFound) })
	mux.HandleFunc("/landed", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, items(9, 10)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	for _, auth := range []config.Conn{
		{Auth: "bearer", Token: "SECRET"},
		{Auth: "query", Param: "api_key", Token: "SECRET"},
	} {
		conn := auth
		conn.Type, conn.BaseURL, conn.Headers = "api", srv.URL, map[string]string{"X-Api-Version": "1"}
		c, _ := testClient(t, &conn)
		for _, s := range []config.Stream{
			{Path: "/next", Records: "items", Paginate: "cursor", CursorField: "next"},
			{Path: "/link", Records: "items", Paginate: "link"},
		} {
			evilSaw.Store("")
			recs, err := c.Fetch(ctx, &s, nil)
			if err != nil || ns(recs) != "0,5" {
				t.Fatalf("%s %s: %v %v", conn.Auth, s.Path, ns(recs), err)
			}
			if saw := evilSaw.Load().(string); saw == "" || strings.Contains(saw, "SECRET") || strings.Contains(saw, "|1|") {
				t.Fatalf("%s %s: other host saw %q", conn.Auth, s.Path, saw)
			}
		}
		_, err := c.Fetch(ctx, &config.Stream{Path: "/redirect"}, nil)
		if !errors.Is(err, errRedirect) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("redirect to other host: %v", err)
		}
		if recs, err := c.Fetch(ctx, &config.Stream{Path: "/hop"}, nil); err != nil || ns(recs) != "9" {
			t.Fatalf("same-host redirect: %v %v", recs, err)
		}
	}
	c, _ := testClient(t, &config.Conn{Type: "api", BaseURL: srv.URL, Auth: "query", Param: "api_key", Token: "SECRET"})
	u, _ := url.Parse(evil.URL + "/x?api_key=SECRET&y=2")
	if got := c.withToken(u, false); got.RawQuery != "y=2" {
		t.Fatalf("token kept for other host: %s", got)
	}
}

func TestRetry(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/flaky", func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			http.Error(w, "later", 503)
		case 2:
			w.Header().Set("Retry-After", "2.5")
			http.Error(w, "later", 429)
		case 3:
			http.Error(w, "later", 502)
		default:
			writeJSON(w, items(0, 1))
		}
	})
	mux.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad key "+r.URL.Query().Get("api_key")+"\x1b]52;c;x\x07", 503)
	})
	mux.HandleFunc("/forbidden", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no", 403)
	})
	mux.HandleFunc("/secondary", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "secondary rate limit", 403)
			return
		}
		writeJSON(w, items(0, 1))
	})
	mux.HandleFunc("/long", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "later", 429)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	run := func(path string) (*clock, error) {
		calls.Store(0)
		c, ck := testClient(t, &config.Conn{Name: path, Type: "api", BaseURL: srv.URL, Auth: "query", Param: "api_key", Token: "SECRET"})
		_, err := c.Fetch(ctx, &config.Stream{Path: path}, nil)
		return ck, err
	}

	ck, err := run("/flaky")
	if w := ck.waits(); err != nil || len(w) != 2 || w[0] != 2500*time.Millisecond || w[1] < 2*time.Second || w[1] >= 4*time.Second {
		t.Fatalf("Retry-After 0, 2.5 then backoff: %v %v", w, err)
	}

	ck, err = run("/down")
	if err == nil || calls.Load() != attempts || len(ck.waits()) != attempts-1 {
		t.Fatalf("want %d attempts and %d waits: %d %v %v", attempts, attempts-1, calls.Load(), ck.waits(), err)
	}
	if msg := err.Error(); strings.Contains(msg, "SECRET") || !strings.Contains(msg, "api_key=xxxxx") || strings.ContainsAny(msg, "\x1b\x07") {
		t.Fatalf("error text: %q", msg)
	}
	for i, d := range ck.waits() {
		lo := min(time.Second<<i, maxBackoff) / 2
		if d < lo || d >= 2*lo {
			t.Fatalf("backoff %d: %v not in [%v, %v)", i, d, lo, 2*lo)
		}
	}

	if _, err := run("/forbidden"); err == nil || calls.Load() != 1 {
		t.Fatalf("plain 403 is final: %d %v", calls.Load(), err)
	}
	if ck, err := run("/secondary"); err != nil || calls.Load() != 2 || ck.waits()[0] != time.Minute {
		t.Fatalf("403 with Retry-After: %d %v %v", calls.Load(), ck.waits(), err)
	}
	if _, err := run("/long"); err == nil || calls.Load() != 1 || !strings.Contains(err.Error(), "wait 1h0m0s") {
		t.Fatalf("Retry-After over the cap: %d %v", calls.Load(), err)
	}
}

func TestTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	c, ck := testClient(t, &config.Conn{Type: "api", BaseURL: base, Auth: "query", Param: "k", Token: "SECRET"})
	_, err := c.Fetch(context.Background(), &config.Stream{Path: "/x"}, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") || len(ck.waits()) != attempts-1 {
		t.Fatalf("%v %v", ck.waits(), err)
	}
	if transient(&net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}) {
		t.Error("unknown host is final")
	}
	if transient(fmt.Errorf("x: %w", errRedirect)) || !transient(io.ErrUnexpectedEOF) {
		t.Error("transient")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, ck = testClient(t, &config.Conn{Type: "api", BaseURL: base})
	if _, err := c.Fetch(ctx, &config.Stream{Path: "/x"}, nil); err == nil || len(ck.waits()) != 0 {
		t.Fatalf("cancelled: %v %v", ck.waits(), err)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		h    http.Header
		want time.Duration
		ok   bool
	}{
		{http.Header{"Retry-After": {"7"}}, 7 * time.Second, true},
		{http.Header{"Retry-After": {"2.0"}}, 2 * time.Second, true},
		{http.Header{"Retry-After": {now.Add(90 * time.Second).Format(http.TimeFormat)}}, 90 * time.Second, true},
		{http.Header{"Retry-After": {now.Add(-time.Hour).Format(http.TimeFormat)}}, 0, true},
		{http.Header{"Retry-After": {"1e999"}}, 0, false},
		{http.Header{"Retry-After": {"Inf"}}, 24 * time.Hour, true},
		{http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Unix()+30, 10)}}, 30 * time.Second, true},
		{http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.UnixMilli()+1500, 10)}}, 1500 * time.Millisecond, true},
		{http.Header{"Ratelimit-Remaining": {"0"}, "Ratelimit-Reset": {"12"}}, 12 * time.Second, true},
		{http.Header{"X-Ratelimit-Remaining": {"3"}, "X-Ratelimit-Reset": {"12"}}, 0, false},
		{http.Header{}, 0, false},
	} {
		d, ok := retryAfter(tc.h, now)
		if d != tc.want || ok != tc.ok {
			t.Errorf("%v: got %v %v", tc.h, d, ok)
		}
	}
}

func TestRateLimitHeadersPause(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Unix()+120, 10))
		}
		writeJSON(w, items(0, 1))
	}))
	defer srv.Close()
	conn := &config.Conn{Type: "api", BaseURL: srv.URL}
	a, _ := testClient(t, conn)
	b, ck := testClient(t, conn)
	if a.shared != b.shared {
		t.Fatal("clients of one connection share limits")
	}
	if _, err := a.Fetch(context.Background(), &config.Stream{Path: "/x"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Fetch(context.Background(), &config.Stream{Path: "/x"}, nil); err != nil {
		t.Fatal(err)
	}
	if w := ck.waits(); len(w) != 1 || w[0] < 100*time.Second {
		t.Fatalf("second client should wait for the reset: %v", w)
	}
	other, _ := testClient(t, &config.Conn{Name: conn.Name, Type: "api", BaseURL: srv.URL, RateLimit: "10/s"})
	if other.shared == a.shared || other.shared.limiter == nil {
		t.Fatal("a changed rate_limit gets its own limiter")
	}
	c, _ := testClient(t, &config.Conn{Type: "api", BaseURL: srv.URL + "/far"})
	c.shared.pause(time.Now().Add(time.Hour))
	if _, err := c.Fetch(context.Background(), &config.Stream{Path: "/x"}, nil); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("pause over the cap: %v", err)
	}
}

func TestBodyLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, items(0, 100))
	}))
	defer srv.Close()
	c, _ := testClient(t, &config.Conn{Type: "api", BaseURL: srv.URL})
	c.maxBody = 64
	if _, err := c.Fetch(context.Background(), &config.Stream{Path: "/x"}, nil); !errors.Is(err, errBodyLimit) || calls.Load() != 1 {
		t.Fatalf("%d %v", calls.Load(), err)
	}
}

func TestNewClient(t *testing.T) {
	ok, err := NewClient(&config.Conn{Name: "a", Type: "api", BaseURL: "https://x", Timeout: "15s"})
	if err != nil || ok.http.Timeout != 15*time.Second {
		t.Fatalf("%v", err)
	}
	if def, _ := NewClient(&config.Conn{Name: "a", Type: "api", BaseURL: "https://x"}); def.http.Timeout != defaultTimeout {
		t.Fatal("default timeout")
	}
	for _, c := range []config.Conn{
		{},
		{BaseURL: "example.com"},
		{BaseURL: "ftp://x"},
		{BaseURL: "https://x", Auth: "oauth"},
		{BaseURL: "https://x", Auth: "query"},
		{BaseURL: "https://x", Timeout: "soon"},
		{BaseURL: "https://x", Timeout: "-1s"},
		{BaseURL: "https://x", RateLimit: "fast"},
		{BaseURL: "https://x", Headers: map[string]string{"Bad Name": "v"}},
		{BaseURL: "https://x", Headers: map[string]string{"X-A": "v\r\nX-B: w"}},
	} {
		c.Name = "bad"
		if _, err := NewClient(&c); err == nil {
			t.Errorf("%+v should fail", c)
		}
	}
}
