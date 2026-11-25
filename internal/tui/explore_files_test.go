package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"tummy/internal/config"
)

// fileHome is testHome with a FILES connection on a fake container.
func fileHome(t *testing.T, files map[string]string) Options {
	t.Helper()
	opts := testHome(t)
	mod := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC).Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sig") != "s" {
			w.WriteHeader(403)
			return
		}
		if r.URL.Path == "/c" {
			var names []string
			for n := range files {
				if strings.HasPrefix(n, r.URL.Query().Get("prefix")) {
					names = append(names, n)
				}
			}
			sort.Strings(names)
			fmt.Fprint(w, `<EnumerationResults><Blobs>`)
			for _, n := range names {
				fmt.Fprintf(w, `<Blob><Name>%s</Name><Properties><Last-Modified>%s</Last-Modified><Content-Length>%d</Content-Length></Properties></Blob>`, n, mod, len(files[n]))
			}
			fmt.Fprint(w, `</Blobs><NextMarker/></EnumerationResults>`)
			return
		}
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/c/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	f, err := os.OpenFile(opts.Connections, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "[conn.FILES]\ntype = \"azblob\"\nurl = %q\nauth = \"sas\"\ntoken = \"sv=1&sig=s\"\n", srv.URL+"/c")
	f.Close()
	return opts
}

func TestExploreFilesPatternPreviewAdd(t *testing.T) {
	opts := fileHome(t, map[string]string{
		"reports/file1.csv":  "id,v\n1,a\n2,b\n",
		"reports/file2.csv":  "id,v\n3,c\n",
		"reports/file3.csv":  "id,v\n4,d\n",
		"reports/file10.csv": "id,v\n5,e\n",
		"other.csv":          "x\n1\n",
	})
	e, err := newExplorer(opts, "FILES")
	if err != nil {
		t.Fatal(err)
	}
	run(e, e.start())
	if len(e.tables) != 5 || e.page != pageTables {
		t.Fatalf("files: %v %s", e.tables, e.loadErr)
	}

	// a plain filter is text; a pattern matches as a stream path
	e.update(keyRunes("/"))
	typeInto(e, "file1")
	if got := e.visibleTables(); len(got) != 2 {
		t.Fatalf("text filter: %v", got)
	}
	e.filter.SetValue("reports/file[12].csv")
	if got := e.visibleTables(); strings.Join(got, ",") != "reports/file1.csv,reports/file2.csv" {
		t.Fatalf("pattern: %v", got)
	}
	if v := strings.Join(plain(e.view(100, 20)), "\n"); !strings.Contains(v, "2 files match") {
		t.Fatalf("count:\n%s", v)
	}
	e.update(keyEnter) // leave the filter

	// p previews every match
	press1(e, keyRunes("p"))
	if e.page != pagePreview || len(e.rows) != 3 || e.fetchErr != "" {
		t.Fatalf("preview: page=%d rows=%d err=%s", e.page, len(e.rows), e.fetchErr)
	}
	head := strings.Join(plain(e.view(120, 20)), "\n")
	if !strings.Contains(head, "reports/file[12].csv") || !strings.Contains(head, "2 files") || !strings.Contains(head, "_file") {
		t.Fatalf("head:\n%s", head)
	}
	if e.steppable() || e.canBookmark() {
		t.Fatal("steps or bookmarks on files")
	}

	// + adds it as a stream with the pattern as path
	e.update(keyRunes("+"))
	e.repPick.set("name", "drops")
	e.repPick.set("target", "TGT")
	e.update(keySave)
	if e.page != pageStream || e.stream.get("name") != "file12" || e.stream.get("path") != "reports/file[12].csv" {
		t.Fatalf("stream form: page=%d name=%q path=%q", e.page, e.stream.get("name"), e.stream.get("path"))
	}
	e.stream.set("sheet", "")
	if e.update(keySave); e.page != pagePreview {
		t.Fatalf("save: %q", e.stream.err)
	}
	r := readRep(t, opts, "drops")
	if r.Source != "FILES" || r.Resolved[0].Path != "reports/file[12].csv" || r.Resolved[0].Name != "file12" {
		t.Fatalf("written: %+v", r.Resolved)
	}

	// back to the list; enter previews one file
	e.update(keyEsc)
	e.filter.SetValue("")
	for i, n := range e.visibleTables() {
		if n == "other.csv" {
			e.cur = i
		}
	}
	press1(e, keyEnter)
	if e.page != pagePreview || len(e.rows) != 1 || e.cols[0].Name != "x" {
		t.Fatalf("one file: rows=%d cols=%v err=%s", len(e.rows), e.cols, e.fetchErr)
	}
}

