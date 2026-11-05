package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// sandbox makes a config home with one sqlite source and points the global flags at it.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, _ := sql.Open("sqlite", filepath.Join(dir, "src.db"))
	db.Exec("CREATE TABLE things (id INTEGER PRIMARY KEY, v TEXT); INSERT INTO things VALUES (1,'a')")
	db.Close()
	os.MkdirAll(filepath.Join(dir, "replications"), 0o755)
	os.WriteFile(filepath.Join(dir, "connections.toml"), []byte("[conn.SRC]\ntype = \"sqlite\"\npath = \"src.db\"\n[conn.TGT]\ntype = \"sqlite\"\npath = \"tgt.db\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "replications", "ok.toml"), []byte("source = \"SRC\"\ntarget = \"TGT\"\n[streams.things]\n"), 0o644)
	t.Setenv("TUMMY_HOME", dir)
	t.Setenv("TUMMY_STATE", filepath.Join(dir, "state.db"))
	t.Setenv("TUMMY_CONNECTIONS", "")
	return dir
}

// tummy runs the CLI in-process and returns stdout and the error.
func tummy(t *testing.T, args ...string) (string, error) {
	t.Helper()
	flagHome, flagConnections, flagState, flagVerbose, flagQuiet = "", "", "", false, true
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	root := newRoot()
	root.SetArgs(append([]string{"-q"}, args...))
	err := root.ExecuteContext(context.Background())
	w.Close()
	os.Stdout = stdout
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String(), err
}

func TestAdhocValidates(t *testing.T) {
	if _, err := adhoc("SRC", "TGT", "orders", "", "bogus", nil, "", "", 0); err == nil {
		t.Fatal("bad mode accepted")
	}
	if _, err := adhoc("SRC", "TGT", "orders", "", "incremental", nil, "", "", 0); err == nil {
		t.Fatal("incremental without keys accepted")
	}
	if _, err := adhoc("SRC", "", "orders", "", "full-refresh", nil, "", "", 0); err == nil {
		t.Fatal("missing --tgt accepted")
	}
	a, _ := adhoc("SRC", "TGT", "sales.orders", "", "full-refresh", nil, "", "", 0)
	b, _ := adhoc("SRC", "TGT", "sales/x y", "", "full-refresh", nil, "", "", 0)
	if a.Name != "adhoc-SRC-sales.orders" || b.Name != "adhoc-SRC-sales_x_y" {
		t.Fatalf("names: %q %q", a.Name, b.Name)
	}
}

func TestRunByNameAndErrors(t *testing.T) {
	dir := sandbox(t)
	if out, err := tummy(t, "run", "ok"); err != nil {
		t.Fatalf("run NAME: %v %s", err, out)
	}
	if _, err := tummy(t, "run", filepath.Join(dir, "replications", "ok.toml"), "--streams", "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unmatched --streams: %v", err)
	}
	if _, err := tummy(t, "run", "ok", "--src", "SRC"); err == nil {
		t.Fatal("FILE plus ad-hoc flags accepted")
	}
	if _, err := tummy(t, "run", "missing"); err == nil || !strings.Contains(err.Error(), "no replication missing") {
		t.Fatalf("missing name: %v", err)
	}
	if _, err := tummy(t, "run", "--src", "SRC", "--tgt", "TGT", "--stream", "things", "--mode", "bogus"); err == nil {
		t.Fatal("ad-hoc bad mode accepted")
	}
	out, err := tummy(t, "runs", "ok", "--status", "success", "--since", "1h")
	if err != nil || !strings.Contains(out, "things") {
		t.Fatalf("runs: %v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "replications", "nocon.toml"), []byte("source = \"SRC\"\ntarget = \"NOPE\"\n[streams.things]\n"), 0o644)
	tummy(t, "run", "nocon")
	out, err = tummy(t, "runs")
	if err != nil || !strings.Contains(out, "(replication)") || !strings.Contains(out, "failed: unknown connection NOPE") ||
		!strings.Contains(out, "manual") || strings.Contains(out, "success") || !strings.Contains(out, " ok") {
		t.Fatalf("runs words: %v\n%s", err, out)
	}
	if out, _ := tummy(t, "runs", "--status", "failed"); !strings.Contains(out, "nocon") || strings.Contains(out, "things") {
		t.Fatalf("--status failed:\n%s", out)
	}
	if _, err := tummy(t, "runs", "--status", "bogus"); err == nil {
		t.Fatal("bad --status accepted")
	}
}

