package api

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"tummy/internal/config"
)

var placeholder = regexp.MustCompile(`\{([A-Za-z0-9_.]+)\}`)

// Placeholders lists the fields {prefix.field} of a stream's path, body and query use,
// without the prefix, each once.
func Placeholders(s *config.Stream, prefix string) []string {
	var out []string
	seen := map[string]bool{}
	scan := func(text string) {
		for _, m := range placeholder.FindAllStringSubmatch(text, -1) {
			if f, ok := strings.CutPrefix(m[1], prefix); ok && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	scan(s.Path)
	scan(s.Body)
	for _, v := range s.Query {
		scan(v)
	}
	return out
}

// fillPath fills the path part with url.PathEscape and any query part with url.QueryEscape.
func fillPath(p string, vars map[string]string) (string, error) {
	path, query, hasQuery := strings.Cut(p, "?")
	out, err := fill(path, vars, url.PathEscape, true)
	if err != nil || !hasQuery {
		return out, err
	}
	q, err := fill(query, vars, url.QueryEscape, false)
	return out + "?" + q, err
}

// fill replaces {name} with vars, escaped by esc. A placeholder whose prefix
// is not in vars is an error; a missing field, or an empty one when required,
// wraps ErrNoValue. An absent {watermark} is empty unless required.
func fill(s string, vars map[string]string, esc func(string) string, required bool) (string, error) {
	var unknown, missing []string
	out := placeholder.ReplaceAllStringFunc(s, func(m string) string {
		key := m[1 : len(m)-1]
		v, ok := vars[key]
		switch {
		case !ok && key == "watermark" && !required:
		case !ok && key != "watermark" && !knownPrefix(vars, key):
			unknown = append(unknown, key)
		case !ok || (required && v == ""):
			missing = append(missing, key)
		}
		if esc != nil {
			return esc(v)
		}
		return v
	})
	if len(unknown) > 0 {
		return "", fmt.Errorf("unknown placeholders %v in %q", unknown, s)
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("placeholders %v in %q: %w", missing, s, ErrNoValue)
	}
	return out, nil
}

func knownPrefix(vars map[string]string, key string) bool {
	i := strings.IndexByte(key, '.')
	if i < 0 {
		return false
	}
	prefix := key[:i+1]
	for k := range vars {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// jsonEscape escapes s for use inside a JSON string.
func jsonEscape(s string) string {
	b, _ := marshal(s)
	return string(b[1 : len(b)-1])
}
