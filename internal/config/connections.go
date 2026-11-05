package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"tummy/internal/db"
)

// Conn is one entry in connections.toml.
type Conn struct {
	Name   string `toml:"-"`
	Dir    string `toml:"-"` // directory of connections.toml: relative sqlite paths resolve against it
	Type   string `toml:"type"`
	URL    string `toml:"url"`
	Path   string `toml:"path"`
	Schema string `toml:"schema"`

	// api
	BaseURL   string `toml:"base_url"`
	Auth      string `toml:"auth"`
	Token     string `toml:"token"`
	User      string `toml:"user"`
	Password  string `toml:"password"`
	Header    string `toml:"header"`
	Param     string `toml:"param"`
	RateLimit string `toml:"rate_limit"`
	RetryOn   []int  `toml:"retry_on"`

	Headers map[string]string `toml:"headers"` // sent with every request to the base_url host
	Timeout string            `toml:"timeout"` // per request, Go duration; default 2m
}

type Connections struct {
	Conn map[string]*Conn `toml:"conn"`
	Path string           `toml:"-"`

	renamed map[string]string // new name to the name in the file, for WriteConnections
}

// Rename renames a connection. WriteConnections keeps its section in place, with its
// comments, under the new name. Replications that use it are not changed.
func (c *Connections) Rename(from, to string) error {
	cn, ok := c.Conn[from]
	switch {
	case !ok:
		return fmt.Errorf("unknown connection %s", from)
	case from == to:
		return nil
	case c.Conn[to] != nil:
		return fmt.Errorf("connection %s exists", to)
	}
	delete(c.Conn, from)
	cn.Name = to
	c.Conn[to] = cn
	if c.renamed == nil {
		c.renamed = map[string]string{}
	}
	if orig, ok := c.renamed[from]; ok {
		delete(c.renamed, from)
		from = orig
	}
	c.renamed[to] = from
	return nil
}

