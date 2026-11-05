package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	table   Table
	cols    []Column
	perStmt int
	buf     []any
	rows    int
	Total   int64
}

func (d *DB) NewInserter(t Table, cols []Column) *Inserter {
	per := d.Dialect.MaxParams() / max(len(cols), 1)
	per = min(per, d.Dialect.MaxRows())
	return &Inserter{Label: t.String(), db: d, table: t, cols: cols, perStmt: max(per, 1)}
}

func (ins *Inserter) Add(ctx context.Context, row []any) error {
	ins.buf = append(ins.buf, row...)
	ins.rows++
	if ins.rows >= ins.perStmt {
		return ins.Flush(ctx)
	}
	return nil
}

// Flush writes the buffered rows. A failed batch is retried row by row to name the
// failing row; when every row then succeeds, the batch counts as written.
func (ins *Inserter) Flush(ctx context.Context) error {
	if ins.rows == 0 {
		return nil
	}
	d := ins.db
	if _, err := d.ExecContext(ctx, InsertSQL(d.Dialect, ins.table, ins.cols, ins.rows), ins.buf...); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s: insert into %s: %w", d.Name, ins.Label, err)
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
	d := ins.db
	n := len(ins.cols)
	one := InsertSQL(d.Dialect, ins.table, ins.cols, 1)
	for r := 0; r < ins.rows; r++ {
		if _, err := d.ExecContext(ctx, one, ins.buf[r*n:(r+1)*n]...); err != nil {
			where := fmt.Sprintf("row %d", ins.Total+int64(r)+1)
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.ColumnName != "" {
				where += ", column " + pe.ColumnName
			}
			return fmt.Errorf("%s: insert into %s: %s: %w", d.Name, ins.Label, where, err)
		}
	}
	return nil
}
