package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

func readAll(t *testing.T, r Reader) [][]string {
	t.Helper()
	var out [][]string
	for {
		row, _, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if row == nil {
			return out
		}
		out = append(out, row)
	}
}

func TestCSV(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.csv")
	body := "\xEF\xBB\xBFid, name ,,Name\n1,\"two\nlines\",x\n2,\"say \"\"hi\"\"\",,,extra\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(p, "dir/A.CSV", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := strings.Join(r.Header(), "|"); got != "id|name|column_3|Name_2" {
		t.Fatalf("header %q", got)
	}
	rows := readAll(t, r)
	if len(rows) != 2 || rows[0][1] != "two\nlines" || rows[1][1] != `say "hi"` || len(rows[1]) != 5 {
		t.Fatalf("rows %q", rows)
	}
	if _, err := Open(p, "a.csv", "Sheet1"); err == nil {
		t.Fatal("sheet on a csv is accepted")
	}
}

func TestEmptyCSV(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.csv")
	os.WriteFile(p, nil, 0o600)
	r, err := Open(p, "e.csv", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.Header()) != 0 || len(readAll(t, r)) != 0 {
		t.Fatal("empty file has rows")
	}
}

func TestXLSX(t *testing.T) {
	p := filepath.Join(t.TempDir(), "b.xlsx")
	f := excelize.NewFile()
	f.SetSheetName("Sheet1", "Summary")
	f.SetSheetRow("Summary", "A1", &[]any{"only"})
	f.NewSheet("Data")
	f.SetSheetRow("Data", "A1", &[]any{"id", "when", "amount", ""})
	f.SetSheetRow("Data", "A2", &[]any{1, time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), 12.5})
	f.SetSheetRow("Data", "A3", &[]any{2, nil, nil, "tail"})
	if err := f.SaveAs(p); err != nil {
		t.Fatal(err)
	}
	f.Close()

	sheets, err := Sheets(p)
	if err != nil || strings.Join(sheets, ",") != "Summary,Data" {
		t.Fatalf("sheets %v %v", sheets, err)
	}
	r, err := Open(p, "b.xlsx", "Data")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := strings.Join(r.Header(), "|"); got != "id|when|amount" {
		t.Fatalf("header %q", got)
	}
	rows := readAll(t, r)
	if len(rows) != 2 || rows[0][0] != "1" || rows[0][1] != "2026-11-24" || rows[0][2] != "12.5" || rows[1][3] != "tail" {
		t.Fatalf("rows %q", rows)
	}

	first, err := Open(p, "b.xlsx", "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if first.Header()[0] != "only" {
		t.Fatalf("default sheet header %q", first.Header())
	}
	if _, err := Open(p, "b.xlsx", "Nope"); err == nil || !strings.Contains(err.Error(), "Summary, Data") {
		t.Fatalf("unknown sheet: %v", err)
	}
}

func TestKind(t *testing.T) {
	for name, want := range map[string]string{"a.csv": "csv", "A.XLSX": "xlsx", "a.json": "", "csv": ""} {
		if got := Kind(name); got != want {
			t.Errorf("%s: %q", name, got)
		}
	}
	if _, err := Open("x", "a.json", ""); err == nil {
		t.Error("json accepted")
	}
}
