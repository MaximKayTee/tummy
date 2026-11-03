// Package db wraps database/sql with per-engine dialects: types, quoting and SQL text.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

type Kind int

const (
	String Kind = iota
	Int
	Float
	Decimal
	Bool
	Timestamp
	Date
	JSON
	Bytes
)

func (k Kind) String() string {
	return [...]string{"string", "int", "float", "decimal", "bool", "timestamp", "date", "json", "bytes"}[k]
}

type Column struct {
	Name string
	Kind Kind
	// Type is the driver's type name, upper case; empty when unknown.
	Type string
	// Precision and Scale of a decimal source column; 0 when unknown.
	Precision, Scale int64
}

type Table struct {
	Schema string
	Name   string
}

// ParseTable splits "schema.name" or "name". Quotes around either part are stripped.
func ParseTable(s string) Table {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "."); i >= 0 {
		return Table{Schema: unquote(s[:i]), Name: unquote(s[i+1:])}
	}
	return Table{Name: unquote(s)}
}

func unquote(s string) string {
	return strings.Trim(s, `"`+"`[]")
}

func (t Table) String() string {
	if t.Schema == "" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// Plain reports names that survive String and ParseTable unchanged: no dots, quotes or brackets.
func (t Table) Plain() bool {
	return !strings.ContainsAny(t.Schema+t.Name, ".\"`[]'") && t.Name != ""
}

// DB wraps a sql.DB with its dialect.
type DB struct {
	*sql.DB
	Dialect Dialect
	Name    string
	Schema  string // default schema from the connection
}

func (d *DB) Quote(t Table) string { return QuoteTable(d.Dialect, t) }

func (d *DB) TableExists(ctx context.Context, t Table) (bool, error) {
	q, args := d.Dialect.TableExists(t, d.Schema)
	var n int
	if err := d.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return false, fmt.Errorf("%s: %w", d.Name, err)
	}
	return n > 0, nil
}

func (d *DB) ListTables(ctx context.Context, schema string) ([]Table, error) {
	q, args := d.Dialect.ListTables(schema, d.Schema)
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Table
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Schema, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// systemSchemas are left out of ListSchemas, with every pg_ schema.
var systemSchemas = []string{"information_schema", "pg_catalog", "mysql", "performance_schema", "sys"}

// ListSchemas lists the schemas holding tables or views the connection can read, the
// connection's schema first, then by name. System schemas are left out; sqlite has none.
func (d *DB) ListSchemas(ctx context.Context) ([]string, error) {
	if !d.Dialect.HasSchemas() {
		return nil, nil
	}
	rows, err := d.QueryContext(ctx, "SELECT DISTINCT table_schema FROM information_schema.tables")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		low := strings.ToLower(s)
		if slices.Contains(systemSchemas, low) || strings.HasPrefix(low, "pg_") {
			continue
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b string) int {
		switch {
		case a == b:
			return 0
		case a == d.Schema:
			return -1
		case b == d.Schema:
			return 1
		}
		return strings.Compare(a, b)
	})
	return out, rows.Err()
}

