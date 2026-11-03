package probe

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"tummy/internal/config"
)

func TestSQLite(t *testing.T) {
	dir := t.TempDir()
	db, _ := sql.Open("sqlite", filepath.Join(dir, "x.db"))
	db.Exec("CREATE TABLE things (id INTEGER)")
	db.Close()
	t.Chdir(t.TempDir())

	res, err := Conn(context.Background(), &config.Conn{Name: "L", Type: "sqlite", Path: "x.db", Dir: dir}, "")
	if err != nil || len(res.Tables) != 1 || !strings.HasSuffix(res.Tables[0], "things") {
		t.Fatalf("relative path against Dir: %v %+v", err, res)
	}
	_, err = Conn(context.Background(), &config.Conn{Name: "L", Type: "sqlite", Path: "missing.db", Dir: dir}, "")
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("probe created the file")
	}
	if _, err := Conn(context.Background(), &config.Conn{Name: "L", Type: "sqlite", Path: "${T_PROBE_UNSET}"}, ""); err == nil {
		t.Fatal("unset variable should fail")
	}
	if res, err := Conn(context.Background(), &config.Conn{Name: "L", Type: "sqlite", Path: "x.db", Dir: dir}, AllSchemas); err != nil || len(res.Tables) != 1 {
		t.Fatalf("all schemas: %v %+v", err, res)
	}
}

func TestCreateSQLite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "new.db")
	if err := CreateSQLite(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	res, err := Conn(context.Background(), &config.Conn{Name: "N", Type: "sqlite", Path: p}, "")
	if err != nil || len(res.Tables) != 0 {
		t.Fatalf("new file: %v %+v", err, res)
	}
	if err := CreateSQLite(p); err != nil {
		t.Fatalf("existing file: %v", err)
	}
	if err := CreateSQLite(":memory:"); err == nil {
		t.Fatal(":memory: accepted")
	}
}

func TestAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	res, err := Conn(context.Background(), &config.Conn{Name: "A", Type: "api", BaseURL: srv.URL}, "")
	if err != nil || res.Status != 404 || res.Tables != nil {
		t.Fatalf("%v %+v", err, res)
	}
	_, err = Conn(context.Background(), &config.Conn{Name: "A", Type: "api", BaseURL: "http://127.0.0.1:1/?api_key=SECRET"}, "")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unreachable: %v", err)
	}
}
