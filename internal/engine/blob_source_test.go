package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"tummy/internal/blob"
	"tummy/internal/config"
)

// container serves List Blobs and Get Blob for a container named "c", SAS sig "s".
type container struct {
	mu    sync.Mutex
	files map[string]string
	mod   map[string]time.Time
	gets  []string
}

func (c *container) put(name, body string, mod time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[name], c.mod[name] = body, mod
}

func (c *container) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Query().Get("sig") != "s" {
		w.WriteHeader(403)
		return
	}
	if r.URL.Path == "/c" {
		var names []string
		for n := range c.files {
			if strings.HasPrefix(n, r.URL.Query().Get("prefix")) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		fmt.Fprint(w, `<EnumerationResults><Blobs>`)
		for _, n := range names {
			fmt.Fprintf(w, `<Blob><Name>%s</Name><Properties><Last-Modified>%s</Last-Modified><Content-Length>%d</Content-Length></Properties></Blob>`,
				n, c.mod[n].Format(http.TimeFormat), len(c.files[n]))
		}
		fmt.Fprint(w, `</Blobs><NextMarker/></EnumerationResults>`)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/c/")
	body, ok := c.files[name]
	if !ok {
		w.WriteHeader(404)
		return
	}
	c.gets = append(c.gets, name)
	fmt.Fprint(w, body)
}

func blobFixture(t *testing.T) (*fixture, *container) {
	f := setup(t)
	c := &container{files: map[string]string{}, mod: map[string]time.Time{}}
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	f.conns.Conn["FILES"] = &config.Conn{Name: "FILES", Type: "azblob", URL: srv.URL + "/c", Auth: config.BlobSAS, Token: "sv=1&sig=s"}
	return f, c
}

var day1 = time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)

func TestBlobFullRefresh(t *testing.T) {
	f, c := blobFixture(t)
	c.put("in/file1.csv", "id,name\n1,ann\n2,bob\n", day1)
	c.put("in/file2.csv", "\xEF\xBB\xBFid,Name,extra\n3,cy,x\n,,\n", day1)
	c.put("in/file3.csv", "id,name\n9,no\n", day1)
	c.put("in/notes.txt", "x", day1)
	c.put("other/file1.csv", "id\n7\n", day1)
	r := f.replication(`
source = "FILES"
target = "TGT"
[streams.people]
path = "in/file[12].csv"
`)
	res := f.run(f.engine(), r)
	if res[0].Rows != 3 {
		t.Fatalf("rows %d, want 3 (blank row skipped, file3 not matched)", res[0].Rows)
	}
	if f.count("SELECT count(*) FROM people WHERE _file = 'in/file2.csv' AND name = 'cy' AND extra = 'x' AND _row = 1") != 1 {
		t.Fatal("header union by name, ignoring case")
	}
	if f.count("SELECT count(*) FROM people WHERE extra IS NULL") != 2 {
		t.Fatal("missing column is null")
	}
	var mod string
	f.tgt.QueryRow("SELECT _modified FROM people LIMIT 1").Scan(&mod)
	if !strings.HasPrefix(mod, "2026-11-24") {
		t.Fatalf("_modified %q", mod)
	}

	c.put("in/file1.csv", "id,name\n1,ann\n", day1)
	f.run(f.engine(), r)
	if f.count("SELECT count(*) FROM people") != 2 {
		t.Fatal("full refresh did not replace")
	}
}

func TestBlobIncrementalByModified(t *testing.T) {
	f, c := blobFixture(t)
	c.put("in/a.csv", "id,v\n1,a\n2,b\n", day1)
	r := f.replication(`
source = "FILES"
target = "TGT"
mode = "incremental"
update_key = "_modified"
primary_key = ["_file", "_row"]
[streams.drops]
path = "in/*.csv"
`)
	f.run(f.engine(), r)
	c.gets = nil
	c.put("in/b.csv", "id,v\n3,c\n", day1.Add(time.Hour))
	res := f.run(f.engine(), r)
	// with a primary key the watermark is inclusive: a.csv, at it, is read again
	if strings.Join(c.gets, " ") != "in/a.csv in/b.csv" {
		t.Fatalf("downloaded %v", c.gets)
	}
	if res[0].Rows != 3 || f.count("SELECT count(*) FROM drops") != 3 {
		t.Fatalf("rows %d", res[0].Rows)
	}
	// a changed file replaces its rows by _file, _row
	c.put("in/a.csv", "id,v\n1,A\n2,B\n", day1.Add(2*time.Hour))
	f.run(f.engine(), r)
	if f.count("SELECT count(*) FROM drops") != 3 || f.count("SELECT count(*) FROM drops WHERE v = 'A'") != 1 {
		t.Fatal("changed file not upserted")
	}
	// nothing new: only the newest file is read again, its rows replace themselves
	c.gets = nil
	f.run(f.engine(), r)
	if strings.Join(c.gets, " ") != "in/a.csv" || f.count("SELECT count(*) FROM drops") != 3 {
		t.Fatalf("downloaded %v", c.gets)
	}
}

