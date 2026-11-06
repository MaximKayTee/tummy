package db

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/microsoft/go-mssqldb/azuread"
)

// Dialect is everything that differs between engines.
type Dialect interface {
	Name() string
	Quote(ident string) string
	Placeholder(i int) string
	MaxParams() int
	MaxRows() int
	// RejectsNUL: text values with a NUL byte cannot be stored; the inserter drops the byte.
	RejectsNUL() bool
	// BinaryUDTs maps the column types the driver returns in the engine's own binary
	// serialisation to a SELECT expression (%s = the column) that returns them as text.
	BinaryUDTs() map[string]string
	// TxDDL: CREATE TABLE and CREATE SCHEMA roll back with their transaction.
	TxDDL() bool
	// TableStats lists schema, table, rows and bytes of every table from the catalog;
	// "" when the engine keeps no row statistics.
	TableStats() string
	HasSchemas() bool
	KindOf(typeName string) Kind
	// TypeName is the column type for CREATE TABLE and ALTER TABLE ADD. Keys get index-sized types.
	TypeName(c Column, isKey bool) string
	// CastType is the type for CAST(x AS ...).
	CastType(k Kind) string
	TableExists(t Table, defaultSchema string) (string, []any)
	// CreateSchema creates schema when it does not exist; "" when the engine has none.
	CreateSchema(schema string) string
	// ListDatabases lists the databases on the server, one name per row; "" when the
	// engine has no databases beyond the schemas.
	ListDatabases() string
	ListTables(schema, defaultSchema string) (string, []any)
	// PrimaryKey lists the primary-key columns of t in key order, one per row.
	PrimaryKey(t Table, defaultSchema string) (string, []any)
	Rename(from, to Table) string
	// Truncate empties t; safe inside a transaction.
	Truncate(t Table) string
	// DeleteMatching deletes the rows of target whose keys appear in src.
	DeleteMatching(target, src Table, keys []string) string
	// Dedupe deletes the rows of t that share keys with a row that beats them:
	// higher updateKey (nulls lowest), then higher seq. updateKey may be empty.
	Dedupe(t Table, keys []string, updateKey, seq string) string
	// Swap replaces target with tmp. old is a free name for the outgoing table.
	// tx: run the statements in one transaction.
	Swap(tmp, target, old Table, targetExists bool) (stmts []string, tx bool)
	// OrderLimit adds ordering and a row cap to a complete SELECT. order is rendered
	// ("\"a\" DESC, \"b\""); either may be empty.
	OrderLimit(sql, order string, n int) string
	// Derived makes a complete SELECT legal as a derived table.
	Derived(sql string) string

	driver(p Params) (name, dsn string, err error)
	maxOpenConns() int
}

func Dialects() []string { return []string{"postgres", "mysql", "mssql", "sqlite"} }

func DialectFor(typ string) (Dialect, error) {
	switch typ {
	case "postgres", "postgresql", "pg":
		return postgres{}, nil
	case "mysql", "mariadb":
		return mysql{}, nil
	case "mssql", "sqlserver":
		return mssql{}, nil
	case "sqlite", "sqlite3":
		return sqlite{}, nil
	}
	return nil, fmt.Errorf("unknown database type %q (known: %s)", typ, strings.Join(Dialects(), ", "))
}

