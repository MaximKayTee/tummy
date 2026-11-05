package db

import (
	"context"
	"database/sql"
	"fmt"
)

// TableStat is one table's row count and size. -1 is unknown.
type TableStat struct {
	Table Table
	Rows  int64
	Bytes int64 // table, indexes and out-of-line storage
	Exact bool  // Rows is a COUNT(*), not the catalog's estimate
}

// TableStats reads row counts and sizes of every table from the catalog in one query.
// sqlite has no row statistics: its tables are counted, and sized from their pages.
func (d *DB) TableStats(ctx context.Context) ([]TableStat, error) {
	q := d.Dialect.TableStats()
	if q == "" {
		return d.sqliteStats(ctx)
	}
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Name, err)
	}
	defer rows.Close()
	var out []TableStat
	for rows.Next() {
		var s TableStat
		var n, b sql.NullInt64
		if err := rows.Scan(&s.Table.Schema, &s.Table.Name, &n, &b); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Name, err)
		}
		s.Rows, s.Bytes = orUnknown(n), orUnknown(b)
		out = append(out, s)
	}
	return out, rows.Err()
}

func orUnknown(n sql.NullInt64) int64 {
	if !n.Valid || n.Int64 < 0 { // postgres: reltuples -1 before the first ANALYZE
		return -1
	}
	return n.Int64
}

func (d *DB) sqliteStats(ctx context.Context) ([]TableStat, error) {
	var ts []Table
	rows, err := d.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY 1")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Name, err)
	}
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%s: %w", d.Name, err)
		}
		ts = append(ts, t)
	}
	rows.Close()
	size := map[string]int64{}
	rows, err = d.QueryContext(ctx, "SELECT m.tbl_name, SUM(s.pgsize) FROM dbstat s JOIN sqlite_master m ON m.name = s.name GROUP BY 1")
	if err == nil { // dbstat is a compile option
		for rows.Next() {
			var name string
			var n int64
			if rows.Scan(&name, &n) == nil {
				size[name] = n
			}
		}
		rows.Close()
	}
	var out []TableStat
	for _, t := range ts {
		s := TableStat{Table: t, Rows: -1, Bytes: -1, Exact: true}
		if b, ok := size[t.Name]; ok {
			s.Bytes = b
		}
		if s.Rows, err = d.CountTable(ctx, t); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// CountTable is SELECT COUNT(*) of t.
func (d *DB) CountTable(ctx context.Context, t Table) (int64, error) {
	q := "SELECT COUNT(*) FROM " + d.Quote(t)
	var n int64
	if err := d.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("%s: %w\n%s", d.Name, err, q)
	}
	return n, nil
}
