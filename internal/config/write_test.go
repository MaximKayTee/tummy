package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConnectionsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "connections.toml")
	c, err := LoadConnectionsRaw(path)
	if err != nil || len(c.Conn) != 0 {
		t.Fatalf("missing file: %v %v", c, err)
	}
	c.Conn["PG"] = &Conn{Type: "postgres", URL: "postgres://u:${PW}@h/db", Schema: "public"}
	c.Conn["API"] = &Conn{Type: "api", BaseURL: "https://x", Auth: "bearer", Token: "${TOK}", RateLimit: "100/m", RetryOn: []int{429, 503}}
	if err := WriteConnections(path, c); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	want := "[conn.API]\ntype = \"api\"\nbase_url = \"https://x\"\nauth = \"bearer\"\ntoken = \"${TOK}\"\nrate_limit = \"100/m\"\nretry_on = [429, 503]\n\n[conn.PG]\ntype = \"postgres\"\nurl = \"postgres://u:${PW}@h/db\"\nschema = \"public\"\n"
	if string(raw) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", raw, want)
	}
	back, err := LoadConnectionsRaw(path)
	if err != nil || back.Conn["PG"].URL != "postgres://u:${PW}@h/db" || back.Conn["API"].RetryOn[1] != 503 {
		t.Fatalf("round trip: %v %+v", err, back.Conn)
	}
	t.Setenv("PW", "s")
	t.Setenv("TOK", "t")
	conns, err := LoadConnections(path)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := conns.Get("PG")
	if err != nil || exp.URL != "postgres://u:s@h/db" || conns.Conn["PG"].URL != "postgres://u:${PW}@h/db" {
		t.Fatalf("expanded: %v %+v", err, exp)
	}
}

func TestRenderReplication(t *testing.T) {
	d := ReplicationDraft{
		Name: "sales", Source: "PG", Target: "DUCK", Schedule: "*/15 * * * *", Enabled: false,
		Exclude:  []string{"sales.tmp_*"},
		Defaults: Stream{Mode: "incremental", PrimaryKey: []string{"id"}, Object: "{stream_schema}_{stream_table}"},
		Streams: []StreamDraft{
			{Name: "sales.*", Stream: Stream{Mode: "incremental", PrimaryKey: []string{"id"}, Object: "{stream_schema}_{stream_table}"}},
			{Name: "sales.orders", Stream: Stream{Mode: "incremental", PrimaryKey: []string{"order_id"}, UpdateKey: "updated_at", Object: "{stream_schema}_{stream_table}"}},
			{Name: "daily", Stream: Stream{Mode: "full-refresh", PrimaryKey: nil, Object: "daily", SQL: "select 1\nfrom x"}},
			{Name: "api_thing", Stream: Stream{Mode: "incremental", PrimaryKey: []string{"id"}, Object: "{stream_schema}_{stream_table}", Path: "/t", Query: map[string]string{"b": "2", "a": "1"}, AddColumns: map[string]string{"pid": "{p.id}"}}},
		},
	}
	got, err := RenderReplication(d)
	if err != nil {
		t.Fatal(err)
	}
	want := `source = "PG"
target = "DUCK"
schedule = "*/15 * * * *"
enabled = false
exclude = ["sales.tmp_*"]
mode = "incremental"
object = "{stream_schema}_{stream_table}"
primary_key = ["id"]

[streams."sales.*"]

[streams."sales.orders"]
primary_key = ["order_id"]
update_key = "updated_at"

[streams.daily]
mode = "full-refresh"
object = "daily"
primary_key = []
sql = """
select 1
from x"""

[streams.api_thing]
path = "/t"
query = { a = "1", b = "2" }
add_columns = { pid = "{p.id}" }
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// and it loads back identically
	r, err := LoadReplicationText(got, "/x/sales.toml")
	if err != nil {
		t.Fatal(err)
	}
	back := DraftOf(r)
	if back.Name != "sales" || back.Enabled || len(back.Streams) != 4 || back.Streams[2].Stream.SQL != "select 1\nfrom x" || len(back.Streams[2].Stream.PrimaryKey) != 0 {
		t.Fatalf("round trip: %+v", back)
	}
	if back.Streams[3].Stream.Query["a"] != "1" || back.Streams[3].Stream.AddColumns["pid"] != "{p.id}" {
		t.Fatalf("maps: %+v", back.Streams[3].Stream)
	}
	again, _ := RenderReplication(back)
	if again != got {
		t.Fatalf("second render differs:\n%s", again)
	}
}

func TestWriteReplicationValidates(t *testing.T) {
	home := t.TempDir()
	bad := ReplicationDraft{Name: "x", Source: "A", Target: "B", Enabled: true, Defaults: Stream{Mode: "incremental"}, Streams: []StreamDraft{{Name: "t", Stream: Stream{Mode: "incremental"}}}}
	if _, err := WriteReplication(home, bad); err == nil || !strings.Contains(err.Error(), "incremental requires") {
		t.Fatalf("want validation error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(ReplicationsDir(home), "x.toml")); err == nil {
		t.Fatal("invalid file written")
	}
	good := bad
	good.Defaults.Mode = "full-refresh"
	good.Streams[0].Stream.Mode = "full-refresh"
	p, err := WriteReplication(home, good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReplication(p); err != nil {
		t.Fatal(err)
	}
	if err := DeleteReplication(home, "x"); err != nil {
		t.Fatal(err)
	}
}

func TestQuoteAndKey(t *testing.T) {
	cases := map[string]string{`a"b`: `"a\"b"`, `c\d`: `"c\\d"`, "tab\t": `"tab\t"`, "": `""`}
	for in, want := range cases {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %s, want %s", in, got, want)
		}
	}
	if key("sales.orders") != `"sales.orders"` || key("plain_1") != "plain_1" || key("a b") != `"a b"` {
		t.Fatal("key quoting")
	}
}

func TestPatchKeepsComments(t *testing.T) {
	existing := `# Sales from the warehouse
source = "PG"   # prod replica
target = "DUCK"
mode = "incremental"
primary_key = ["id"]
old_key = "gone"

# every table in sales
[streams."sales.*"]

[streams."sales.orders"]
# orders change often
update_key = "updated_at"  # watermark
primary_key = ["order_id"]

[streams.dropme]
mode = "snapshot"
`
	desired := `source = "PG"
target = "DUCK2"
schedule = "0 3 * * *"
mode = "incremental"
primary_key = ["id"]

[streams."sales.*"]

[streams."sales.orders"]
update_key = "changed_at"
primary_key = ["order_id"]
limit = 10

[streams.fresh]
mode = "truncate"
`
	got, err := patchTOML(existing, desired)
	if err != nil {
		t.Fatal(err)
	}
	want := `# Sales from the warehouse
source = "PG"   # prod replica
target = "DUCK2"
mode = "incremental"
primary_key = ["id"]
schedule = "0 3 * * *"

# every table in sales
[streams."sales.*"]

[streams."sales.orders"]
# orders change often
update_key = "changed_at"  # watermark
primary_key = ["order_id"]
limit = 10

[streams.fresh]
mode = "truncate"
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if _, err := LoadReplicationText(got, "/x/sales.toml"); err != nil {
		t.Fatal(err)
	}
	// multi-line values survive a patch of a neighbouring key
	ml := "source = \"A\"\ntarget = \"B\"\n\n[streams.q]\n# the query\nsql = \"\"\"\nselect 1\nfrom x\"\"\"\nobject = \"q\"\n"
	des := "source = \"A\"\ntarget = \"B\"\n\n[streams.q]\nsql = \"\"\"\nselect 1\nfrom x\"\"\"\nobject = \"q2\"\n"
	got, err = patchTOML(ml, des)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# the query\nsql = \"\"\"\nselect 1\nfrom x\"\"\"\nobject = \"q2\"\n") {
		t.Fatalf("multiline:\n%s", got)
	}
	if got, _ := patchTOML("", desired); got != desired {
		t.Fatal("empty existing should take desired as is")
	}
}