func quoteDouble(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// decimalType renders decimal(p,s) when the source precision fits, else def.
func decimalType(c Column, maxP, maxS int64, def string) string {
	if c.Precision > 0 && c.Precision <= maxP && c.Scale >= 0 && c.Scale <= c.Precision && c.Scale <= maxS {
		return fmt.Sprintf("decimal(%d,%d)", c.Precision, c.Scale)
	}
	return def
}

// DELETE forms: "DELETE FROM t AS a" (postgres, sqlite) or "DELETE a FROM t AS a" (mysql, mssql).
func deleteMatching(dl Dialect, target, src Table, keys []string, named bool) string {
	sub := " WHERE EXISTS (SELECT 1 FROM " + QuoteTable(dl, src) + " AS _tmp WHERE " + keysEqual(dl, "_tmp", "_tgt", keys) + ")"
	if named {
		return "DELETE _tgt FROM " + QuoteTable(dl, target) + " AS _tgt" + sub
	}
	return "DELETE FROM " + QuoteTable(dl, target) + " AS _tgt" + sub
}

func dedupeExists(dl Dialect, t Table, keys []string, updateKey, seq string, named bool) string {
	sub := " WHERE EXISTS (SELECT 1 FROM " + QuoteTable(dl, t) + " AS _b WHERE " + keysEqual(dl, "_b", "_a", keys) + " AND " + beats(dl, updateKey, seq) + ")"
	if named {
		return "DELETE _a FROM " + QuoteTable(dl, t) + " AS _a" + sub
	}
	return "DELETE FROM " + QuoteTable(dl, t) + " AS _a" + sub
}

// infoSchemaPK reads primary-key columns from information_schema; schema and table are
// the placeholders.
func infoSchemaPK(schema, table string) string {
	return "SELECT kcu.column_name FROM information_schema.table_constraints tc" +
		" JOIN information_schema.key_column_usage kcu ON kcu.constraint_schema = tc.constraint_schema" +
		" AND kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema AND kcu.table_name = tc.table_name" +
		" WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = " + schema + " AND tc.table_name = " + table +
		" ORDER BY kcu.ordinal_position"
}

// swapTx drops target and renames tmp, for engines with transactional DDL.
func swapTx(dl Dialect, tmp, target Table, exists bool) ([]string, bool) {
	var out []string
	if exists {
		out = append(out, DropTableSQL(dl, target))
	}
	return append(out, dl.Rename(tmp, target)), true
}

// ---------- postgres ----------

type postgres struct{}

func (postgres) Name() string                  { return "postgres" }
func (postgres) Quote(s string) string         { return quoteDouble(s) }
func (postgres) Placeholder(i int) string      { return "$" + strconv.Itoa(i) }
func (postgres) MaxParams() int                { return 65000 }
func (postgres) RejectsNUL() bool              { return true }
func (postgres) BinaryUDTs() map[string]string { return nil }
func (postgres) TxDDL() bool                   { return true }

// reltuples is an estimate kept by ANALYZE and autovacuum; -1 before the first.
func (postgres) TableStats() string {
	return `SELECT n.nspname, c.relname, c.reltuples::bigint, pg_total_relation_size(c.oid)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'm') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'`
}
func (postgres) MaxRows() int         { return 5000 }
func (postgres) HasSchemas() bool     { return true }
func (postgres) KindOf(n string) Kind { return kindFromTypeName(n) }
func (postgres) TypeName(c Column, _ bool) string {
	switch c.Kind {
	case Int:
		return "bigint"
	case Float:
		return "double precision"
	case Decimal:
		return "numeric"
	case Bool:
		return "boolean"
	case Timestamp:
		return "timestamp"
	case Date:
		return "date"
	case JSON:
		return "jsonb"
	case Bytes:
		return "bytea"
	}
	return "text"
}
func (d postgres) CastType(k Kind) string { return d.TypeName(Column{Kind: k}, false) }
func (postgres) TableExists(t Table, def string) (string, []any) {
	return "SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2",
		[]any{cmp.Or(t.Schema, def, "public"), t.Name}
}
func (postgres) ListDatabases() string {
	return "SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1"
}
func (d postgres) CreateSchema(s string) string { return "CREATE SCHEMA IF NOT EXISTS " + d.Quote(s) }
func (postgres) ListTables(schema, def string) (string, []any) {
	if schema == AllSchemas {
		return "SELECT table_schema, table_name FROM information_schema.tables WHERE table_type IN ('BASE TABLE','VIEW') ORDER BY 1, 2", nil
	}
	return "SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema = $1 AND table_type IN ('BASE TABLE','VIEW') ORDER BY 2",
		[]any{cmp.Or(schema, def, "public")}
}
func (postgres) PrimaryKey(t Table, def string) (string, []any) {
	return infoSchemaPK("$1", "$2"), []any{cmp.Or(t.Schema, def, "public"), t.Name}
}
func (d postgres) Rename(from, to Table) string {
	return "ALTER TABLE " + QuoteTable(d, from) + " RENAME TO " + d.Quote(to.Name)
}
func (d postgres) Truncate(t Table) string { return "TRUNCATE TABLE " + QuoteTable(d, t) }
func (d postgres) DeleteMatching(target, src Table, keys []string) string {
	return deleteMatching(d, target, src, keys, false)
}
func (d postgres) Dedupe(t Table, keys []string, uk, seq string) string {
	return dedupeExists(d, t, keys, uk, seq, false)
}
func (d postgres) Swap(tmp, target, _ Table, exists bool) ([]string, bool) {
	return swapTx(d, tmp, target, exists)
}
func (d postgres) OrderLimit(sql, order string, n int) string { return orderLimit(d, sql, order, n) }
func (postgres) Derived(sql string) string                    { return sql }
func (postgres) driver(p Params) (string, string, error)      { return "pgx", p.URL, nil }
func (postgres) maxOpenConns() int                            { return 0 }

// ---------- mysql ----------

type mysql struct{}

func (mysql) Name() string                  { return "mysql" }
func (mysql) Quote(s string) string         { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func (mysql) Placeholder(int) string        { return "?" }
func (mysql) MaxParams() int                { return 60000 }
func (mysql) RejectsNUL() bool              { return false }
func (mysql) BinaryUDTs() map[string]string { return nil }
func (mysql) TxDDL() bool                   { return false }

// table_rows is InnoDB's estimate.
func (mysql) TableStats() string {
	return `SELECT table_schema, table_name, table_rows, data_length + index_length FROM information_schema.tables
WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')`
}
func (mysql) MaxRows() int         { return 1000 } // max_allowed_packet
func (mysql) HasSchemas() bool     { return true }
func (mysql) KindOf(n string) Kind { return kindFromTypeName(n) }
func (mysql) TypeName(c Column, isKey bool) string {
	switch c.Kind {
	case Int:
		return "bigint"
	case Float:
		return "double"
	case Decimal:
		return decimalType(c, 65, 30, "decimal(38,10)")
	case Bool:
		return "tinyint(1)"
	case Timestamp:
		return "datetime(6)"
	case Date:
		return "date"
	case JSON:
		return "json"
	case Bytes:
		return "longblob"
	}
	if isKey {
		return "varchar(255)"
	}
	return "longtext"
}
func (mysql) CastType(k Kind) string {
	switch k {
	case Int, Bool:
		return "SIGNED"
	case Float:
		return "DOUBLE"
	case Decimal:
		return "DECIMAL(38,10)"
	case Timestamp:
		return "DATETIME(6)"
	case Date:
		return "DATE"
	case JSON:
		return "JSON"
	case Bytes:
		return "BINARY"
	}
	return "CHAR"
}
func (mysql) TableExists(t Table, def string) (string, []any) {
	if s := cmp.Or(t.Schema, def); s != "" {
		return "SELECT count(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?", []any{s, t.Name}
	}
	return "SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", []any{t.Name}
}
func (mysql) ListDatabases() string          { return "" }
func (d mysql) CreateSchema(s string) string { return "CREATE DATABASE IF NOT EXISTS " + d.Quote(s) }
func (mysql) ListTables(schema, def string) (string, []any) {
	if schema == AllSchemas {
		return "SELECT table_schema, table_name FROM information_schema.tables ORDER BY 1, 2", nil
	}
	if s := cmp.Or(schema, def); s != "" {
		return "SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema = ? ORDER BY 2", []any{s}
	}
	return "SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY 2", nil
}
func (mysql) PrimaryKey(t Table, def string) (string, []any) {
	const q = "SELECT column_name FROM information_schema.key_column_usage WHERE table_schema = %s AND table_name = ? AND constraint_name = 'PRIMARY' ORDER BY ordinal_position"
	if s := cmp.Or(t.Schema, def); s != "" {
		return fmt.Sprintf(q, "?"), []any{s, t.Name}
	}
	return fmt.Sprintf(q, "DATABASE()"), []any{t.Name}
}
func (d mysql) Rename(from, to Table) string {
	return "RENAME TABLE " + QuoteTable(d, from) + " TO " + QuoteTable(d, to)
}

// Truncate is a DELETE: TRUNCATE commits implicitly.
func (d mysql) Truncate(t Table) string { return "DELETE FROM " + QuoteTable(d, t) }
func (d mysql) DeleteMatching(target, src Table, keys []string) string {
	return deleteMatching(d, target, src, keys, true)
}

// Dedupe joins: a subquery may not read the table being deleted from.
func (d mysql) Dedupe(t Table, keys []string, uk, seq string) string {
	return "DELETE _a FROM " + QuoteTable(d, t) + " AS _a JOIN " + QuoteTable(d, t) + " AS _b ON " +
		keysEqual(d, "_b", "_a", keys) + " AND " + beats(d, uk, seq)
}

// Swap renames both tables in one atomic RENAME TABLE; DDL is not transactional.
func (d mysql) Swap(tmp, target, old Table, exists bool) ([]string, bool) {
	if !exists {
		return []string{d.Rename(tmp, target)}, false
	}
	return []string{
		"RENAME TABLE " + QuoteTable(d, target) + " TO " + QuoteTable(d, old) + ", " + QuoteTable(d, tmp) + " TO " + QuoteTable(d, target),
		DropTableSQL(d, old),
	}, false
}
func (d mysql) OrderLimit(sql, order string, n int) string { return orderLimit(d, sql, order, n) }
func (mysql) Derived(sql string) string                    { return sql }
func (mysql) driver(p Params) (string, string, error) {
	dsn, err := mysqlDSN(p.URL)
	return "mysql", dsn, err
}
func (mysql) maxOpenConns() int { return 0 }

// ---------- mssql ----------

type mssql struct{}

func (mssql) Name() string             { return "mssql" }
func (mssql) Quote(s string) string    { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }
func (mssql) Placeholder(i int) string { return "@p" + strconv.Itoa(i) }
func (mssql) MaxParams() int           { return 2000 }
func (mssql) RejectsNUL() bool         { return false }

// CLR types arrive as SQL Server's serialisation. ToString gives WKT with Z and M for
// spatial and the /1/2/ path for hierarchyid; NULL stays NULL.
var mssqlUDTs = map[string]string{"GEOGRAPHY": "%s.ToString()", "GEOMETRY": "%s.ToString()", "HIERARCHYID": "%s.ToString()"}

func (mssql) BinaryUDTs() map[string]string { return mssqlUDTs }
func (mssql) TxDDL() bool                   { return true }

// sys.partitions rows are documented as approximate; they need no VIEW DATABASE STATE.
func (mssql) TableStats() string {
	return `SELECT s.name, t.name,
  (SELECT SUM(p.rows) FROM sys.partitions p WHERE p.object_id = t.object_id AND p.index_id IN (0, 1)),
  (SELECT CAST(SUM(a.total_pages) AS bigint) * 8192 FROM sys.partitions p JOIN sys.allocation_units a
     ON a.container_id = CASE WHEN a.type = 2 THEN p.partition_id ELSE p.hobt_id END
   WHERE p.object_id = t.object_id)
FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id`
}
func (mssql) MaxRows() int     { return 1000 }
func (mssql) HasSchemas() bool { return true }
func (mssql) KindOf(n string) Kind {
	switch strings.ToUpper(n) {
	case "TIMESTAMP", "ROWVERSION": // binary(8), not a time
		return Bytes
	}
	return kindFromTypeName(n)
}
func (mssql) TypeName(c Column, isKey bool) string {
	switch c.Kind {
	case Int:
		return "bigint"
	case Float:
		return "float"
	case Decimal:
		return decimalType(c, 38, 38, "decimal(38,10)")
	case Bool:
		return "bit"
	case Timestamp:
		return "datetime2"
	case Date:
		return "date"
	case Bytes:
		return "varbinary(max)"
	}
	if isKey {
		return "nvarchar(450)"
	}
	return "nvarchar(max)"
}
func (d mssql) CastType(k Kind) string { return d.TypeName(Column{Kind: k}, false) }
func (mssql) TableExists(t Table, def string) (string, []any) {
	return "SELECT count(*) FROM information_schema.tables WHERE table_schema = @p1 AND table_name = @p2",
		[]any{cmp.Or(t.Schema, def, "dbo"), t.Name}
}
func (mssql) ListDatabases() string { return "SELECT name FROM sys.databases ORDER BY name" }
func (d mssql) CreateSchema(s string) string {
	return "IF SCHEMA_ID(N'" + strings.ReplaceAll(s, "'", "''") + "') IS NULL EXEC('CREATE SCHEMA " + strings.ReplaceAll(d.Quote(s), "'", "''") + "')"
}
func (mssql) ListTables(schema, def string) (string, []any) {
	if schema == AllSchemas {
		return "SELECT table_schema, table_name FROM information_schema.tables ORDER BY 1, 2", nil
	}
	return "SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema = @p1 ORDER BY 2",
		[]any{cmp.Or(schema, def, "dbo")}
}
func (mssql) PrimaryKey(t Table, def string) (string, []any) {
	return infoSchemaPK("@p1", "@p2"), []any{cmp.Or(t.Schema, def, "dbo"), t.Name}
}
func (d mssql) Rename(from, to Table) string {
	return "EXEC sp_rename '" + strings.ReplaceAll(QuoteTable(d, from), "'", "''") + "', '" + strings.ReplaceAll(to.Name, "'", "''") + "'"
}
func (d mssql) Truncate(t Table) string { return "TRUNCATE TABLE " + QuoteTable(d, t) }
func (d mssql) DeleteMatching(target, src Table, keys []string) string {
	return deleteMatching(d, target, src, keys, true)
}
func (d mssql) Dedupe(t Table, keys []string, uk, seq string) string {
	return dedupeExists(d, t, keys, uk, seq, true)
}
func (d mssql) Swap(tmp, target, _ Table, exists bool) ([]string, bool) {
	return swapTx(d, tmp, target, exists)
}
func (d mssql) OrderLimit(sql, order string, n int) string {
	switch {
	case order == "" && n <= 0:
		return sql
	case n > 0:
		out := Wrap(d, sql, "TOP "+strconv.Itoa(n)+" *", "_t")
		if order != "" {
			out += " ORDER BY " + order
		}
		return out
	}
	// a nested ORDER BY needs TOP or OFFSET
	return Wrap(d, sql, "*", "_t") + " ORDER BY " + order + " OFFSET 0 ROWS"
}

// Derived adds OFFSET 0 ROWS to a top-level ORDER BY without TOP or OFFSET.
func (mssql) Derived(sql string) string {
	m := mask(sql)
	if reOrderBy.MatchString(m) && !reOffset.MatchString(m) && !reTop.MatchString(m) {
		return endComment(sql) + " OFFSET 0 ROWS"
	}
	return sql
}

// driver uses the azuread wrapper: plain logins pass through, fedauth= selects Entra.
func (mssql) driver(p Params) (string, string, error) { return azuread.DriverName, p.URL, nil }
func (mssql) maxOpenConns() int                       { return 0 }

// ---------- sqlite ----------

type sqlite struct{}

func (sqlite) Name() string                  { return "sqlite" }
func (sqlite) Quote(s string) string         { return quoteDouble(s) }
func (sqlite) Placeholder(int) string        { return "?" }
func (sqlite) MaxParams() int                { return 30000 }
func (sqlite) RejectsNUL() bool              { return false }
func (sqlite) BinaryUDTs() map[string]string { return nil }
func (sqlite) TxDDL() bool                   { return true }

func (sqlite) TableStats() string   { return "" }
func (sqlite) MaxRows() int         { return 2000 }
func (sqlite) HasSchemas() bool     { return false }
func (sqlite) KindOf(n string) Kind { return kindFromTypeName(n) }

// TypeName uses declared types the driver reads back as the same kind:
// TIMESTAMP and DATE text comes back as time.Time.
func (sqlite) TypeName(c Column, _ bool) string {
	switch c.Kind {
	case Int:
		return "INTEGER"
	case Bool:
		return "BOOLEAN"
	case Float:
		return "REAL"
	case Decimal:
		return "NUMERIC"
	case Timestamp:
		return "TIMESTAMP"
	case Date:
		return "DATE"
	case Bytes:
		return "BLOB"
	}
	return "TEXT"
}
func (sqlite) CastType(k Kind) string {
	switch k {
	case Int, Bool:
		return "INTEGER"
	case Float:
		return "REAL"
	case Decimal:
		return "NUMERIC"
	case Bytes:
		return "BLOB"
	}
	return "TEXT"
}
func (sqlite) TableExists(t Table, _ string) (string, []any) {
	return "SELECT count(*) FROM sqlite_master WHERE type IN ('table','view') AND name = ?", []any{t.Name}
}
func (sqlite) ListDatabases() string      { return "" }
func (sqlite) CreateSchema(string) string { return "" }
func (sqlite) ListTables(string, string) (string, []any) {
	return "SELECT '', name FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY 1", nil
}
func (sqlite) PrimaryKey(t Table, _ string) (string, []any) {
	return "SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk", []any{t.Name}
}
func (d sqlite) Rename(from, to Table) string {
	return "ALTER TABLE " + QuoteTable(d, from) + " RENAME TO " + d.Quote(to.Name)
}
func (d sqlite) Truncate(t Table) string { return "DELETE FROM " + QuoteTable(d, t) }
func (d sqlite) DeleteMatching(target, src Table, keys []string) string {
	return deleteMatching(d, target, src, keys, false)
}
func (d sqlite) Dedupe(t Table, keys []string, uk, seq string) string {
	return dedupeExists(d, t, keys, uk, seq, false)
}
func (d sqlite) Swap(tmp, target, _ Table, exists bool) ([]string, bool) {
	return swapTx(d, tmp, target, exists)
}
func (d sqlite) OrderLimit(sql, order string, n int) string { return orderLimit(d, sql, order, n) }
func (sqlite) Derived(sql string) string                    { return sql }
func (sqlite) driver(p Params) (string, string, error) {
	if p.Path == "" {
		return "", "", fmt.Errorf("sqlite needs path")
	}
	if !p.Create && p.Path != ":memory:" && !strings.HasPrefix(p.Path, "file:") {
		if _, err := os.Stat(p.Path); errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("no such file: %s", p.Path)
		} else if err != nil {
			return "", "", err
		}
	}
	// _time_format=sqlite writes "2006-01-02 15:04:05.999999999-07:00"; values are UTC
	return "sqlite", p.Path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_time_format=sqlite", nil
}
func (sqlite) maxOpenConns() int { return 1 }
