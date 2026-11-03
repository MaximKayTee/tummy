package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tummy/internal/api"
	"tummy/internal/config"
	"tummy/internal/db"
)

func TestCancelRequest(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\n[streams.customers]\n[streams.orders]\n")
	e := f.engine()
	e.CancelPoll = 5 * time.Millisecond
	first := ""
	e.OnEvent = func(ev Event) {
		if ev.Phase != PhaseLoading || first != "" {
			return
		}
		first = ev.Stream
		runs, _ := f.st.RunsFor(ctx, r.Name, ev.Stream, 1)
		if err := f.st.Cancel(ctx, runs[0].ID); err != nil {
			t.Error(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	res, err := e.Run(ctx, r, nil)
	if !errors.Is(err, ErrCancelled) || len(res) != 1 || !errors.Is(res[0].Err, ErrCancelled) {
		t.Fatalf("%v %+v", err, res)
	}
	runs, _ := f.st.RunsFor(ctx, r.Name, "", 10)
	if len(runs) != 1 || runs[0].Stream != first || runs[0].Status != "cancelled" || runs[0].Error != "" {
		t.Fatalf("one cancelled row, no later stream: %+v", runs)
	}
	if f.count("SELECT count(*) FROM sqlite_master WHERE name LIKE '_tummy_tmp_%' OR name = '"+first+"'") != 0 {
		t.Fatal("temp table or target left")
	}
	if err := f.st.Cancel(ctx, runs[0].ID); err == nil {
		t.Fatal("cancel of a finished run")
	}
}

func TestInterruptRecordsCancelled(t *testing.T) {
	f := setup(t)
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\n[streams.customers]\n[streams.orders]\n")
	ctx, cancel := context.WithCancel(context.Background())
	e := f.engine()
	e.OnEvent = func(ev Event) {
		if ev.Phase == PhaseLoading {
			cancel()
		}
	}
	res, err := e.Run(ctx, r, nil)
	if !errors.Is(err, ErrCancelled) || len(res) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	runs, _ := f.st.RunsFor(context.Background(), r.Name, "", 10)
	if len(runs) != 1 || runs[0].Status != "cancelled" || runs[0].Error != "interrupted" {
		t.Fatalf("%+v", runs)
	}
	// cancelled before any stream: the replication row
	if _, err := e.Run(ctx, r, nil); !errors.Is(err, ErrCancelled) {
		t.Fatal(err)
	}
	runs, _ = f.st.RunsFor(context.Background(), r.Name, "*", 1)
	if len(runs) != 1 || runs[0].Status != "cancelled" {
		t.Fatalf("%+v", runs)
	}
}

func TestAPIColumnOrder(t *testing.T) {
	f := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"zeta":1,"alpha":"a","mid":{"y":1,"x":2}},{"zeta":2,"extra":true,"alpha":"b"}]}`))
	}))
	defer srv.Close()
	f.conns.Conn["API"] = &config.Conn{Name: "API", Type: "api", BaseURL: srv.URL}
	r := f.replication("source = \"API\"\ntarget = \"TGT\"\n[streams.things]\npath = \"/t\"\nrecords = \"data[]\"\nadd_columns = { b_added = \"x\", a_added = \"y\" }\n[streams.picked]\npath = \"/t\"\nrecords = \"data[]\"\nselect = [\"alpha\", \"zeta\"]\n")
	f.run(f.engine(), r)
	cols := func(table string) string {
		var s string
		f.tgt.QueryRow("SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_table_info('" + table + "') ORDER BY cid)").Scan(&s)
		return s
	}
	if got := cols("things"); got != "zeta,alpha,mid_y,mid_x,extra,a_added,b_added" {
		t.Fatalf("response order: %s", got)
	}
	if got := cols("picked"); got != "alpha,zeta" {
		t.Fatalf("select order: %s", got)
	}

	client, err := api.NewClient(f.conns.Conn["API"])
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	recs, err := client.FetchWith(context.Background(), &config.Stream{Path: "/t", Records: "data[]"}, api.Options{Keys: &keys})
	if err != nil {
		t.Fatal(err)
	}
	tc, _ := Tabulate(recs, keys...)
	var names []string
	for _, c := range tc {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "zeta,alpha,mid_y,mid_x,extra" {
		t.Fatalf("Tabulate: %v", names)
	}
}

func TestRunQueryAndWatermark(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r := f.replication("source = \"SRC\"\ntarget = \"TGT\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\n[streams.orders]\nupdate_key = \"updated_at\"\nlimit = 10\nobject = \"o_{stream_table}\"\n")
	src, err := db.Open(ctx, f.conns.Conn["SRC"].DB())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	tp := f.conns.Conn["TGT"].DB()
	tp.Create = true
	tgt, err := db.Open(ctx, tp)
	if err != nil {
		t.Fatal(err)
	}
	defer tgt.Close()
	s := r.Resolved[0]
	if got := TargetTable(&db.DB{Dialect: tgt.Dialect}, s).String(); got != "o_orders" {
		t.Fatalf("target: %s", got)
	}
	wm, err := Watermark(ctx, tgt, s)
	if err != nil || wm != nil {
		t.Fatalf("no target yet: %v %v", wm, err)
	}
	q, args, err := RunQuery(ctx, src, s, wm)
	if err != nil || len(args) != 0 || strings.Contains(q, ">=") {
		t.Fatalf("first run: %s %v %v", q, args, err)
	}
	f.run(f.engine(), r)
	if wm, err = Watermark(ctx, tgt, s); err != nil || wm == nil {
		t.Fatalf("watermark: %v %v", wm, err)
	}
	q, args, err = RunQuery(ctx, src, s, wm)
	if err != nil || len(args) != 1 || !strings.Contains(q, `"updated_at" >= ?`) || !strings.HasSuffix(q, "LIMIT 10") {
		t.Fatalf("next run: %s %v %v", q, args, err)
	}
	if n, err := src.Count(ctx, q, args...); err != nil || n != 1 {
		t.Fatalf("rows at the watermark: %d %v", n, err)
	}
	if _, _, err := RunQuery(ctx, src, config.ResolvedStream{Name: "sales.*"}, nil); err == nil {
		t.Fatal("pattern accepted")
	}
}
