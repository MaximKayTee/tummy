package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadReplicationDefaultsAndOverrides(t *testing.T) {
	p := write(t, "sales.toml", `
source = "A"
target = "B"
mode = "incremental"
primary_key = ["id"]
schedule = "*/5 * * * *"

[streams."sales.*"]

[streams."sales.orders"]
update_key = "updated_at"
primary_key = ["order_id"]

[streams.daily]
sql = "select 1"
object = "daily"
mode = "full-refresh"
`)
	r, err := LoadReplication(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "sales" || r.Source != "A" || r.Target != "B" || r.Schedule != "*/5 * * * *" {
		t.Fatalf("top level: %+v", r)
	}
	if !r.IsEnabled() {
		t.Fatal("enabled should default to true")
	}
	if len(r.Resolved) != 3 {
		t.Fatalf("want 3 streams, got %d", len(r.Resolved))
	}
	if r.Resolved[0].Name != "sales.*" || r.Resolved[1].Name != "sales.orders" || r.Resolved[2].Name != "daily" {
		t.Fatalf("file order lost: %v", r.Resolved)
	}
	w := r.Resolved[0]
	if w.Mode != "incremental" || w.PrimaryKey[0] != "id" || w.UpdateKey != "" {
		t.Fatalf("wildcard should take defaults: %+v", w.Stream)
	}
	o := r.Resolved[1]
	if o.UpdateKey != "updated_at" || o.PrimaryKey[0] != "order_id" || o.Mode != "incremental" {
		t.Fatalf("override: %+v", o.Stream)
	}
	d := r.Resolved[2]
	if d.Mode != "full-refresh" || d.SQL != "select 1" || d.Object != "daily" {
		t.Fatalf("sql stream: %+v", d.Stream)
	}
}

func TestLoadReplicationErrors(t *testing.T) {
	cases := map[string]string{
		"missing target": "source = \"A\"\n[streams.x]\n",
		"bad mode":       "source = \"A\"\ntarget = \"B\"\nmode = \"sideways\"\n[streams.x]\n",
		"sql no object":  "source = \"A\"\ntarget = \"B\"\n[streams.x]\nsql = \"select 1\"\n",
		"unknown key":    "source = \"A\"\ntarget = \"B\"\nbogus = 1\n[streams.x]\n",
		"incremental":    "source = \"A\"\ntarget = \"B\"\nmode = \"incremental\"\n[streams.x]\n",
		"backfill":       "source = \"A\"\ntarget = \"B\"\nmode = \"backfill\"\n[streams.x]\n",
		"path and sql":   "source = \"A\"\ntarget = \"B\"\n[streams.x]\npath = \"/x\"\nsql = \"select 1\"\nobject = \"x\"\n",
	}
	for name, body := range cases {
		if _, err := LoadReplication(write(t, "r.toml", body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestExpandEnvString(t *testing.T) {
	t.Setenv("TUMMY_TEST_PW", "s3cret")
	out, err := expandEnv("url = \"pg://u:${TUMMY_TEST_PW}@h/db\"")
	if err != nil || !strings.Contains(out, "u:s3cret@h") {
		t.Fatalf("got %q, %v", out, err)
	}
	if _, err := expandEnv("x = \"${TUMMY_UNSET_VAR_XYZ}\""); err == nil {
		t.Fatal("unset variable should error")
	}
}

func TestLoadConnections(t *testing.T) {
	p := write(t, "connections.toml", `
[conn.PG]
type = "postgres"
url = "postgres://x"

[conn.API]
type = "api"
base_url = "https://example.com"
rate_limit = "10/s"
`)
	c, err := LoadConnections(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Names(); len(got) != 2 || got[0] != "API" || got[1] != "PG" {
		t.Fatalf("names: %v", got)
	}
	if c.Conn["PG"].Name != "PG" {
		t.Fatal("name not set")
	}
	if _, err := c.Get("NOPE"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := LoadConnections(write(t, "c.toml", "[conn.X]\nurl = \"x\"\n")); err == nil {
		t.Fatal("missing type should error")
	}
}

func TestSetEnabled(t *testing.T) {
	// insert when absent, keep blank line before the first header
	p := write(t, "r.toml", "source = \"A\"\ntarget = \"B\"\n\n[streams.x]\nmode = \"snapshot\"\n")
	if err := SetEnabled(p, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "source = \"A\"\ntarget = \"B\"\nenabled = false\n\n[streams.x]\nmode = \"snapshot\"\n"
	if string(got) != want {
		t.Fatalf("insert:\n%q\nwant\n%q", got, want)
	}
	r, err := LoadReplication(p)
	if err != nil || r.IsEnabled() {
		t.Fatalf("after disable: %v %v", err, r)
	}

	// replace in place
	if err := SetEnabled(p, true); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(p)
	want = "source = \"A\"\ntarget = \"B\"\nenabled = true\n\n[streams.x]\nmode = \"snapshot\"\n"
	if string(got) != want {
		t.Fatalf("replace:\n%q\nwant\n%q", got, want)
	}

	// a stream-level "enabled" line must not be touched
	p2 := write(t, "r2.toml", "source = \"A\"\ntarget = \"B\"\n\n[streams.x]\nenabled = true\n")
	if err := SetEnabled(p2, false); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(p2)
	if !strings.Contains(string(got), "enabled = false\n\n[streams.x]\nenabled = true\n") {
		t.Fatalf("stream key touched:\n%s", got)
	}

	// no headers at all
	p3 := write(t, "r3.toml", "source = \"A\"\ntarget = \"B\"\n")
	if err := SetEnabled(p3, false); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(p3)
	if string(got) != "source = \"A\"\ntarget = \"B\"\nenabled = false\n" {
		t.Fatalf("no headers:\n%q", got)
	}
}

func TestListReplicationFiles(t *testing.T) {
	home := t.TempDir()
	if files, err := ListReplicationFiles(home); err != nil || len(files) != 0 {
		t.Fatalf("missing dir: %v %v", files, err)
	}
	dir := ReplicationsDir(home)
	os.MkdirAll(dir, 0o755)
	for _, n := range []string{"b.toml", "a.toml", "notes.txt", ".hidden.toml"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	files, err := ListReplicationFiles(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || filepath.Base(files[0]) != "a.toml" || filepath.Base(files[1]) != "b.toml" {
		t.Fatalf("got %v", files)
	}
}

func TestLoadConnectionsMissingFile(t *testing.T) {
	c, err := LoadConnections(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || len(c.Conn) != 0 || len(c.Names()) != 0 {
		t.Fatalf("missing file should be empty: %v %v", c, err)
	}
	if _, err := c.Get("X"); err == nil {
		t.Fatal("expected unknown connection error")
	}
}

func TestSqlitePathRelativeToConfigDir(t *testing.T) {
	p := write(t, "connections.toml", "[conn.L]\ntype = \"sqlite\"\npath = \"local.db\"\n[conn.A]\ntype = \"sqlite\"\npath = \"/abs/x.db\"\n")
	c, err := LoadConnections(p)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := c.Get("L")
	a, _ := c.Get("A")
	if l.Path != filepath.Join(filepath.Dir(p), "local.db") || a.Path != "/abs/x.db" || c.Conn["L"].SQLitePath() != l.Path {
		t.Fatalf("%q %q", l.Path, a.Path)
	}
	if c.Conn["L"].DB().Path != l.Path {
		t.Fatalf("DB(): %q", c.Conn["L"].DB().Path)
	}
	raw, _ := LoadConnectionsRaw(p)
	if raw.Conn["L"].Path != "local.db" {
		t.Fatal("raw loader must keep the relative path")
	}
}
