package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tummy/internal/config"
)

// fake serves List Blobs and Get Blob for one container, two blobs per page.
type fake struct {
	files    map[string]string
	modified time.Time
	auth     string // header or query the requests must carry
	status   int    // when set, every request gets it once
	hits     int
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits++
	if f.status != 0 {
		st := f.status
		f.status = 0
		w.WriteHeader(st)
		return
	}
	if r.Header.Get("x-ms-version") == "" {
		w.WriteHeader(400)
		return
	}
	if r.Header.Get("Authorization") != f.auth && r.URL.Query().Get("sig") != f.auth {
		w.Header().Set("x-ms-error-code", "AuthenticationFailed")
		w.WriteHeader(403)
		fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>AuthorizationPermissionMismatch</Code><Message>This request is not authorized.
RequestId:x</Message></Error>`)
		return
	}
	name, _ := strings.CutPrefix(r.URL.Path, "/transfer/")
	if r.URL.Path == "/transfer" {
		f.list(w, r)
		return
	}
	body, ok := f.files[name]
	if !ok {
		w.WriteHeader(404)
		fmt.Fprint(w, `<Error><Code>BlobNotFound</Code><Message>The specified blob does not exist.</Message></Error>`)
		return
	}
	fmt.Fprint(w, body)
}

func (f *fake) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var names []string
	for n := range f.files {
		if strings.HasPrefix(n, q.Get("prefix")) {
			names = append(names, n)
		}
	}
	sortStrings(names)
	start := 0
	if m := q.Get("marker"); m != "" {
		fmt.Sscan(m, &start)
	}
	end := min(start+2, len(names))
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><EnumerationResults><Blobs>`)
	for _, n := range names[start:end] {
		fmt.Fprintf(&b, `<Blob><Name>%s</Name><Properties><Last-Modified>%s</Last-Modified><Content-Length>%d</Content-Length></Properties></Blob>`,
			n, f.modified.Format(http.TimeFormat), len(f.files[n]))
	}
	b.WriteString(`</Blobs><NextMarker>`)
	if end < len(names) {
		fmt.Fprint(&b, end)
	}
	b.WriteString(`</NextMarker></EnumerationResults>`)
	fmt.Fprint(w, b.String())
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func newFake(t *testing.T, auth string) (*fake, string) {
	f := &fake{
		files: map[string]string{
			"reports/file1.csv":      "a,b\n1,2\n",
			"reports/file2.csv":      "a,b\n3,4\n",
			"reports/file3.csv":      "a,b\n5,6\n",
			"reports/other.xlsx":     "x",
			"reports/with space.csv": "a\n1\n",
			"top.csv":                "a\n1\n",
		},
		modified: time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC),
		auth:     auth,
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL + "/transfer"
}

func tokenClient(t *testing.T, url string) *Client {
	t.Helper()
	cl, err := New(&config.Conn{Name: "T", Type: "azblob", URL: url, Auth: config.BlobManagedIdentity})
	if err != nil {
		t.Fatal(err)
	}
	return cl.WithToken(func(context.Context) (string, error) { return "tok", nil })
}

func TestListPagesAndPrefix(t *testing.T) {
	_, u := newFake(t, "Bearer tok")
	cl := tokenClient(t, u)
	all, err := cl.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("listed %d, want 6 over three pages", len(all))
	}
	if !all[0].Modified.Equal(time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)) || all[0].Size != 8 {
		t.Fatalf("properties: %+v", all[0])
	}
	some, err := cl.List(context.Background(), "reports/file")
	if err != nil {
		t.Fatal(err)
	}
	if len(some) != 3 || some[2].Name != "reports/file3.csv" {
		t.Fatalf("prefix: %+v", some)
	}
}

