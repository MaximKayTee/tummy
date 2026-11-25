// Package files reads CSV and xlsx files as a header and rows of text.
package files

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Reader yields the rows of one file under its first row, the header.
type Reader interface {
	// Header is the first row: blanks become column_N, repeats get _2, _3.
	Header() []string
	// Next is the next row with its line (CSV) or row number (xlsx); nil at the end.
	Next() ([]string, int, error)
	Close() error
}

// Kind is csv or xlsx from the name's extension, "" for neither.
func Kind(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".csv":
		return "csv"
	case ".xlsx", ".xlsm":
		return "xlsx"
	}
	return ""
}

// Open reads the file at p, of the kind its name gives. sheet picks an xlsx sheet,
// "" the first; it is an error for CSV.
func Open(p, name, sheet string) (Reader, error) {
	switch Kind(name) {
	case "csv":
		if sheet != "" {
			return nil, fmt.Errorf("%s: sheet is for xlsx files", name)
		}
		return openCSV(p, name)
	case "xlsx":
		return openXLSX(p, name, sheet)
	}
	return nil, fmt.Errorf("%s: want a .csv or .xlsx file", name)
}

// Sheets lists the sheets of an xlsx file in workbook order.
func Sheets(p string) ([]string, error) {
	f, err := excelize.OpenFile(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.GetSheetList(), nil
}

type csvReader struct {
	f      *os.File
	r      *csv.Reader
	name   string
	header []string
}

func openCSV(p, name string) (*csvReader, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(f)
	if bom, _ := br.Peek(3); bytes.Equal(bom, []byte{0xEF, 0xBB, 0xBF}) {
		br.Discard(3)
	}
	r := csv.NewReader(br)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	c := &csvReader{f: f, r: r, name: name}
	h, err := r.Read()
	switch {
	case errors.Is(err, io.EOF):
		c.header = []string{}
	case err != nil:
		f.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	default:
		c.header = Names(h)
	}
	return c, nil
}

func (c *csvReader) Header() []string { return c.header }

func (c *csvReader) Next() ([]string, int, error) {
	rec, err := c.r.Read()
	if errors.Is(err, io.EOF) {
		return nil, 0, nil
	}
	line, _ := c.r.FieldPos(0)
	if err != nil {
		return nil, line, fmt.Errorf("%s: %w", c.name, err)
	}
	return rec, line, nil
}

func (c *csvReader) Close() error { return c.f.Close() }

type xlsxReader struct {
	f      *excelize.File
	rows   *excelize.Rows
	name   string
	header []string
	n      int
}

func openXLSX(p, name, sheet string) (*xlsxReader, error) {
	f, err := excelize.OpenFile(p, excelize.Options{ShortDatePattern: "yyyy-mm-dd", LongDatePattern: "yyyy-mm-dd"})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	list := f.GetSheetList()
	if sheet == "" && len(list) > 0 {
		sheet = list[0]
	}
	found := false
	for _, s := range list {
		found = found || s == sheet
	}
	if !found {
		f.Close()
		return nil, fmt.Errorf("%s: no sheet %q (%s)", name, sheet, strings.Join(list, ", "))
	}
	rows, err := f.Rows(sheet)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	x := &xlsxReader{f: f, rows: rows, name: name, header: []string{}}
	h, _, err := x.Next()
	if err != nil {
		x.Close()
		return nil, err
	}
	if h != nil {
		x.header = Names(h)
	}
	return x, nil
}

func (x *xlsxReader) Header() []string { return x.header }

func (x *xlsxReader) Next() ([]string, int, error) {
	if !x.rows.Next() {
		if err := x.rows.Error(); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", x.name, err)
		}
		return nil, 0, nil
	}
	x.n++
	cells, err := x.rows.Columns()
	if err != nil {
		return nil, x.n, fmt.Errorf("%s: row %d: %w", x.name, x.n, err)
	}
	if cells == nil {
		cells = []string{}
	}
	return cells, x.n, nil
}

func (x *xlsxReader) Close() error {
	x.rows.Close()
	return x.f.Close()
}

// Names makes header cells usable as column names: trimmed, blanks column_N (1-based),
// repeats suffixed _2, _3 ignoring case.
func Names(cells []string) []string {
	out := make([]string, len(cells))
	seen := map[string]bool{}
	for i, c := range cells {
		n := strings.TrimSpace(c)
		if n == "" {
			n = "column_" + strconv.Itoa(i+1)
		}
		base := n
		for k := 2; seen[strings.ToLower(n)]; k++ {
			n = base + "_" + strconv.Itoa(k)
		}
		seen[strings.ToLower(n)] = true
		out[i] = n
	}
	return out
}
