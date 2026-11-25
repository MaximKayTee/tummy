package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tummy/internal/blob"
	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/files"
)

// Columns an azblob stream adds to each row.
const (
	FileColumn     = "_file"     // blob name
	RowColumn      = "_row"      // row within the file, from 1, blank rows not counted
	ModifiedColumn = "_modified" // the blob's last-modified time
)

// openBlob downloads the files matching the stream's path to a temp directory, then
// serves their rows: the union of their headers as text, plus _file, _row, _modified.
// With update_key = "_modified" an incremental run downloads only files modified
// since the watermark.
func (e *Engine) openBlob(ctx context.Context, cl *blob.Client, tgt *db.DB, s stream, target db.Table, exists bool, runID int64, timeout time.Duration) (*rowSource, error) {
	if err := CheckFileStream(s.ResolvedStream); err != nil {
		return nil, err
	}
	wm, err := watermark(ctx, tgt, s, target, exists)
	if err != nil {
		return nil, err
	}
	fctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		fctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	inclusive := len(s.PrimaryKey) > 0
	newer := func(v any) bool {
		c, ok := compare(v, wm)
		return ok && (c > 0 || inclusive && c == 0)
	}
	matched, skipped, err := matchFiles(fctx, cl, s.Path)
	if err != nil {
		return nil, err
	}
	for _, name := range skipped {
		e.log().Warn("file skipped: not .csv or .xlsx", "stream", s.Name, "file", name)
	}
	var todo []blob.Blob
	for _, b := range matched {
		if wm == nil || !strings.EqualFold(s.UpdateKey, ModifiedColumn) || newer(b.Modified) {
			todo = append(todo, b)
		}
	}
	if len(todo) == 0 {
		e.log().Info("no files to read", "stream", s.Name, "path", s.Path)
		return &rowSource{whole: true, next: func() ([]any, error) { return nil, nil }, close: func() {}}, nil
	}

	dir, err := os.MkdirTemp("", "tummy-blob-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	local := make([]string, len(todo))
	for i, b := range todo {
		e.phase(ctx, runID, s, b.Name, PhaseFetching, int64(i))
		local[i] = filepath.Join(dir, fmt.Sprintf("%05d%s", i, filepath.Ext(b.Name)))
		if err := download(fctx, cl, b.Name, local[i]); err != nil {
			cleanup()
			return nil, err
		}
	}

	u := newUnion()
	headers := make([][]string, len(todo))
	for i, b := range todo {
		r, err := files.Open(local[i], b.Name, s.Sheet)
		if err != nil {
			cleanup()
			return nil, err
		}
		headers[i] = u.take(r.Header())
		r.Close()
	}
	cols := u.columns(s.Stream)
	ukAt := -1
	if wm != nil {
		ukAt = slices.IndexFunc(cols, func(c db.Column) bool { return strings.EqualFold(c.Name, s.UpdateKey) })
	}

	var (
		cur  *fileRows
		fi   = -1
		sent int
	)
	closeCur := func() {
		if cur != nil {
			cur.close()
			cur = nil
		}
	}
	next := func() ([]any, error) {
		for {
			if s.Limit > 0 && sent >= s.Limit {
				return nil, nil
			}
			if cur == nil {
				if fi++; fi >= len(todo) {
					return nil, nil
				}
				if cur, err = openRows(local[fi], todo[fi], s.Sheet, headers[fi], cols); err != nil {
					return nil, err
				}
				e.phase(ctx, runID, s, todo[fi].Name, PhaseLoading, int64(sent))
			}
			row, err := cur.next()
			if err != nil {
				return nil, err
			}
			if row == nil {
				closeCur()
				continue
			}
			if ukAt >= 0 && !newer(row[ukAt]) {
				continue
			}
			sent++
			return row, nil
		}
	}
	return &rowSource{cols: cols, next: next, close: func() { closeCur(); cleanup() }}, nil
}

// FilePreview is the first rows of the files a stream path matches.
type FilePreview struct {
	Cols     []db.Column
	Rows     [][]any
	Files    int      // files matched
	Read     int      // files read for the rows
	Skipped  []string // matched but not .csv or .xlsx
	Complete bool     // every row of every file
}

// PreviewFiles reads up to n rows of the files matching s.Path, in name order, one file
// at a time: columns are those of the files read. c is an expanded azblob connection.
func PreviewFiles(ctx context.Context, c *config.Conn, s config.Stream, n int) (FilePreview, error) {
	var out FilePreview
	if _, err := blob.Patterns(s.Path); err != nil {
		return out, fmt.Errorf("path %q: %w", s.Path, err)
	}
	cl, err := blob.New(c)
	if err != nil {
		return out, err
	}
	matched, skipped, err := matchFiles(ctx, cl, s.Path)
	if err != nil {
		return out, err
	}
	out.Files, out.Skipped = len(matched), skipped
	dir, err := os.MkdirTemp("", "tummy-preview-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(dir)

	type part struct {
		b      blob.Blob
		local  string
		header []string
	}
	u := newUnion()
	var parts []part
	have := 0
	for i, b := range matched {
		if have >= n {
			break
		}
		local := filepath.Join(dir, fmt.Sprintf("%05d%s", i, filepath.Ext(b.Name)))
		if err := download(ctx, cl, b.Name, local); err != nil {
			return out, err
		}
		r, err := files.Open(local, b.Name, s.Sheet)
		if err != nil {
			return out, err
		}
		p := part{b: b, local: local, header: u.take(r.Header())}
		for have < n {
			cells, _, err := r.Next()
			if err != nil {
				r.Close()
				return out, err
			}
			if cells == nil {
				break
			}
			if !blankCells(cells) {
				have++
			}
		}
		r.Close()
		parts = append(parts, p)
	}
	s.Limit = 0
	out.Cols = u.columns(s)
	for _, p := range parts {
		fr, err := openRows(p.local, p.b, s.Sheet, p.header, out.Cols)
		if err != nil {
			return out, err
		}
		for len(out.Rows) < n {
			row, err := fr.next()
			if err != nil {
				fr.close()
				return out, err
			}
			if row == nil {
				break
			}
			out.Rows = append(out.Rows, row)
		}
		more, _ := fr.next()
		fr.close()
		out.Read++
		if more != nil {
			return out, nil
		}
	}
	out.Complete = out.Read == len(matched)
	return out, nil
}

// matchFiles lists the blobs matching path: .csv and .xlsx ones, and the names of the rest.
func matchFiles(ctx context.Context, cl *blob.Client, path string) ([]blob.Blob, []string, error) {
	listed, err := cl.List(ctx, blob.Prefix(path))
	if err != nil {
		return nil, nil, err
	}
	var ok []blob.Blob
	var skipped []string
	for _, b := range blob.Filter(listed, path) {
		if files.Kind(b.Name) == "" {
			skipped = append(skipped, b.Name)
		} else {
			ok = append(ok, b)
		}
	}
	return ok, skipped, nil
}

// union merges file headers into one column list: a name is matched ignoring case and
// keeps its first spelling; a header named like an added column gets _2.
type union struct {
	names []string
	pos   map[string]int // lower name -> index; -1 for the added columns
}

func newUnion() *union {
	u := &union{pos: map[string]int{}}
	for _, n := range []string{FileColumn, RowColumn, ModifiedColumn} {
		u.pos[strings.ToLower(n)] = -1
	}
	return u
}

// take adds a file's header and returns the column each of its cells feeds.
func (u *union) take(header []string) []string {
	out := make([]string, len(header))
	for i, h := range header {
		if p, ok := u.pos[strings.ToLower(h)]; ok && p >= 0 {
			out[i] = u.names[p]
			continue
		}
		n := h
		for k := 2; ; k++ {
			if _, taken := u.pos[strings.ToLower(n)]; !taken {
				break
			}
			n = fmt.Sprintf("%s_%d", h, k)
		}
		u.pos[strings.ToLower(n)] = len(u.names)
		u.names = append(u.names, n)
		out[i] = n
	}
	return out
}

// columns are the stream's columns: the headers as text, then the added ones, with
// select applied.
func (u *union) columns(s config.Stream) []db.Column {
	all := append(slices.Clone(u.names), FileColumn, RowColumn, ModifiedColumn)
	var cols []db.Column
	for _, n := range s.Columns(all) {
		k := db.String
		switch {
		case strings.EqualFold(n, RowColumn):
			k = db.Int
		case strings.EqualFold(n, ModifiedColumn):
			k = db.Timestamp
		}
		cols = append(cols, db.Column{Name: n, Kind: k})
	}
	return cols
}

// fileRows reads one downloaded file as rows of cols.
type fileRows struct {
	r     files.Reader
	b     blob.Blob
	into  []int // header cell -> column, -1 not loaded
	ncols int
	added map[string]int
	n     int
}

func openRows(local string, b blob.Blob, sheet string, header []string, cols []db.Column) (*fileRows, error) {
	r, err := files.Open(local, b.Name, sheet)
	if err != nil {
		return nil, err
	}
	at := map[string]int{}
	for j, c := range cols {
		at[strings.ToLower(c.Name)] = j
	}
	f := &fileRows{r: r, b: b, into: make([]int, len(header)), ncols: len(cols), added: map[string]int{}}
	for k, h := range header {
		f.into[k] = -1
		if j, ok := at[strings.ToLower(h)]; ok {
			f.into[k] = j
		}
	}
	for _, n := range []string{FileColumn, RowColumn, ModifiedColumn} {
		if j, ok := at[strings.ToLower(n)]; ok {
			f.added[n] = j
		}
	}
	return f, nil
}

// next is the next row that is not blank, nil at the end. Empty cells are null.
func (f *fileRows) next() ([]any, error) {
	for {
		cells, line, err := f.r.Next()
		if err != nil || cells == nil {
			return nil, err
		}
		if blankCells(cells) {
			continue
		}
		row := make([]any, f.ncols)
		for k, v := range cells {
			if v == "" {
				continue
			}
			if k >= len(f.into) {
				return nil, fmt.Errorf("%s: row at %d has more cells than the header", f.b.Name, line)
			}
			if f.into[k] >= 0 {
				row[f.into[k]] = v
			}
		}
		f.n++
		for name, v := range map[string]any{FileColumn: f.b.Name, RowColumn: int64(f.n), ModifiedColumn: f.b.Modified} {
			if j, ok := f.added[name]; ok {
				row[j] = v
			}
		}
		return row, nil
	}
}

func (f *fileRows) close() { f.r.Close() }

func blankCells(cells []string) bool {
	for _, c := range cells {
		if c != "" {
			return false
		}
	}
	return true
}

// CheckFileStream checks a stream of an azblob source: a path pattern, no sql.
func CheckFileStream(s config.ResolvedStream) error {
	switch {
	case s.Path == "":
		return fmt.Errorf("stream %s: path is required: the file pattern, as in reports/*.csv", s.Name)
	case s.SQL != "":
		return fmt.Errorf("stream %s: sql is for database sources", s.Name)
	case s.ForEach != "":
		return fmt.Errorf("stream %s: for_each is for api sources", s.Name)
	}
	if _, err := blob.Patterns(s.Path); err != nil {
		return fmt.Errorf("stream %s: path %q: %w", s.Name, s.Path, err)
	}
	return nil
}

func download(ctx context.Context, cl *blob.Client, name, to string) error {
	f, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := cl.Download(ctx, name, f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