func TestWriteReplicationPreservesComments(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(ReplicationsDir(home), 0o755)
	p := filepath.Join(ReplicationsDir(home), "s.toml")
	os.WriteFile(p, []byte("# keep me\nsource = \"A\"\ntarget = \"B\"\n\n[streams.t]\n# and me\nmode = \"snapshot\"\n"), 0o644)
	r, err := LoadReplication(p)
	if err != nil {
		t.Fatal(err)
	}
	d := DraftOf(r)
	d.Schedule = "@hourly"
	d.Streams[0].Stream.Mode = "truncate"
	if _, err := WriteReplication(home, d); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "# keep me\nsource = \"A\"\ntarget = \"B\"\nschedule = \"@hourly\"\n\n[streams.t]\n# and me\nmode = \"truncate\"\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteConnectionsPreservesComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	os.WriteFile(path, []byte("# prod\n[conn.PG]\ntype = \"postgres\"\nurl = \"postgres://u:${PW}@h/db\"  # replica\n\n# scratch\n[conn.OLD]\ntype = \"sqlite\"\npath = \"old.db\"\n"), 0o644)
	c, _ := LoadConnectionsRaw(path)
	delete(c.Conn, "OLD")
	c.Conn["PG"].Schema = "public"
	c.Conn["NEW"] = &Conn{Type: "sqlite", Path: "new.db"}
	if err := WriteConnections(path, c); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "# prod\n[conn.PG]\ntype = \"postgres\"\nurl = \"postgres://u:${PW}@h/db\"  # replica\nschema = \"public\"\n\n[conn.NEW]\ntype = \"sqlite\"\npath = \"new.db\"\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenameConnectionInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	os.WriteFile(path, []byte("# api\n[conn.POKEAPI]  # public\ntype = \"api\"\nbase_url = \"https://pokeapi.co/api/v2\"\n\n# local\n[conn.LOCAL]\ntype = \"sqlite\"\npath = \"local.db\"\n"), 0o644)
	c, _ := LoadConnectionsRaw(path)
	if err := c.Rename("POKEAPI", "POKE"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("NOPE", "X"); err == nil {
		t.Fatal("unknown connection renamed")
	}
	if err := c.Rename("POKE", "LOCAL"); err == nil {
		t.Fatal("rename onto an existing name")
	}
	c.Conn["POKE"].RateLimit = "100/m"
	if err := WriteConnections(path, c); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "# api\n[conn.POKE]  # public\ntype = \"api\"\nbase_url = \"https://pokeapi.co/api/v2\"\nrate_limit = \"100/m\"\n\n# local\n[conn.LOCAL]\ntype = \"sqlite\"\npath = \"local.db\"\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPreviewWrite(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(ReplicationsDir(home), 0o755)
	orig := "source = \"A\"\ntarget = \"B\"\nmode = \"truncate\"\n\n# runs every minute\nschedule = \"@every 1m\"\n\n[streams.x]\n"
	p := filepath.Join(ReplicationsDir(home), "hr.toml")
	os.WriteFile(p, []byte(orig), 0o644)
	r, err := LoadReplicationRaw(p)
	if err != nil {
		t.Fatal(err)
	}
	d := DraftOf(r)
	d.Schedule = "@every 5m"
	text, diff, err := PreviewWrite(home, d)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != orig {
		t.Fatal("preview wrote the file")
	}
	if !strings.Contains(text, "# runs every minute\nschedule = \"@every 5m\"\n") {
		t.Fatalf("not patched in place:\n%s", text)
	}
	if strings.Join(diff, "\n") != "  source = \"A\"\n  target = \"B\"\n  mode = \"truncate\"\n  \n  # runs every minute\n- schedule = \"@every 1m\"\n+ schedule = \"@every 5m\"\n  \n  [streams.x]" {
		t.Fatalf("diff:\n%s", strings.Join(diff, "\n"))
	}
	if _, err := WriteReplication(home, d); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != text {
		t.Fatalf("written:\n%s\npreview:\n%s", b, text)
	}

	// a rename patches the old file while the new one does not exist
	d.Name = "staff"
	text, _, err = PreviewWrite(home, d)
	if err != nil || !strings.Contains(text, "# runs every minute") {
		t.Fatalf("rename: %v\n%s", err, text)
	}
	if _, err := WriteReplication(home, d); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ReplicationsDir(home), "staff.toml")); string(b) != text {
		t.Fatalf("renamed file:\n%s", b)
	}

	// new file: every line added; a draft that does not load returns its text and the error
	nd := ReplicationDraft{Name: "fresh", Source: "A", Target: "B", Enabled: true, Streams: []StreamDraft{{Name: "t"}}}
	if _, diff, err := PreviewWrite(home, nd); err != nil || len(diff) == 0 || !strings.HasPrefix(diff[0], "+ ") {
		t.Fatalf("new: %v %q", err, diff)
	}
	nd.Defaults.Mode = "bogus"
	if text, _, err := PreviewWrite(home, nd); err == nil || !strings.Contains(text, "bogus") {
		t.Fatalf("invalid draft: %v\n%s", err, text)
	}
}