// LoadConnectionsRaw reads connections.toml for editing: ${VAR} references and relative
// paths stay as written. Unknown keys are an error. A missing file is an empty set.
func LoadConnectionsRaw(path string) (*Connections, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Connections{Conn: map[string]*Conn{}, Path: path}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Connections
	md, err := toml.Decode(string(raw), &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := unknownConnKeys(md); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	if c.Conn == nil {
		c.Conn = map[string]*Conn{}
	}
	for name, cn := range c.Conn {
		cn.Name = name
		cn.Dir = filepath.Dir(path)
	}
	return &c, nil
}

// LoadConnections reads and checks connections.toml. Values keep their ${VAR} references;
// Get and Expanded resolve them, so an unset variable only matters to the connection
// that uses it. A missing file is an empty set.
func LoadConnections(path string) (*Connections, error) {
	c, err := LoadConnectionsRaw(path)
	if err != nil {
		return nil, err
	}
	for _, name := range c.Names() {
		if err := c.Conn[name].check(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return c, nil
}

func unknownConnKeys(md toml.MetaData) error {
	var msgs []string
	reported := map[string]bool{}
	for _, k := range md.Undecoded() {
		switch {
		case len(k) >= 1 && k[0] != "conn":
			if !reported[k[0]] {
				reported[k[0]] = true
				msgs = append(msgs, fmt.Sprintf("%s: connections go under [conn.NAME]", k[0]))
			}
		case len(k) == 3:
			msgs = append(msgs, fmt.Sprintf("conn %s: %s", k[1], unknownKeyMsg(k[2], connKeys())))
		case len(k) > 3 && !reported[strings.Join(k[:3], ".")]:
			reported[strings.Join(k[:3], ".")] = true
			msgs = append(msgs, fmt.Sprintf("conn %s: %s", k[1], unknownKeyMsg(k[2], connKeys())))
		}
	}
	if len(msgs) > 0 {
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
}

func connKeys() []string { return keysOf(Conn{}) }

var authModes = []string{"none", "bearer", "basic", "header", "query"}

// check validates values that do not depend on the environment.
func (c *Conn) check() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("conn %s: "+format, append([]any{c.Name}, args...)...)
	}
	set := func(s string) bool { return s != "" && !hasEnvRef(s) }
	if c.Type == "" {
		return fail(`type is required: postgres, mysql, mssql, sqlite or api`)
	}
	if hasEnvRef(c.Type) {
		return fail("type cannot use ${VAR}")
	}
	if c.Type == "api" {
		if c.BaseURL == "" {
			return fail("base_url is required")
		}
		if set(c.Auth) && !contains(authModes, c.Auth) {
			return fail("auth %q: want %s", c.Auth, orList(authModes))
		}
		if set(c.RateLimit) {
			if err := checkRate(c.RateLimit); err != nil {
				return fail("rate_limit %q: %v", c.RateLimit, err)
			}
		}
		if set(c.Timeout) {
			if d, err := time.ParseDuration(c.Timeout); err != nil || d <= 0 {
				return fail("timeout %q: want a duration such as 30s or 2m", c.Timeout)
			}
		}
		for _, code := range c.RetryOn {
			if code < 100 || code > 599 {
				return fail("retry_on: %d is not an HTTP status", code)
			}
		}
		return nil
	}
	if _, err := db.DialectFor(c.Type); err != nil {
		return fail("type %q: want postgres, mysql, mssql, sqlite or api", c.Type)
	}
	if t, _ := db.DialectFor(c.Type); t.Name() == "sqlite" {
		if c.Path == "" {
			return fail("path is required")
		}
	} else if c.URL == "" {
		return fail("url is required")
	} else if err := CheckURL(t.Name(), c.URL); err != nil {
		return fail("url: %v", err)
	}
	return nil
}

func checkRate(s string) error {
	n, unit, ok := strings.Cut(s, "/")
	var count float64
	if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &count); err != nil || !ok || count <= 0 {
		return errors.New("want N/s, N/m or N/h")
	}
	switch strings.TrimSpace(unit) {
	case "s", "m", "h":
		return nil
	}
	return errors.New("want N/s, N/m or N/h")
}

// Get returns the named connection with ${VAR} references and the sqlite path resolved.
func (c *Connections) Get(name string) (*Conn, error) {
	cn, ok := c.Conn[name]
	if !ok {
		if len(c.Conn) == 0 {
			return nil, fmt.Errorf("unknown connection %s: %s defines none", name, c.Path)
		}
		return nil, fmt.Errorf("unknown connection %s (%s: %s)", name, c.Path, strings.Join(c.Names(), ", "))
	}
	return cn.Expanded()
}

func (c *Connections) Names() []string {
	names := make([]string, 0, len(c.Conn))
	for n := range c.Conn {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Expanded returns a copy with ${VAR} references resolved in every string field and the
// sqlite path resolved against Dir.
func (c *Conn) Expanded() (*Conn, error) {
	out := *c
	deepCopy(&out)
	if err := expandFields(&out); err != nil {
		return nil, fmt.Errorf("conn %s: %w", c.Name, err)
	}
	out.Path = out.SQLitePath()
	if strings.Contains(out.URL, "://") {
		if _, err := url.Parse(out.URL); err != nil {
			// the parse error repeats the URL, password included
			return nil, fmt.Errorf("conn %s: url: not a valid URL", c.Name)
		}
	}
	return &out, nil
}

// SQLitePath is Path with ~ expanded and a relative path resolved against Dir.
// ":memory:" and "file:" URIs are returned as written.
func (c *Conn) SQLitePath() string { return ResolvePath(c.Path, c.Dir) }

// Target is the url, path or base_url with passwords and secret parameters masked.
func (c *Conn) Target() string {
	for _, s := range []string{c.URL, c.Path, c.BaseURL} {
		if s != "" {
			return Redact(s)
		}
	}
	return ""
}

// DB returns the parameters the database layer needs.
func (c *Conn) DB() db.Params {
	return db.Params{Name: c.Name, Type: c.Type, URL: c.URL, Path: c.SQLitePath(), Schema: c.Schema}
}
