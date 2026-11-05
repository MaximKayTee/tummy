// Package probe checks that a connection answers: a database opens and lists its tables,
// an API's base_url answers HTTP.
package probe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tummy/internal/api"
	"tummy/internal/config"
	"tummy/internal/db"
)

type Result struct {
	Tables []string // schema.table or table; databases only
	Status int      // HTTP status; APIs only
	Took   time.Duration
}

// AllSchemas as the schema of Conn lists the tables of every schema.
const AllSchemas = db.AllSchemas

// Conn tests c as loaded from connections.toml: ${VAR} references are resolved and a
// relative sqlite path is resolved against c.Dir. schema limits the table list; "" is
// the connection's schema, AllSchemas every schema. A sqlite file that does not exist
// is an error, not created.
func Conn(ctx context.Context, c *config.Conn, schema string) (Result, error) {
	start := time.Now()
	x, err := c.Expanded()
	if err != nil {
		return Result{}, err
	}
	if x.Type == "api" {
		if _, err := api.NewClient(x); err != nil {
			return Result{}, err
		}
		status, err := reach(ctx, x.BaseURL)
		if err != nil {
			return Result{}, fmt.Errorf("conn %s: %w", x.Name, err)
		}
		return Result{Status: status, Took: time.Since(start)}, nil
	}
	d, err := db.Open(ctx, x.DB())
	if err != nil {
		return Result{}, fmt.Errorf("conn %s: connect after %s: %s", x.Name, time.Since(start).Round(time.Millisecond), strings.TrimPrefix(err.Error(), "conn "+x.Name+": "))
	}
	defer d.Close()
	connected := time.Now()
	var tables []db.Table
	if schema == AllSchemas {
		tables, err = d.ListAllTables(ctx)
	} else {
		tables, err = d.ListTables(ctx, schema)
	}
	if err != nil {
		return Result{}, fmt.Errorf("conn %s: list tables after %s, connected in %s: %w", x.Name, time.Since(start).Round(time.Millisecond), connected.Sub(start).Round(time.Millisecond), err)
	}
	res := Result{Took: time.Since(start)}
	for _, t := range tables {
		res.Tables = append(res.Tables, t.String())
	}
	return res, nil
}

// Open opens a database connection as Conn does, without listing anything.
func Open(ctx context.Context, c *config.Conn) (*db.DB, error) {
	start := time.Now()
	x, err := c.Expanded()
	if err != nil {
		return nil, err
	}
	d, err := db.Open(ctx, x.DB())
	if err != nil {
		return nil, fmt.Errorf("conn %s: connect after %s: %s", x.Name, time.Since(start).Round(time.Millisecond), strings.TrimPrefix(err.Error(), "conn "+x.Name+": "))
	}
	return d, nil
}

// CreateSQLite creates an empty sqlite database at path, 0600, and its directory, 0700.
// path is resolved, as Conn.Expanded gives it. An existing file is left alone.
func CreateSQLite(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return fmt.Errorf("%q: not a file path", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// reach checks the host answers at all. Any HTTP status counts, since many APIs 404 at
// the root.
func reach(ctx context.Context, base string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return 0, err
	}
	cl := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := cl.Do(req)
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			// *url.Error repeats the URL, query secrets included
			return 0, ue.Unwrap()
		}
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
