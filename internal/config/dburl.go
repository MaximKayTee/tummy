package config

import (
	"errors"
	"net/url"
	"strings"
)

// URLParts is a database url split into its fields. Env references stay as written.
type URLParts struct {
	Scheme, User, Password, Host, Port, Database, Instance string
	Query                                                  url.Values
}

// urlSchemes lists the schemes each database type's driver reads a host from.
var urlSchemes = map[string][]string{
	"postgres": {"postgres", "postgresql"},
	"mysql":    {"mysql"},
	"mssql":    {"sqlserver"},
}

// URLExample is the url shape for a type.
var URLExample = map[string]string{
	"postgres": "postgres://user:${PG_PASS}@host:5432/db?sslmode=disable",
	"mysql":    "mysql://user:${MYSQL_PASS}@host:3306/db",
	"mssql":    "sqlserver://user:${MSSQL_PASS}@host:1433?database=db",
}

// DefaultPort is the port the driver dials when the url has none.
var DefaultPort = map[string]string{"postgres": "5432", "mysql": "3306", "mssql": "1433"}

// CheckURL rejects a url whose scheme the type's driver would not read a host from.
// A url that is one env reference, or a driver-native key=value DSN, passes.
func CheckURL(typ, raw string) error {
	schemes, ok := urlSchemes[typ]
	if !ok || strings.HasPrefix(raw, "${") {
		return nil
	}
	scheme, _, found := strings.Cut(raw, "://")
	if !found {
		if strings.Contains(raw, "=") || strings.Contains(raw, "@tcp(") {
			return nil
		}
		return errors.New("want " + URLExample[typ])
	}
	for _, s := range schemes {
		if scheme == s {
			return nil
		}
	}
	return errors.New("want " + URLExample[typ])
}

// ParseURL splits scheme://[user[:pass]@]host[:port][/path][?query]. Env references in
// user and password are kept; other values are unescaped. ok is false without "://".
func ParseURL(raw string) (p URLParts, ok bool) {
	scheme, rest, found := strings.Cut(raw, "://")
	if !found {
		return p, false
	}
	p.Scheme = scheme
	p.Query = url.Values{}
	if rest, q, has := strings.Cut(rest, "?"); has {
		p.Query, _ = url.ParseQuery(q)
		raw = rest
	} else {
		raw = rest
	}
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		user := raw[:i]
		raw = raw[i+1:]
		u, pw, _ := strings.Cut(user, ":")
		p.User, p.Password = unescapeKeep(u), unescapeKeep(pw)
	}
	host, path, _ := strings.Cut(raw, "/")
	if h, port, has := strings.Cut(host, ":"); has {
		host, p.Port = h, port
	}
	p.Host = host
	switch scheme {
	case "sqlserver":
		p.Instance = path
		p.Database = p.Query.Get("database")
		p.Query.Del("database")
	default:
		p.Database = path
	}
	return p, true
}

// String joins the parts back into a url.
func (p URLParts) String() string {
	var b strings.Builder
	b.WriteString(p.Scheme + "://")
	if p.User != "" || p.Password != "" {
		b.WriteString(escapeKeep(p.User))
		if p.Password != "" {
			b.WriteString(":" + escapeKeep(p.Password))
		}
		b.WriteString("@")
	}
	b.WriteString(p.Host)
	if p.Port != "" {
		b.WriteString(":" + p.Port)
	}
	q := url.Values{}
	for k, v := range p.Query {
		q[k] = v
	}
	switch p.Scheme {
	case "sqlserver":
		if p.Instance != "" {
			b.WriteString("/" + p.Instance)
		}
		if p.Database != "" {
			q.Set("database", p.Database)
		}
	default:
		if p.Database != "" {
			b.WriteString("/" + p.Database)
		}
	}
	if len(q) > 0 {
		b.WriteString("?" + q.Encode())
	}
	return b.String()
}

func escapeKeep(s string) string {
	if hasEnvRef(s) {
		return s
	}
	return url.UserPassword("", s).String()[1:]
}

func unescapeKeep(s string) string {
	if hasEnvRef(s) {
		return s
	}
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}
