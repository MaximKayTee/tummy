package engine

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tummy/internal/config"
	"tummy/internal/db"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

// TestSourceQueryGolden renders source queries without steps per dialect into testdata/source_<dialect>.sql.
func TestSourceQueryGolden(t *testing.T) {
	cases := []struct {
		label string
		s     config.Stream
		wm    any
	}{
		{"full", config.Stream{Mode: "full-refresh"}, nil},
		{"select where limit", config.Stream{Mode: "full-refresh", Select: []string{"id", "at"}, Where: "id > 0", Limit: 10}, nil},
		{"incremental first run", config.Stream{Mode: "incremental", UpdateKey: "at", PrimaryKey: []string{"id"}}, nil},
		{"incremental with key", config.Stream{Mode: "incremental", UpdateKey: "at", PrimaryKey: []string{"id"}}, "2026-01-01"},
		{"incremental append", config.Stream{Mode: "incremental", UpdateKey: "at"}, "2026-01-01"},
		{"incremental limit", config.Stream{Mode: "incremental", UpdateKey: "at", Limit: 100, Where: "id > 0"}, "2026-01-01"},
		{"backfill", config.Stream{Mode: "backfill", UpdateKey: "at", Range: "2026-01-01, 2026-02-01"}, nil},
		{"sql where", config.Stream{Mode: "incremental", UpdateKey: "at", SQL: "select * from t order by at;", Where: "id > 0"}, "2026-01-01"},
		{"placeholder twice", config.Stream{Mode: "incremental", UpdateKey: "at", SQL: "select * from a where {incremental_where_cond} union all select * from b where {incremental_where_cond}"}, "2026-01-01"},
		{"placeholder first run", config.Stream{Mode: "incremental", UpdateKey: "at", SQL: "select * from a where {incremental_where_cond}"}, nil},
		{"cte", config.Stream{Mode: "incremental", UpdateKey: "at", SQL: "with x as (select * from t) select * from x"}, "2026-01-01"},
	}
	for _, name := range db.Dialects() {
		t.Run(name, func(t *testing.T) {
			dl, _ := db.DialectFor(name)
			src := &db.DB{Dialect: dl}
			var b strings.Builder
			for _, c := range cases {
				s := stream{ResolvedStream: config.ResolvedStream{Name: "s.orders", Stream: c.s}, table: db.Table{Schema: "s", Name: "orders"}}
				q, args, err := sourceQuery(context.Background(), src, s, c.wm)
				if err != nil {
					t.Fatalf("%s: %v", c.label, err)
				}
				fmt.Fprintf(&b, "-- %s %v\n%s\n\n", c.label, args, q)
			}
			golden(t, filepath.Join("testdata", "source_"+name+".sql"), b.String())
		})
	}
}

func golden(t *testing.T, path, got string) {
	t.Helper()
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
		t.Errorf("%s differs; go test ./internal/engine -run Golden -update, then review the diff\n%s", path, got)
	}
}

func TestAuxTable(t *testing.T) {
	long := db.Table{Schema: "s", Name: strings.Repeat("é", 40)}
	a := auxTable(long, "tmp", "rep1")
	if len(a.Name) > 63 || !strings.HasPrefix(a.Name, "_tummy_tmp_") || a.Schema != "s" {
		t.Fatalf("%q %d", a.Name, len(a.Name))
	}
	if auxTable(long, "tmp", "rep2") == a {
		t.Fatal("replications share a temp table")
	}
	if auxTable(long, "tmp", "rep1") != a {
		t.Fatal("not stable")
	}
}

func TestFormatWatermark(t *testing.T) {
	ts := time.Date(2026, 7, 1, 12, 0, 0, 0, time.FixedZone("BST", 3600))
	cases := []struct {
		v      any
		format string
		want   string
	}{
		{int64(123456789), "", "123456789"}, // an id, not 1973
		{int64(1700000000), "", "1700000000"},
		{"1700000000", "", "1700000000"},
		{1.5, "", "1.5"},
		{ts, "", "2026-07-01T11:00:00Z"},
		{"2026-07-01 12:00:00+00:00", "", "2026-07-01T12:00:00Z"},
		{int64(1700000000), "rfc3339", "2023-11-14T22:13:20Z"},
		{ts, "unix", "1782903600"},
		{ts, "date", "2026-07-01"},
		{"abc", "", "abc"},
	}
	for _, c := range cases {
		if got := formatWatermark(c.v, c.format); got != c.want {
			t.Errorf("%v %q: %q, want %q", c.v, c.format, got, c.want)
		}
	}
}
