package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvExpandedAfterDecode(t *testing.T) {
	t.Setenv("T_PASS", `p\s"s`+"\nx")
	t.Setenv("T_TENANT", "acme")
	p := write(t, "connections.toml", `
# url = "${T_OLD_UNSET}"
[conn.PG]
type = "api"
base_url = "https://h"
token = "${T_PASS}"

[conn.OTHER]
type = "postgres"
url = "postgres://u:${T_UNSET_XYZ}@h/db"
`)
	c, err := LoadConnections(p)
	if err != nil {
		t.Fatalf("an unset variable in a comment or an unused connection must not fail the load: %v", err)
	}
	pg, err := c.Get("PG")
	if err != nil || pg.Token != `p\s"s`+"\nx" {
		t.Fatalf("value with quotes, backslash and newline: %v %+v", err, pg)
	}
	if _, err := c.Get("OTHER"); err == nil || !strings.Contains(err.Error(), "conn OTHER: url: unset environment variable T_UNSET_XYZ") {
		t.Fatalf("error should name conn and key: %v", err)
	}

	r, err := LoadReplication(write(t, "r.toml", `
source = "A"
target = "B"
where = "tenant = '${T_TENANT}'"
on_error = "echo ${TUMMY_ERROR}"
[streams.x]
query = { key = "${T_TENANT}" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Resolved[0].Where != "tenant = 'acme'" || r.Resolved[0].Query["key"] != "acme" || r.OnError != "echo ${TUMMY_ERROR}" {
		t.Fatalf("expanded: %+v %q", r.Resolved[0].Stream, r.OnError)
	}
	d := DraftOf(r)
	if d.Defaults.Where != "tenant = '${T_TENANT}'" || d.Streams[0].Stream.Query["key"] != "${T_TENANT}" {
		t.Fatalf("draft must keep the values as written: %+v", d.Streams[0].Stream)
	}
	_, err = LoadReplication(write(t, "r.toml", "source = \"A\"\ntarget = \"B\"\n[streams.x]\nwhere = \"${T_UNSET_XYZ}\"\n"))
	if err == nil || !strings.Contains(err.Error(), "stream x: where: unset environment variable T_UNSET_XYZ") {
		t.Fatalf("error should name stream and key: %v", err)
	}
	raw, err := LoadReplicationRaw(write(t, "r.toml", "source = \"A\"\ntarget = \"B\"\nquery_timeout = \"${T_UNSET_XYZ}\"\n[streams.x]\n"))
	if err != nil || raw.Resolved[0].QueryTimeout != "${T_UNSET_XYZ}" {
		t.Fatalf("raw load: %v", err)
	}
}

func TestSaveKeepsEnvReferences(t *testing.T) {
	t.Setenv("T_TENANT", "acme")
	home := t.TempDir()
	os.MkdirAll(ReplicationsDir(home), 0o755)
	p := filepath.Join(ReplicationsDir(home), "s.toml")
	os.WriteFile(p, []byte("source = \"A\"\ntarget = \"B\"\nwhere = \"tenant = '${T_TENANT}'\"\n\n[streams.x]\nquery = { key = \"${T_TENANT}\" }\n"), 0o644)
	r, err := LoadReplication(p)
	if err != nil {
		t.Fatal(err)
	}
	d := DraftOf(r)
	d.Schedule = "@hourly"
	if _, err := WriteReplication(home, d); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if strings.Contains(string(got), "acme") || !strings.Contains(string(got), "${T_TENANT}") {
		t.Fatalf("expanded value written:\n%s", got)
	}
}

func TestDefaultsChangeNotBakedIntoStreams(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(ReplicationsDir(home), 0o755)
	p := filepath.Join(ReplicationsDir(home), "s.toml")
	os.WriteFile(p, []byte("source = \"A\"\ntarget = \"B\"\nmode = \"incremental\"\nprimary_key = [\"id\"]\n\n[streams.a]\n\n[streams.b]\nmode = \"incremental\"\n"), 0o644)
	r, _ := LoadReplication(p)
	d := DraftOf(r)
	d.Defaults.Mode = "snapshot"
	d.Defaults.PrimaryKey = []string{"uuid"}
	if _, err := WriteReplication(home, d); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "source = \"A\"\ntarget = \"B\"\nmode = \"snapshot\"\nprimary_key = [\"uuid\"]\n\n[streams.a]\n\n[streams.b]\nmode = \"incremental\"\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	r, _ = LoadReplication(p)
	if r.Resolved[0].Mode != "snapshot" || r.Resolved[0].PrimaryKey[0] != "uuid" || r.Resolved[1].Mode != "incremental" {
		t.Fatalf("%+v", r.Resolved)
	}

	// a stream rebuilt by an editor without Keys keeps the file's explicit keys
	d = DraftOf(r)
	d.Streams[1] = StreamDraft{Name: "b", Stream: Stream{Mode: "incremental", PrimaryKey: []string{"uuid"}, Limit: 5}}
	d.Defaults.Mode = "incremental"
	if _, err := WriteReplication(home, d); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(p)
	if !strings.Contains(string(got), "[streams.b]\nmode = \"incremental\"\nlimit = 5\n") {
		t.Fatalf("explicit key lost:\n%s", got)
	}
}

func TestSetEnabledCommentIndentCRLF(t *testing.T) {
	cases := map[string]string{
		"comment":  "source = \"A\"\ntarget = \"B\"\nenabled = false  # paused\n\n[streams.x]\n",
		"indent":   "source = \"A\"\ntarget = \"B\"\n  enabled = false\n\n[streams.x]\n",
		"crlf":     "source = \"A\"\r\ntarget = \"B\"\r\nenabled = false\r\n\r\n[streams.x]\r\n",
		"subtable": "source = \"A\"\ntarget = \"B\"\nenabled = false # x\n\n[streams.x]\n[streams.x.query]\na = \"1\"\n",
	}
	for name, body := range cases {
		p := write(t, "r.toml", body)
		if err := SetEnabled(p, true); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, _ := os.ReadFile(p)
		if strings.Count(string(got), "enabled") != 1 {
			t.Fatalf("%s: duplicate key:\n%q", name, got)
		}
		r, err := LoadReplication(p)
		if err != nil || !r.IsEnabled() {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "comment" && !strings.Contains(string(got), "enabled = true  # paused") {
			t.Fatalf("comment lost: %q", got)
		}
		if name == "crlf" && strings.Count(string(got), "\r\n") != strings.Count(string(got), "\n") {
			t.Fatalf("line endings changed: %q", got)
		}
	}
}

func TestPatchMultilineAndQuoting(t *testing.T) {
	existing := `# top
source = "A"
target = "B"
exclude = [
  "tmp_*",   # scratch
  "audit.*",
]

[streams.'a.b']
# the query
sql = '''
select 1
from x'''
object = "q"

[streams."caf\u00e9"]
limit = 1_000
primary_key = [ "id" ]
`
	desired := `source = "A"
target = "C"
exclude = ["tmp_*", "audit.*"]

[streams."a.b"]
sql = """
select 1
from x"""
object = "q2"

[streams."café"]
limit = 1000
primary_key = ["id"]
`
	got, err := patchTOML(existing, desired)
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"# top\n", `"tmp_*",   # scratch`, "[streams.'a.b']\n# the query\nsql = '''", "limit = 1_000", `primary_key = [ "id" ]`, `[streams."caf\u00e9"]`} {
		if !strings.Contains(got, keep) {
			t.Fatalf("lost %q:\n%s", keep, got)
		}
	}
	if !strings.Contains(got, "target = \"C\"") || !strings.Contains(got, "object = \"q2\"") {
		t.Fatalf("not patched:\n%s", got)
	}

	// what the parser cannot follow is an error, never a silent clean render
	if _, err := patchTOML("a = 1\n[x.y.z]\nb = 2\n", "a = 2\n"); err == nil {
		t.Fatal("sub-table header should fail the patch")
	}
}

func TestPatchDropsKeyWithItsComment(t *testing.T) {
	existing := "source = \"A\"\n# why we limit\nlimit = 5\n\n# keep\ntarget = \"B\"\n"
	got, err := patchTOML(existing, "source = \"A\"\ntarget = \"B\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "source = \"A\"\n\n# keep\ntarget = \"B\"\n" {
		t.Fatalf("%q", got)
	}
}

func TestWriteFailsRatherThanDropComments(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(ReplicationsDir(home), 0o755)
	p := filepath.Join(ReplicationsDir(home), "s.toml")
	body := "# mine\nsource = \"A\"\ntarget = \"B\"\n\n[streams.x]\n[streams.x.query]\na = \"1\"\n"
	os.WriteFile(p, []byte(body), 0o644)
	r, err := LoadReplication(p)
	if err != nil {
		t.Fatal(err)
	}
	d := DraftOf(r)
	d.Schedule = "@daily"
	if _, err := WriteReplication(home, d); err == nil {
		t.Fatal("expected an error")
	}
	if got, _ := os.ReadFile(p); string(got) != body {
		t.Fatalf("file changed:\n%s", got)
	}
}

func TestMapsReplaceDefaults(t *testing.T) {
	r, err := LoadReplication(write(t, "r.toml", `source = "A"
target = "B"
query = { a = "1" }
[streams.x]
query = { b = "2" }
[streams.y]
query = {}
[streams.z]
`))
	if err != nil {
		t.Fatal(err)
	}
	if q := r.Resolved[0].Query; len(q) != 1 || q["b"] != "2" {
		t.Fatalf("x: %v", q)
	}
	if q := r.Resolved[1].Query; q == nil || len(q) != 0 {
		t.Fatalf("y: %v", q)
	}
	if q := r.Resolved[2].Query; q["a"] != "1" {
		t.Fatalf("z: %v", q)
	}
	d := DraftOf(r)
	d.Streams[2].Stream.Query = nil
	text, err := RenderReplication(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "[streams.y]\nquery = {}\n") || !strings.Contains(text, "[streams.z]\nquery = {}\n") {
		t.Fatalf("empty override lost:\n%s", text)
	}
}

func TestConnectionsRejectUnknownKeys(t *testing.T) {
	for body, want := range map[string]string{
		"[conn.L]\ntype = \"sqlite\"\npaht = \"x.db\"\n":                          `unknown key "paht" (path?)`,
		"[conn.L]\ntype = \"api\"\nbase_url = \"x\"\nconcurrency = 2\n":           "concurrency: not supported",
		"[L]\ntype = \"sqlite\"\n":                                                "connections go under [conn.NAME]",
		"[conn.L]\ntype = \"oracle\"\nurl = \"x\"\n":                              `type "oracle"`,
		"[conn.L]\ntype = \"api\"\nbase_url = \"x\"\nauth = \"oauth\"\n":          `auth "oauth"`,
		"[conn.L]\ntype = \"api\"\nbase_url = \"x\"\nrate_limit = \"10 per s\"\n": "rate_limit",
	} {
		_, err := LoadConnections(write(t, "c.toml", body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", body, want, err)
		}
	}
	if _, err := LoadConnectionsRaw(write(t, "c.toml", "[conn.L]\ntype = \"sqlite\"\npaht = \"x.db\"\n")); err == nil {
		t.Error("raw load must reject unknown keys too")
	}
}

func TestResolvePath(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"x.db":              "/cfg/x.db",
		"~/x.db":            filepath.Join(home, "x.db"),
		"/abs/x.db":         "/abs/x.db",
		":memory:":          ":memory:",
		"file:x.db?mode=ro": "file:x.db?mode=ro",
		"":                  "",
	} {
		if got := ResolvePath(in, "/cfg"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestValidationAtLoad(t *testing.T) {
	cases := map[string]string{
		"fetch_size":             "fetch_size = 10\n[streams.x]\n",
		"range without backfill": "[streams.x]\nrange = \"1,2\"\n",
		"cursor without field":   "[streams.x]\npath = \"/x\"\npaginate = \"cursor\"\n",
		"wildcard sql":           "[streams.\"s.*\"]\nsql = \"select 1\"\nobject = \"o_{stream_table}\"\n",
		"wildcard one object":    "[streams.\"s.*\"]\nobject = \"one\"\n",
		"paginate":               "[streams.x]\npath = \"/x\"\npaginate = \"pages\"\n",
		"method":                 "[streams.x]\npath = \"/x\"\nmethod = \"FETCH\"\n",
		"query_timeout":          "[streams.x]\nquery_timeout = \"10\"\n",
		"schedule":               "schedule = \"every 5 min\"\n[streams.x]\n",
		"zero streams":           "",
		"mixed select":           "[streams.x]\nselect = [\"a\", \"-b\"]\n",
		"params_in":              "[streams.x]\npath = \"/x\"\nparams_in = \"header\"\n",
	}
	for name, body := range cases {
		if _, err := LoadReplication(write(t, "r.toml", "source = \"A\"\ntarget = \"B\"\n"+body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	ok := "source = \"A\"\ntarget = \"B\"\nschedule = \"CRON_TZ=Europe/Amsterdam 30 2 * * *\"\n[streams.\"s.*\"]\nobject = \"x_{stream_table}\"\n[streams.y]\nselect = [\"-secret\"]\n"
	if _, err := LoadReplication(write(t, "r.toml", ok)); err != nil {
		t.Fatal(err)
	}
}

func TestFriendlyErrors(t *testing.T) {
	cases := map[string]string{
		"[streams.sales.orders]\nmode = \"snapshot\"\n":  `quote names that contain dots, as in [streams."sales.orders"]`,
		"[streams.x]\nschedule = \"@daily\"\n":           "schedule is a top-level key",
		"[defaults]\nmode = \"snapshot\"\n[streams.x]\n": "[defaults]: defaults are top-level keys",
		"source_options = { a = 1 }\n[streams.x]\n":      "source_options: not supported",
		"[streams.x]\nupdate_kye = \"u\"\n":              `unknown key "update_kye" (update_key?)`,
		"schedule = \"every 10m\"\n[streams.x]\n":        "(@every 10m?)",
	}
	for body, want := range cases {
		_, err := LoadReplication(write(t, "r.toml", "source = \"A\"\ntarget = \"B\"\n"+body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q:\nwant %q\ngot  %v", body, want, err)
		}
	}
	r, err := LoadReplication(write(t, "r.toml", "source = \"A\"\ntarget = \"B\"\nprimary_key = \"id\"\nstreams.x.mode = \"snapshot\"\n[streams.y]\nselect = \"a\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Resolved[0].Name != "x" || r.Resolved[0].Mode != "snapshot" || r.Resolved[0].PrimaryKey[0] != "id" || r.Resolved[1].Select[0] != "a" {
		t.Fatalf("%+v", r.Resolved)
	}
}

func TestSQLFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "queries"), 0o755)
	os.WriteFile(filepath.Join(dir, "queries", "x.sql"), []byte("select 1\n"), 0o644)
	p := filepath.Join(dir, "r.toml")
	os.WriteFile(p, []byte("source = \"A\"\ntarget = \"B\"\n[streams.x]\nsql = \"file:queries/x.sql\"\nobject = \"x\"\n"), 0o644)
	r, err := LoadReplication(p)
	if err != nil || r.Resolved[0].SQL != "select 1" {
		t.Fatalf("%v %+v", err, r)
	}
	if DraftOf(r).Streams[0].Stream.SQL != "file:queries/x.sql" {
		t.Fatal("draft must keep the reference")
	}
	os.WriteFile(p, []byte("source = \"A\"\ntarget = \"B\"\n[streams.x]\nsql = \"file:nope.sql\"\nobject = \"x\"\n"), 0o644)
	if _, err := LoadReplication(p); err == nil {
		t.Fatal("missing sql file should fail")
	}
}

func TestSelectColumns(t *testing.T) {
	all := []string{"id", "name", "secret"}
	if got := (Stream{Select: Names{"-secret"}}).Columns(all); strings.Join(got, ",") != "id,name" {
		t.Fatal(got)
	}
	if got := (Stream{Select: Names{"name"}}).Columns(all); strings.Join(got, ",") != "name" {
		t.Fatal(got)
	}
	if got := (Stream{}).Columns(all); len(got) != 3 {
		t.Fatal(got)
	}
}

func TestNoDefaultModeWritten(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(ReplicationsDir(home), 0o755)
	p := filepath.Join(ReplicationsDir(home), "s.toml")
	os.WriteFile(p, []byte("source = \"A\"\ntarget = \"B\"\n\n[streams.t]\n"), 0o644)
	r, _ := LoadReplication(p)
	if r.Mode != "full-refresh" || r.Resolved[0].Mode != "full-refresh" {
		t.Fatal("default mode at use")
	}
	d := DraftOf(r)
	d.Defaults.Mode = "full-refresh"
	WriteReplication(home, d)
	if got, _ := os.ReadFile(p); strings.Contains(string(got), "mode") {
		t.Fatalf("mode added:\n%s", got)
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"postgres://u:pa:ss@h/db":                      "postgres://u:***@h/db",
		"postgres://u:p@ss@h:5432/db?sslmode=disable":  "postgres://u:***@h:5432/db?sslmode=disable",
		"postgres://u@h/db?password=x&sslmode=require": "postgres://u@h/db?password=***&sslmode=require",
		"host=h user=u password=hunter2 dbname=d":      "host=h user=u password=*** dbname=d",
		"server=h;user id=u;password=x;database=d":     "server=h;user id=u;password=***;database=d",
		"u:secret@tcp(h:3306)/db?parseTime=true":       "u:***@tcp(h:3306)/db?parseTime=true",
		"https://api.example.com/v1?api_key=abc&x=1":   "https://api.example.com/v1?api_key=***&x=1",
		"https://example.com/users/@me":                "https://example.com/users/@me",
		"local.db":                                     "local.db",
		"sqlserver://sa:p%2Fw@h?database=x":            "sqlserver://sa:***@h?database=x",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	t.Setenv("T_SET_ALREADY", "keep")
	p := write(t, "tummy.env", "# secrets\nexport T_ENV_A=one\nT_ENV_B = 'two words'\nT_ENV_C=\"a\\nb\"\nT_SET_ALREADY=other\n")
	os.Unsetenv("T_ENV_A")
	os.Unsetenv("T_ENV_B")
	os.Unsetenv("T_ENV_C")
	t.Cleanup(func() { os.Unsetenv("T_ENV_A"); os.Unsetenv("T_ENV_B"); os.Unsetenv("T_ENV_C") })
	if err := LoadEnvFile(p); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("T_ENV_A") != "one" || os.Getenv("T_ENV_B") != "two words" || os.Getenv("T_ENV_C") != "a\nb" || os.Getenv("T_SET_ALREADY") != "keep" {
		t.Fatal(os.Getenv("T_ENV_A"), os.Getenv("T_ENV_B"), os.Getenv("T_ENV_C"))
	}
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "none")); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnvFile(write(t, "bad.env", "not a pair\n")); err == nil {
		t.Fatal("bad line should fail")
	}
}

func TestParseSchedule(t *testing.T) {
	for _, ok := range []string{"* * * * *", "*/15 * * * *", "@every 10m", "@hourly", "0 3 * * 1-5", "CRON_TZ=Europe/Amsterdam 0 3 * * *"} {
		if _, err := ParseSchedule(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "* * *", "every 10m", "61 * * * *"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	s, _ := ParseSchedule("30 2 * * *")
	from := time.Date(2026, 3, 29, 0, 0, 0, 0, time.FixedZone("CET", 3600))
	if got := NextRun(s, from); !got.Equal(time.Date(2026, 3, 29, 2, 30, 0, 0, time.UTC)) {
		t.Fatalf("cron fields are UTC: %v", got)
	}
}

func TestExpandWildcards(t *testing.T) {
	r, err := LoadReplication(write(t, "r.toml", "source = \"A\"\ntarget = \"B\"\nexclude = [\"sales.tmp_*\"]\n[streams.\"sales.*\"]\n[streams.\"sales.orders\"]\nlimit = 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Expand(func(schema string) ([]string, error) {
		if schema != "sales" {
			t.Fatalf("schema %q", schema)
		}
		return []string{"sales.customers", "sales.orders", "sales.tmp_x"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "sales.customers" || got[1].Name != "sales.orders" || got[1].Limit != 5 {
		t.Fatalf("%+v", got)
	}
}

func TestWriteFileMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	p := filepath.Join(dir, "connections.toml")
	c := &Connections{Conn: map[string]*Conn{"L": {Type: "sqlite", Path: "x.db"}}, Path: p}
	if err := WriteConnections(p, c); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("file %v dir %v", fi.Mode(), di.Mode())
	}
	os.Chmod(p, 0o640)
	WriteConnections(p, c)
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatalf("existing mode not kept: %v", fi.Mode())
	}
}
