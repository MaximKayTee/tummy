package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBookmarksRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.toml")
	b, err := LoadBookmarks(path)
	if err != nil || len(b.Bookmark) != 0 {
		t.Fatalf("missing file: %v %d", err, len(b.Bookmark))
	}
	if err := b.Set(Bookmark{Name: "paid", Conn: "OUT", Table: "orders", Steps: "filter status = 'paid'", Rep: "sales", Stream: "orders"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Set(Bookmark{Name: "q", Conn: "OUT", SQL: "select 1\nfrom t"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteBookmarks(path, b); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	text := string(raw)
	if !strings.HasPrefix(text, "[bookmark.paid]\nconn = \"OUT\"\ntable = \"orders\"\nsteps = ") || !strings.Contains(text, "\n[bookmark.q]\n") {
		t.Fatalf("file:\n%s", text)
	}
	if strings.Contains(text, "stream = \"\"") {
		t.Fatalf("empty keys written:\n%s", text)
	}

	// comments survive a rewrite
	os.WriteFile(path, []byte("# mine\n"+text), 0o600)
	b2, err := LoadBookmarks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := b2.For("OUT"); len(got) != 2 || got[0].Name != "paid" || got[1].SQL != "select 1\nfrom t" || got[0].Rep != "sales" {
		t.Fatalf("for: %+v", got)
	}
	if got := b2.For("X"); len(got) != 0 {
		t.Fatalf("for X: %+v", got)
	}
	delete(b2.Bookmark, "q")
	if err := WriteBookmarks(path, b2); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if !strings.HasPrefix(string(raw), "# mine\n") || strings.Contains(string(raw), "bookmark.q") {
		t.Fatalf("rewrite:\n%s", raw)
	}
}

func TestBookmarksErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.toml")
	b := &Bookmarks{Bookmark: map[string]*Bookmark{}}
	for _, bm := range []Bookmark{
		{Name: "bad name", Conn: "C", Table: "t"},
		{Name: "x", Table: "t"},
		{Name: "x", Conn: "C"},
		{Name: "x", Conn: "C", Table: "t", SQL: "select 1"},
	} {
		if err := b.Set(bm); err == nil {
			t.Errorf("%+v accepted", bm)
		}
	}
	os.WriteFile(path, []byte("[bookmark.x]\nconn = \"C\"\ntabel = \"t\"\n"), 0o600)
	if _, err := LoadBookmarks(path); err == nil || !strings.Contains(err.Error(), `unknown key "tabel" (table?)`) {
		t.Fatalf("unknown key: %v", err)
	}
	os.WriteFile(path, []byte("[conn.x]\ntype = \"sqlite\"\n"), 0o600)
	if _, err := LoadBookmarks(path); err == nil || !strings.Contains(err.Error(), "[bookmark.NAME]") {
		t.Fatalf("wrong table: %v", err)
	}
}