func TestExploreFilesNoMatch(t *testing.T) {
	opts := fileHome(t, map[string]string{"a.txt": "x"})
	e, err := newExplorer(opts, "FILES")
	if err != nil {
		t.Fatal(err)
	}
	run(e, e.start())
	e.filter.SetValue("*.csv")
	press1(e, keyRunes("p"))
	if !strings.Contains(e.fetchErr, "no .csv or .xlsx file matches *.csv") {
		t.Fatalf("err %q", e.fetchErr)
	}
}

func TestFileStreamName(t *testing.T) {
	for in, want := range map[string]string{
		"reports/file[12].csv":  "file12",
		"in/*.{csv,xlsx}":       "files",
		"Sales Report-2026.csv": "Sales_Report_2026",
	} {
		if got := fileStreamName(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func TestAddFileStreamWizard(t *testing.T) {
	opts := fileHome(t, map[string]string{"in/a.csv": "id\n1\n"})
	os.WriteFile(opts.Home+"/replications/drops.toml", []byte("source = \"FILES\"\ntarget = \"TGT\"\n[streams.a]\npath = \"in/a.csv\"\n"), 0o644)
	conns, _ := config.LoadConnections(opts.Connections)
	w, err := newAddStreamWizard(opts, conns, "drops")
	if err != nil {
		t.Fatal(err)
	}
	w.start()
	if w.stream == nil || w.stream.field("path") == nil || w.stream.field("sheet") == nil || w.stream.field("where") != nil || w.stream.field("records") != nil {
		t.Fatal("file stream form: path and sheet, no where or api keys")
	}
	w.stream.set("name", "books")
	w.stream.set("path", "in/{a,b")
	if res := send(t, w, keySave); res == wizDone {
		t.Fatal("bad pattern saved")
	}
	w.stream.set("path", "in/*.xlsx")
	w.stream.set("sheet", "Data")
	if res := send(t, w, keySave); res != wizDone {
		t.Fatalf("save: %q %q", w.stream.err, w.err)
	}
	r := readRep(t, opts, "drops")
	if len(r.Resolved) != 2 || r.Resolved[1].Path != "in/*.xlsx" || r.Resolved[1].Sheet != "Data" {
		t.Fatalf("written: %+v", r.Resolved)
	}
}

func TestConnWizardAzblob(t *testing.T) {
	opts := fileHome(t, map[string]string{"in/a.csv": "id\n1\n", "b.csv": "x\n"})
	w, err := newConnWizard(opts, "FILES")
	if err != nil {
		t.Fatal(err)
	}
	if w.form.get("container") == "" || w.form.get("blob_auth") != "sas" || w.form.get("token_sas") != "sv=1&sig=s" || w.form.shown("read_only") || w.form.shown("host") {
		t.Fatalf("fields: %q %q %q", w.form.get("container"), w.form.get("blob_auth"), w.form.get("token_sas"))
	}
	c := w.conn()
	if c.Type != "azblob" || c.Auth != "sas" || c.Token != "sv=1&sig=s" || c.ReadOnly {
		t.Fatalf("conn: %+v", c)
	}
	msg := probeNow(c)
	w.update(msg)
	w.step = connTest
	if v := strings.Join(plain(w.view(100, 20)), "\n"); msg.Err != nil || !strings.Contains(v, "2 files") || !strings.Contains(v, "in/a.csv") {
		t.Fatalf("test: %v\n%s", msg.Err, v)
	}
	w.form.set("blob_auth", "managed_identity")
	if c := w.conn(); c.Token != "" || c.Auth != "managed_identity" {
		t.Fatalf("token kept without sas: %+v", c)
	}
	w.form.set("container", "https://acct.blob.core.windows.net/c?sig=x")
	if w.form.validate() {
		t.Fatal("a url with a query accepted")
	}
}