func TestBlobXLSXSheetSelectLimit(t *testing.T) {
	f, c := blobFixture(t)
	x := excelize.NewFile()
	x.SetSheetRow("Sheet1", "A1", &[]any{"ignored"})
	x.NewSheet("Data")
	x.SetSheetRow("Data", "A1", &[]any{"id", "when", "amount"})
	x.SetSheetRow("Data", "A2", &[]any{1, "2026-11-01", 12.5})
	x.SetSheetRow("Data", "A3", &[]any{2, "2026-11-02", 7})
	x.SetSheetRow("Data", "A4", &[]any{3, "2026-11-03", 1})
	var buf bytes.Buffer
	if err := x.Write(&buf); err != nil {
		t.Fatal(err)
	}
	c.put("book.xlsx", buf.String(), day1)
	r := f.replication(`
source = "FILES"
target = "TGT"
[streams.book]
path = "book.xlsx"
sheet = "Data"
select = ["id", "amount", "_file"]
limit = 2
`)
	res := f.run(f.engine(), r)
	if res[0].Rows != 2 {
		t.Fatalf("rows %d", res[0].Rows)
	}
	if f.count("SELECT count(*) FROM book WHERE amount = '12.5' AND _file = 'book.xlsx'") != 1 {
		t.Fatal("xlsx values")
	}
	cols, _ := f.tgt.Query("SELECT * FROM book")
	names, _ := cols.Columns()
	cols.Close()
	if strings.Join(names, ",") != "id,amount,_file" {
		t.Fatalf("columns %v", names)
	}

	bad := f.replication("source = \"FILES\"\ntarget = \"TGT\"\n[streams.book]\npath = \"book.xlsx\"\nsheet = \"Nope\"\n")
	if _, err := f.engine().Run(context.Background(), bad, nil); err == nil || !strings.Contains(err.Error(), "Sheet1, Data") {
		t.Fatalf("unknown sheet: %v", err)
	}
}

func TestBlobErrors(t *testing.T) {
	f, c := blobFixture(t)
	c.put("a.csv", "id\n1,2\n", day1)
	r := f.replication("source = \"FILES\"\ntarget = \"TGT\"\n[streams.a]\npath = \"a.csv\"\n")
	if _, err := f.engine().Run(context.Background(), r, nil); err == nil || !strings.Contains(err.Error(), "a.csv: row at 2 has more cells than the header") {
		t.Fatalf("ragged row: %v", err)
	}
	r = f.replication("source = \"FILES\"\ntarget = \"TGT\"\n[streams.a]\nobject = \"a\"\n")
	if _, err := f.engine().Run(context.Background(), r, nil); err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("no path: %v", err)
	}
	r = f.replication("source = \"TGT\"\ntarget = \"FILES\"\n[streams.a]\n")
	if _, err := f.engine().Run(context.Background(), r, nil); err == nil || !strings.Contains(err.Error(), "not a target") {
		t.Fatalf("azblob target: %v", err)
	}
	// a pattern that matches nothing loads nothing and does not fail
	r = f.replication("source = \"FILES\"\ntarget = \"TGT\"\n[streams.none]\npath = \"nothing/*.csv\"\n")
	res := f.run(f.engine(), r)
	if res[0].Rows != 0 {
		t.Fatal("rows from no files")
	}
}

func TestBlobColumnNamedLikeAdded(t *testing.T) {
	f, c := blobFixture(t)
	c.put("a.csv", "_file,x\nmine,1\n", day1)
	r := f.replication("source = \"FILES\"\ntarget = \"TGT\"\n[streams.a]\npath = \"a.csv\"\n")
	f.run(f.engine(), r)
	if f.count("SELECT count(*) FROM a WHERE _file_2 = 'mine' AND _file = 'a.csv'") != 1 {
		t.Fatal("a header named _file is renamed, not overwritten")
	}
}

func TestPreviewFiles(t *testing.T) {
	f, c := blobFixture(t)
	c.put("in/a.csv", "id,v\n1,a\n2,b\n", day1)
	c.put("in/b.csv", "id,w\n3,c\n", day1)
	c.put("in/c.csv", "id,x\n4,d\n", day1)
	c.put("in/d.txt", "x", day1)
	c.put("in/z[odd].csv", "id\n9\n", day1)
	conn := f.conns.Conn["FILES"]

	p, err := PreviewFiles(context.Background(), conn, config.Stream{Path: "in/*"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if p.Files != 4 || p.Read != 2 || len(p.Rows) != 3 || p.Complete || strings.Join(p.Skipped, ",") != "in/d.txt" {
		t.Fatalf("%+v", p)
	}
	var names []string
	for _, col := range p.Cols {
		names = append(names, col.Name)
	}
	if strings.Join(names, ",") != "id,v,w,_file,_row,_modified" {
		t.Fatalf("columns %v: only the files read", names)
	}
	c.gets = nil
	if p, err = PreviewFiles(context.Background(), conn, config.Stream{Path: "in/*.csv"}, 100); err != nil || !p.Complete || len(p.Rows) != 5 {
		t.Fatalf("%+v %v", p, err)
	}
	if p, err = PreviewFiles(context.Background(), conn, config.Stream{Path: blob.Literal("in/z[odd].csv")}, 100); err != nil || p.Files != 1 || len(p.Rows) != 1 {
		t.Fatalf("literal name: %+v %v", p, err)
	}
}