// ListAllTables lists the tables and views of every schema from ListSchemas, in that
// order; without schemas, those of the default schema.
func (d *DB) ListAllTables(ctx context.Context) ([]Table, error) {
	schemas, err := d.ListSchemas(ctx)
	if err != nil {
		return nil, err
	}
	if len(schemas) == 0 {
		schemas = []string{""}
	}
	var out []Table
	for _, s := range schemas {
		ts, err := d.ListTables(ctx, s)
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}

// PrimaryKey lists the primary-key columns of t in key order; none for a view or a table
// without one.
func (d *DB) PrimaryKey(ctx context.Context, t Table) ([]string, error) {
	q, args := d.Dialect.PrimaryKey(t, d.Schema)
	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Count is the number of rows q returns.
func (d *DB) Count(ctx context.Context, q string, args ...any) (int64, error) {
	cq := Wrap(d.Dialect, q, "COUNT(*)", "_n")
	var n int64
	if err := d.QueryRowContext(ctx, cq, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("%s: %w\n%s", d.Name, err, cq)
	}
	return n, nil
}

// Distinct lists up to limit distinct values of col in the rows of base, in order, NULL
// included. limit <= 0 means all.
func (d *DB) Distinct(ctx context.Context, base, col string, limit int, args ...any) ([]any, error) {
	qc := d.Dialect.Quote(col)
	q := d.Dialect.OrderLimit(Wrap(d.Dialect, base, "DISTINCT "+qc, "_d"), qc, limit)
	rd, err := d.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	var out []any
	for {
		row, err := rd.Next()
		if err != nil {
			return nil, err
		}
		if row == nil {
			return out, nil
		}
		out = append(out, row[0])
	}
}

// Columns reads a table's columns through an empty select.
func (d *DB) Columns(ctx context.Context, t Table) ([]Column, error) {
	rows, err := d.QueryContext(ctx, "SELECT * FROM "+d.Quote(t)+" WHERE 1=0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return columnsOf(rows, d.Dialect)
}

func columnsOf(rows *sql.Rows, dl Dialect) ([]Column, error) {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	cols := make([]Column, len(cts))
	for i, ct := range cts {
		typ := strings.ToUpper(ct.DatabaseTypeName())
		cols[i] = Column{Name: ct.Name(), Kind: dl.KindOf(typ), Type: typ}
		if p, s, ok := ct.DecimalSize(); ok && cols[i].Kind == Decimal && p > 0 && p < 1000 && s >= 0 && s <= p {
			cols[i].Precision, cols[i].Scale = p, s
		}
	}
	return cols, nil
}

func (d *DB) Exec(ctx context.Context, q string, args ...any) error {
	_, err := d.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", d.Name, err, q)
	}
	return nil
}

func (d *DB) ExecTx(ctx context.Context, tx *sql.Tx, q string, args ...any) error {
	_, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", d.Name, err, q)
	}
	return nil
}

// ExecAll runs stmts in order, in one transaction when inTx.
func (d *DB) ExecAll(ctx context.Context, inTx bool, stmts ...string) error {
	if !inTx {
		for _, q := range stmts {
			if err := d.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", d.Name, err)
	}
	defer tx.Rollback()
	for _, q := range stmts {
		if err := d.ExecTx(ctx, tx, q); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", d.Name, err)
	}
	return nil
}

// Max returns MAX(col) from t as the column's kind, or nil if the table is empty.
func (d *DB) Max(ctx context.Context, t Table, col string) (any, error) {
	rd, err := d.Query(ctx, MaxSQL(d.Dialect, t, col))
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	row, err := rd.Next()
	if err != nil || row == nil {
		return nil, err
	}
	return row[0], nil
}

// normalize converts driver-native values into the small set tummy moves around.
// Times are UTC: targets store wall-clock time.
func normalize(v any, c Column) any {
	switch x := v.(type) {
	case []byte:
		switch {
		case c.Type == "UNIQUEIDENTIFIER" && len(x) == 16:
			return guid(x)
		case c.Kind == Bytes:
			return x
		case c.Kind == Bool && len(x) == 1: // mysql BIT(1)
			return x[0] != 0
		}
		return string(x)
	case time.Time:
		return x.UTC()
	case int64:
		if c.Kind == Bool { // sqlite BOOLEAN
			return x != 0
		}
	}
	return v
}

// guid formats SQL Server's mixed-endian uniqueidentifier bytes.
func guid(b []byte) string {
	return fmt.Sprintf("%02X%02X%02X%02X-%02X%02X-%02X%02X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// exactKinds maps whole type names; checked before the substring rules.
var exactKinds = map[string]Kind{
	"BOOL": Bool, "BOOLEAN": Bool, "BIT": Bool, "DATE": Date,
	"INT": Int, "INTEGER": Int, "INT2": Int, "INT4": Int, "INT8": Int, "TINYINT": Int, "SMALLINT": Int,
	"MEDIUMINT": Int, "BIGINT": Int, "SERIAL": Int, "SMALLSERIAL": Int, "BIGSERIAL": Int, "YEAR": Int,
	"REAL": Float, "FLOAT": Float, "FLOAT4": Float, "FLOAT8": Float, "DOUBLE": Float, "DOUBLE PRECISION": Float,
	"NUMERIC": Decimal, "DECIMAL": Decimal, "MONEY": Decimal, "SMALLMONEY": Decimal,
	"JSON": JSON, "JSONB": JSON,
	"UUID": String, "NAME": String, "UNIQUEIDENTIFIER": String, "XML": String, "TIME": String, "TIMETZ": String,
	"INTERVAL": String, "POINT": String, "MULTIPOINT": String, "LINE": String, "LSEG": String, "BOX": String,
	"PATH": String, "POLYGON": String, "CIRCLE": String, "INET": String, "CIDR": String, "MACADDR": String,
	"GEOMETRY": String, "GEOGRAPHY": String, "HIERARCHYID": String, "SQL_VARIANT": String,
}

// kindFromTypeName maps common SQL type names across engines.
func kindFromTypeName(name string) Kind {
	n := strings.ToUpper(strings.TrimSpace(name))
	if i := strings.IndexByte(n, '('); i >= 0 {
		n = strings.TrimSpace(n[:i])
	}
	if k, ok := exactKinds[n]; ok {
		return k
	}
	switch {
	case strings.HasPrefix(n, "_"), strings.HasSuffix(n, "[]"), strings.Contains(n, "RANGE"):
		return String // arrays and ranges arrive as text
	case strings.Contains(n, "JSON"):
		return JSON
	case strings.Contains(n, "TIMESTAMP"), strings.Contains(n, "DATETIME"):
		return Timestamp
	case strings.Contains(n, "CHAR"), strings.Contains(n, "TEXT"), strings.Contains(n, "CLOB"):
		return String
	case strings.Contains(n, "BYTEA"), strings.Contains(n, "BLOB"), strings.Contains(n, "BINARY"), n == "IMAGE":
		return Bytes
	case strings.Contains(n, "NUMERIC"), strings.Contains(n, "DECIMAL"), strings.Contains(n, "MONEY"):
		return Decimal
	case strings.Contains(n, "FLOAT"), strings.Contains(n, "DOUBLE"), strings.Contains(n, "REAL"):
		return Float
	case strings.HasSuffix(n, "INT"), strings.HasSuffix(n, "INTEGER"), strings.HasSuffix(n, "SERIAL"):
		return Int // UNSIGNED BIGINT, UNSIGNED BIG INT
	}
	return String
}