func TestDownloadEscapesName(t *testing.T) {
	_, u := newFake(t, "Bearer tok")
	cl := tokenClient(t, u)
	var buf bytes.Buffer
	if err := cl.Download(context.Background(), "reports/with space.csv", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "a\n1\n" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestSAS(t *testing.T) {
	_, u := newFake(t, "sasig")
	cl, err := New(&config.Conn{Name: "T", Type: "azblob", URL: u, Auth: config.BlobSAS, Token: "?sv=2022-11-02&sp=rl&sig=sasig"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.List(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := New(&config.Conn{Name: "T", Type: "azblob", URL: u, Auth: config.BlobSAS, Token: "sv=1"}); err == nil {
		t.Fatal("a token without sig= is accepted")
	}
}

func TestErrorCodeAndNoSecret(t *testing.T) {
	_, u := newFake(t, "Bearer other")
	cl := tokenClient(t, u)
	_, err := cl.List(context.Background(), "")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 403 || he.Code != "AuthorizationPermissionMismatch" {
		t.Fatalf("err %v", err)
	}
	if want := "403 Forbidden: AuthorizationPermissionMismatch: This request is not authorized."; !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("err %q, want suffix %q", err, want)
	}

	_, u = newFake(t, "right")
	cl, _ = New(&config.Conn{Name: "T", Type: "azblob", URL: u, Auth: config.BlobSAS, Token: "sig=wrongsecret"})
	_, err = cl.List(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), "wrongsecret") {
		t.Fatalf("err %v", err)
	}
	cl, _ = New(&config.Conn{Name: "T", Type: "azblob", URL: "http://127.0.0.1:1/transfer", Auth: config.BlobSAS, Token: "sig=wrongsecret"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = cl.List(ctx, "")
	if err == nil || strings.Contains(err.Error(), "wrongsecret") {
		t.Fatalf("network error shows the SAS: %v", err)
	}
}

func TestRetries503(t *testing.T) {
	f, u := newFake(t, "Bearer tok")
	f.status = 503
	cl := tokenClient(t, u)
	if _, err := cl.List(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if f.hits < 2 {
		t.Fatalf("hits %d", f.hits)
	}
}

func TestPatterns(t *testing.T) {
	names := []string{"reports/file1.csv", "reports/file2.csv", "reports/file3.csv", "reports/file10.csv", "reports/x/file1.csv", "top.csv", "reports/a.xlsx"}
	cases := []struct {
		pat  string
		want []string
	}{
		{"reports/file[123].csv", []string{"reports/file1.csv", "reports/file2.csv", "reports/file3.csv"}},
		{"reports/file*.csv", []string{"reports/file1.csv", "reports/file2.csv", "reports/file3.csv", "reports/file10.csv"}},
		{"reports/{file1,file10}.csv", []string{"reports/file1.csv", "reports/file10.csv"}},
		{"reports/*.{csv,xlsx}", []string{"reports/file1.csv", "reports/file2.csv", "reports/file3.csv", "reports/file10.csv", "reports/a.xlsx"}},
		{"*.csv", []string{"top.csv"}},
		{"reports/*/file1.csv", []string{"reports/x/file1.csv"}},
		{"top.csv", []string{"top.csv"}},
	}
	var blobs []Blob
	for _, n := range names {
		blobs = append(blobs, Blob{Name: n})
	}
	for _, c := range cases {
		var got []string
		for _, b := range Filter(blobs, c.pat) {
			got = append(got, b.Name)
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s: got %v, want %v", c.pat, got, c.want)
		}
	}
	for _, bad := range []string{"a{b", "a}b", "a[b", "{a,{b}}"} {
		if _, err := Patterns(bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
	if p := Prefix("reports/file[123].csv"); p != "reports/file" {
		t.Errorf("prefix %q", p)
	}
	if p := Prefix("reports/{a,b}.csv"); p != "reports/" {
		t.Errorf("prefix %q", p)
	}
}

func TestContainerURL(t *testing.T) {
	for _, bad := range []string{"", "acct.blob.core.windows.net/transfer", "https://acct.blob.core.windows.net", "https://acct.blob.core.windows.net/transfer/x", "https://acct.blob.core.windows.net/transfer?sig=1"} {
		if _, err := config.ContainerURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if u, err := config.ContainerURL("https://acct.blob.core.windows.net/transfer/"); err != nil || u.Path != "/transfer" {
		t.Errorf("trailing slash: %v %v", u, err)
	}
}
