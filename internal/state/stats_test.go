package state

import (
	"context"
	"testing"
	"time"
)

func TestTableStats(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if err := s.SaveTableStats(ctx, "PG", t0, true, []TableStat{
		{Schema: "public", Table: "a", Rows: 100, Bytes: 8192},
		{Schema: "public", Table: "b", Rows: -1, Bytes: 16384},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTableStats(ctx, "PG", t0.Add(time.Hour), true, []TableStat{
		{Schema: "public", Table: "a", Rows: 150, Bytes: 12288},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTableStats(ctx, "PG", t0.Add(2*time.Hour), false, []TableStat{
		{Schema: "public", Table: "b", Rows: 7, Bytes: 16384, Exact: true},
	}); err != nil {
		t.Fatal(err)
	}
	s.SaveTableStats(ctx, "OTHER", t0, true, []TableStat{{Schema: "x", Table: "a", Rows: 1, Bytes: 1}})

	got, err := s.TableStats(ctx, "PG")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	a, b := got[0], got[1]
	if a.Table != "a" || a.Rows != 150 || a.PrevRows != 100 || a.Exact || !a.Refreshed.Equal(t0.Add(time.Hour)) {
		t.Fatalf("a: %+v", a)
	}
	if b.Table != "b" || b.Rows != 7 || !b.Exact || b.PrevRows != -1 {
		t.Fatalf("b: %+v", b)
	}
	cs, err := s.ConnStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// the latest full refresh had only a: b's single count does not count
	if c := cs["PG"]; c.Tables != 1 || c.Bytes != 12288 || !c.Refreshed.Equal(t0.Add(time.Hour)) || len(cs) != 2 {
		t.Fatalf("conn stats: %+v", cs)
	}
}