func TestURLRoundTrip(t *testing.T) {
	for _, raw := range []string{
		"postgres://user:${PG_PASS}@host:5432/db?sslmode=require",
		"mysql://user:p%40ss@host/db",
		"sqlserver://user:${MSSQL_PASS}@host:1433?database=db",
		"sqlserver://app%40tenant:secret@host/INST?database=db&fedauth=ActiveDirectoryServicePrincipal",
		"sqlserver://host?fedauth=ActiveDirectoryDefault",
	} {
		p, ok := ParseURL(raw)
		if !ok {
			t.Fatalf("%s: not parsed", raw)
		}
		if got := p.String(); got != raw {
			t.Errorf("%s\n got %s", raw, got)
		}
	}
	p, _ := ParseURL("mysql://user:p%40ss@host/db")
	if p.Password != "p@ss" || p.Host != "host" || p.Database != "db" {
		t.Errorf("parts %+v", p)
	}
	p, _ = ParseURL("sqlserver://u@srv/INST?database=db")
	if p.Instance != "INST" || p.Database != "db" || p.User != "u" || len(p.Query) != 0 {
		t.Errorf("parts %+v", p)
	}
}

func TestCheckURL(t *testing.T) {
	bad := []string{"myhost", "myhost:1433", "user:pw@myhost", "mssql://u:p@h"}
	for _, u := range bad {
		if CheckURL("mssql", u) == nil {
			t.Errorf("%s: accepted", u)
		}
	}
	good := []string{"sqlserver://u:p@h", "server=h;user id=u", "${MSSQL_URL}"}
	for _, u := range good {
		if err := CheckURL("mssql", u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if CheckURL("postgres", "postgresql://u@h/db") != nil || CheckURL("mysql", "u:p@tcp(h)/db") != nil {
		t.Error("native forms rejected")
	}
}
