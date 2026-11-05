package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// Reader streams rows from a query.
type Reader struct {
	Columns []Column
	rows    *sql.Rows
	ptrs    []any
	vals    []any
}

func (d *DB) Query(ctx context.Context, q string, args ...any) (*Reader, error) {
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", d.Name, err, q)
	}
	cols, err := columnsOf(rows, d.Dialect)
	if err != nil {
		rows.Close()
		return nil, err
	}
	r := &Reader{Columns: cols, rows: rows, ptrs: make([]any, len(cols)), vals: make([]any, len(cols))}
	for i := range r.vals {
		r.ptrs[i] = &r.vals[i]
	}
	return r, nil
}

// Next reads the next row into a fresh slice. Returns nil at the end.
func (r *Reader) Next() ([]any, error) {
	if !r.rows.Next() {
		return nil, r.rows.Err()
	}
	if err := r.rows.Scan(r.ptrs...); err != nil {
		return nil, err
	}
	out := make([]any, len(r.vals))
	for i, v := range r.vals {
		out[i] = normalize(v, r.Columns[i])
	}
	return out, nil
}

func (r *Reader) Close() error { return r.rows.Close() }

// CreateTable issues CREATE TABLE with the dialect's types. keys get key-sized string types.
func (d *DB) CreateTable(ctx context.Context, t Table, cols []Column, keys []string) error {
	if err := d.EnsureSchema(ctx, t.Schema); err != nil {
		return err
	}
	return d.Exec(ctx, CreateTableSQL(d.Dialect, t, cols, keys))
}

// EnsureSchema creates schema on engines that have them, once per connection.
func (d *DB) EnsureSchema(ctx context.Context, schema string) error {
	if schema == "" || !d.Dialect.HasSchemas() {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.schemas[schema] {
		return nil
	}
	if q := d.Dialect.CreateSchema(schema); q != "" {
		if err := d.Exec(ctx, q); err != nil {
			return fmt.Errorf("create schema %s: %w", schema, err)
		}
	}
	if d.schemas == nil {
		d.schemas = map[string]bool{}
	}
	d.schemas[schema] = true
	return nil
}

// DropTable drops t if it exists.
func (d *DB) DropTable(ctx context.Context, t Table) error {
	ok, err := d.TableExists(ctx, t)
	if err != nil || !ok {
		return err
	}
	return d.Exec(ctx, DropTableSQL(d.Dialect, t))
}

// Inserter batches multi-row INSERTs into t.
type Inserter struct {
	// Label names the table in errors; defaults to the table.
	Label   string
	db      *DB
	ex      execer
	table   Table
	cols    []Column
	perStmt int
	copy    bool // postgres: COPY instead of INSERT
	buf     []any
	rows    int
	Total   int64
	strip   bool
	// Stripped counts the values a NUL byte was removed from (RejectsNUL targets);
	// Repaired the values whose invalid UTF-8 bytes became U+FFFD.
	Stripped int64
	Repaired int64
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func (d *DB) NewInserter(t Table, cols []Column) *Inserter {
	per := d.Dialect.MaxParams() / max(len(cols), 1)
	per = min(per, d.Dialect.MaxRows())
	_, copy := d.Dialect.(postgres)
	if copy {
		per = copyRows
	}
	return &Inserter{Label: t.String(), db: d, ex: d.DB, table: t, cols: cols, perStmt: max(per, 1), copy: copy, strip: d.Dialect.RejectsNUL()}
}

func (ins *Inserter) Add(ctx context.Context, row []any) error {
	if ins.strip {
		for i, v := range row {
			str, ok := v.(string)
			if !ok {
				continue
			}
			if !utf8.ValidString(str) { // bytes of a binary or non-Unicode column read as text
				str = strings.ToValidUTF8(str, "\uFFFD")
				row[i] = str
				ins.Repaired++
			}
			if strings.IndexByte(str, 0) >= 0 {
				row[i] = strings.ReplaceAll(str, "\x00", "")
				ins.Stripped++
			}
		}
	}
	ins.buf = append(ins.buf, row...)
	ins.rows++
	if ins.rows >= ins.perStmt {
		return ins.Flush(ctx)
	}
	return nil
}

// Flush writes the buffered rows: one COPY on postgres, one multi-row INSERT
// elsewhere. A failed batch is retried row by row to name the failing row; when
// every row then succeeds, the batch counts as written.
func (ins *Inserter) Flush(ctx context.Context) error {
	if ins.rows == 0 {
		return nil
	}
	d := ins.db
	var err error
	if ins.copy {
		err = ins.copyFlush(ctx)
	} else {
		_, err = ins.ex.ExecContext(ctx, InsertSQL(d.Dialect, ins.table, ins.cols, ins.rows), ins.buf...)
	}
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("%s: insert into %s: %w", d.Name, ins.Label, err)
		case ins.rows == 1:
			return ins.rowErr(0, err)
		}
		if rerr := ins.rowByRow(ctx); rerr != nil {
			return rerr
		}
	}
	ins.Total += int64(ins.rows)
	ins.buf = ins.buf[:0]
	ins.rows = 0
	return nil
}

