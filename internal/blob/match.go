package blob

import (
	"errors"
	"path"
	"strings"
)

// Patterns expands {a,b} groups: "r/{x,y}_*.csv" is r/x_*.csv and r/y_*.csv. Each
// result is a path.Match pattern, where * and ? do not cross a /. A backslash makes the
// next character literal.
func Patterns(p string) ([]string, error) {
	i := unescaped(p, '{')
	if i < 0 {
		if unescaped(p, '}') >= 0 {
			return nil, errors.New("} without {")
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, errors.New("unclosed [ or trailing \\")
		}
		return []string{p}, nil
	}
	j := unescaped(p[i:], '}')
	if j < 0 {
		return nil, errors.New("{ without }")
	}
	j += i
	inner := p[i+1 : j]
	if unescaped(inner, '{') >= 0 {
		return nil, errors.New("{ inside {}")
	}
	rest, err := Patterns(p[j+1:])
	if err != nil {
		return nil, err
	}
	var out []string
	for _, alt := range strings.Split(inner, ",") {
		for _, r := range rest {
			out = append(out, p[:i]+alt+r)
		}
	}
	return out, nil
}

// unescaped is the index of the first c in s not after a backslash, or -1.
func unescaped(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case c:
			return i
		}
	}
	return -1
}

// Literal is a pattern that matches name alone.
func Literal(name string) string {
	var b strings.Builder
	for _, r := range name {
		if strings.ContainsRune(`*?[]{}\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// IsPattern reports whether p holds a wildcard or group.
func IsPattern(p string) bool { return strings.ContainsAny(p, "*?[{") }

// Match reports whether name matches pattern p, {a,b} groups included.
func Match(p, name string) bool {
	pats, err := Patterns(p)
	if err != nil {
		return false
	}
	for _, q := range pats {
		if ok, _ := path.Match(q, name); ok {
			return true
		}
	}
	return false
}

// Prefix is the part of p before its first wildcard or group: the listing prefix.
func Prefix(p string) string {
	if i := strings.IndexAny(p, `*?[{\`); i >= 0 {
		return p[:i]
	}
	return p
}

// Filter keeps the blobs whose names match p.
func Filter(blobs []Blob, p string) []Blob {
	pats, err := Patterns(p)
	if err != nil {
		return nil
	}
	var out []Blob
	for _, b := range blobs {
		for _, q := range pats {
			if ok, _ := path.Match(q, b.Name); ok {
				out = append(out, b)
				break
			}
		}
	}
	return out
}