func TestListAndCheckExitCodes(t *testing.T) {
	dir := sandbox(t)
	if _, err := tummy(t, "list"); err != nil {
		t.Fatalf("clean list: %v", err)
	}
	if _, err := tummy(t, "check"); err != nil {
		t.Fatalf("clean check: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "replications", "nocon.toml"), []byte("source = \"SRC\"\ntarget = \"NOPE\"\n[streams.things]\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "replications", "cron.toml"), []byte("source = \"SRC\"\ntarget = \"TGT\"\nschedule = \"every 5 min\"\n[streams.things]\n"), 0o644)
	out, err := tummy(t, "list")
	var code exitCode
	if !errors.As(err, &code) || code != 1 || !strings.Contains(out, "NOPE?") || !strings.Contains(out, "unknown connection NOPE") ||
		!strings.Contains(out, `schedule "every 5 min"`) || strings.Contains(out, dir) {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if out, _ := tummy(t, "status"); !strings.Contains(out, "unknown connection NOPE") {
		t.Fatalf("status:\n%s", out)
	}
	if out, _ := tummy(t, "conns", "list"); !strings.HasPrefix(out, "NAME  TYPE    URL") {
		t.Fatalf("conns list:\n%s", out)
	}
	if out, err := tummy(t, "conns", "test", "SRC"); err != nil || !strings.Contains(out, "ok  1 tables") {
		t.Fatalf("conns test: %v %s", err, out)
	}
	if _, err := tummy(t, "check", "nocon"); !errors.As(err, &code) {
		t.Fatalf("check: %v", err)
	}
	if out, err := tummy(t, "check", "ok"); err != nil || !strings.Contains(out, "inherited  mode = \"full-refresh\"") {
		t.Fatalf("check ok: %v\n%s", err, out)
	}
	if _, err := tummy(t, "status"); !errors.As(err, &code) {
		t.Fatalf("status with broken files: %v", err)
	}
	if _, err := tummy(t, "conns", "bogus"); err == nil {
		t.Fatal("conns bogus accepted")
	}
}

func TestNextRunAgrees(t *testing.T) {
	dir := sandbox(t)
	os.WriteFile(filepath.Join(dir, "replications", "daily.toml"), []byte("source = \"SRC\"\ntarget = \"TGT\"\nschedule = \"@daily\"\n[streams.things]\n"), 0o644)
	list, _ := tummy(t, "list")
	check, _ := tummy(t, "check", "daily")
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour).Local().Format(time.DateTime)
	if !strings.Contains(list, day) || !strings.Contains(check, "next "+day) {
		t.Fatalf("want %s\nlist:\n%s\ncheck:\n%s", day, list, check)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"24h":              now.Add(-24 * time.Hour),
		"7d":               now.Add(-7 * 24 * time.Hour),
		"2026-11-01":       time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local),
		"2026-11-01 08:00": time.Date(2026, 11, 1, 8, 0, 0, 0, time.Local),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := parseSince("soon", now); err == nil {
		t.Error("bad value accepted")
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("failed to connect:\n\t127.0.0.1:5432: connection refused"); got != "failed to connect: 127.0.0.1:5432: connection refused" {
		t.Fatal(got)
	}
}

// A fresh machine: default paths under XDG, nothing created yet.
func TestFreshMachineDefaults(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	for _, v := range []string{"TUMMY_HOME", "TUMMY_STATE", "TUMMY_CONNECTIONS"} {
		t.Setenv(v, "")
	}
	for _, args := range [][]string{{"conns", "list"}, {"list"}, {"runs"}, {"status"}} {
		if _, err := tummy(t, args...); err != nil {
			var code exitCode
			if !errors.As(err, &code) || code > 1 {
				t.Fatalf("%v: %v", args, err)
			}
		}
	}
	if out, err := tummy(t, "--version"); err != nil || !strings.HasPrefix(out, "tummy ") {
		t.Fatalf("--version: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "tummy")); err == nil {
		t.Fatal("listing must not create the config dir")
	}
}