func (ins *Inserter) rowByRow(ctx context.Context) error {
	n := len(ins.cols)
	one := InsertSQL(ins.db.Dialect, ins.table, ins.cols, 1)
	for r := 0; r < ins.rows; r++ {
		if _, err := ins.ex.ExecContext(ctx, one, ins.buf[r*n:(r+1)*n]...); err != nil {
			return ins.rowErr(r, err)
		}
	}
	return nil
}

// rowErr names buffered row r, and its column when the database says which, in the
// error of a one-row insert.
func (ins *Inserter) rowErr(r int, err error) error {
	where := fmt.Sprintf("row %d", ins.Total+int64(r)+1)
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		if pe.ColumnName != "" {
			where += ", column " + pe.ColumnName
		} else if c, ok := ins.paramColumn(pe.Where); ok {
			where += ", column " + c.Name
			if c.Type != "" {
				where += " (source type " + c.Type + ")"
			}
		}
	}
	return fmt.Errorf("%s: insert into %s: %s: %w", ins.db.Name, ins.Label, where, err)
}

// Scratch is a table that lives inside a transaction that is always rolled back, so
// rows can be test-loaded with the database left as it was.
type Scratch struct {
	*Inserter
	tx   *sql.Tx
	drop string
}

// BeginScratch creates t as CreateTable would, schema included, in a new transaction.
// Where DDL commits (mysql) t is instead a temporary table in the session's database,
// dropped by Close. Rows go in one per statement: the first bad row fails on its own,
// with no retry in a transaction the error has aborted.
func (d *DB) BeginScratch(ctx context.Context, t Table, cols []Column, keys []string) (*Scratch, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Name, err)
	}
	sc := &Scratch{tx: tx}
	var stmts []string
	if d.Dialect.TxDDL() {
		if q := d.Dialect.CreateSchema(t.Schema); t.Schema != "" && q != "" {
			stmts = append(stmts, q)
		}
		stmts = append(stmts, CreateTableSQL(d.Dialect, t, cols, keys))
	} else {
		t.Schema = ""
		stmts = append(stmts, "CREATE TEMPORARY "+strings.TrimPrefix(CreateTableSQL(d.Dialect, t, cols, keys), "CREATE "))
		sc.drop = "DROP TEMPORARY TABLE " + QuoteTable(d.Dialect, t)
	}
	for _, q := range stmts {
		if err := d.ExecTx(ctx, tx, q); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	sc.Inserter = &Inserter{Label: t.String(), db: d, ex: tx, table: t, cols: cols, perStmt: 1, strip: d.Dialect.RejectsNUL()}
	return sc, nil
}

// Close drops a temporary table and rolls the transaction back.
func (sc *Scratch) Close() error {
	if sc.drop != "" {
		sc.tx.Exec(sc.drop)
	}
	return sc.tx.Rollback()
}

var bindParam = regexp.MustCompile(`parameter \$(\d+)`)

// paramColumn maps Postgres's "unnamed portal parameter $N" context, set on errors
// converting a bound value such as bad UTF-8, to the column of a one-row insert.
func (ins *Inserter) paramColumn(where string) (Column, bool) {
	m := bindParam.FindStringSubmatch(where)
	if m == nil {
		return Column{}, false
	}
	i, err := strconv.Atoi(m[1])
	if err != nil || i < 1 || i > len(ins.cols) {
		return Column{}, false
	}
	return ins.cols[i-1], true
}
