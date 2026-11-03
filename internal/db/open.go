package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "modernc.org/sqlite"
)

// Params is what Open needs from a connection entry.
type Params struct {
	Name   string
	Type   string
	URL    string
	Path   string
	Schema string
	// Create lets Open create a missing sqlite file; without it a missing file is an error.
	Create bool
}

// Open connects to a database.
func Open(ctx context.Context, c Params) (*DB, error) {
	dl, err := DialectFor(c.Type)
	if err != nil {
		return nil, fmt.Errorf("conn %s: %w", c.Name, err)
	}
	driver, dsn, err := dl.driver(c)
	if err != nil {
		return nil, fmt.Errorf("conn %s: %w", c.Name, err)
	}
	sdb, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("conn %s: %w", c.Name, err)
	}
	if n := dl.maxOpenConns(); n > 0 {
		sdb.SetMaxOpenConns(n)
	}
	if err := sdb.PingContext(ctx); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("conn %s: %w", c.Name, err)
	}
	return &DB{DB: sdb, Dialect: dl, Name: c.Name, Schema: c.Schema}, nil
}

// mysqlDSN turns mysql://user:pass@host:port/db into the go-sql-driver form.
// Parse errors leave the URL out: it holds the password.
func mysqlDSN(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("mysql url: invalid")
	}
	pass, _ := u.User.Password()
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":3306"
	}
	q := u.Query()
	q.Set("parseTime", "true")
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?%s", u.User.Username(), pass, host, strings.TrimPrefix(u.Path, "/"), q.Encode()), nil
}
