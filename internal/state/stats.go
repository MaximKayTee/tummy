package state

import (
	"context"
	"database/sql"
	"time"
)

// TableStat is one reading of a table's rows and size. -1 is unknown.
type TableStat struct {
	Schema, Table string
	Rows, Bytes   int64
	Exact         bool // Rows is a COUNT(*), not a catalog estimate
	Refreshed     time.Time
	// PrevRows is the reading before, -1 without one.
	PrevRows int64
}

// ConnStats sums the latest full refresh of a connection.
type ConnStats struct {
	Tables    int
	Bytes     int64 // -1 when no table had a size
	Refreshed time.Time
}

// SaveTableStats records readings taken at at. full marks a refresh of every table,
// which ConnStats sums; a single count is not full.
func (s *Store) SaveTableStats(ctx context.Context, conn string, at time.Time, full bool, stats []TableStat) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO table_stats (conn, schema_name, table_name, refreshed, full, rows, bytes, exact) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, t := range stats {
		if _, err := stmt.ExecContext(ctx, conn, t.Schema, t.Table, ts(at), full, known(t.Rows), known(t.Bytes), t.Exact); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func known(n int64) any {
	if n < 0 {
		return nil
	}
	return n
}

// TableStats is the latest reading of each table of conn, with the rows of the one before.
func (s *Store) TableStats(ctx context.Context, conn string) ([]TableStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT schema_name, table_name, rows, bytes, exact, refreshed, prev FROM (
  SELECT *, LAG(rows, 1, -1) OVER w AS prev,
    ROW_NUMBER() OVER (PARTITION BY schema_name, table_name ORDER BY refreshed DESC, id DESC) AS rn
  FROM table_stats WHERE conn = ?
  WINDOW w AS (PARTITION BY schema_name, table_name ORDER BY refreshed, id)
) WHERE rn = 1 ORDER BY schema_name, table_name`, conn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableStat
	for rows.Next() {
		var t TableStat
		var n, b, prev sql.NullInt64
		var at string
		if err := rows.Scan(&t.Schema, &t.Table, &n, &b, &t.Exact, &at, &prev); err != nil {
			return nil, err
		}
		t.Rows, t.Bytes, t.PrevRows = orUnknown(n), orUnknown(b), orUnknown(prev)
		if t.Refreshed, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func orUnknown(n sql.NullInt64) int64 {
	if !n.Valid {
		return -1
	}
	return n.Int64
}

// ConnStats sums each connection's latest full refresh.
func (s *Store) ConnStats(ctx context.Context) (map[string]ConnStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.conn, count(*), sum(t.bytes), t.refreshed FROM table_stats t
JOIN (SELECT conn, max(refreshed) AS at FROM table_stats WHERE full = 1 GROUP BY conn) l ON l.conn = t.conn AND l.at = t.refreshed
WHERE t.full = 1 GROUP BY t.conn, t.refreshed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ConnStats{}
	for rows.Next() {
		var conn, at string
		var c ConnStats
		var b sql.NullInt64
		if err := rows.Scan(&conn, &c.Tables, &b, &at); err != nil {
			return nil, err
		}
		c.Bytes = orUnknown(b)
		if c.Refreshed, err = parseTS(at); err != nil {
			return nil, err
		}
		out[conn] = c
	}
	return out, rows.Err()
}
