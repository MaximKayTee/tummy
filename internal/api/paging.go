package api

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/jmespath/go-jmespath"

	"tummy/internal/config"
)

// call is one Fetch in progress.
type call struct {
	s       *config.Stream
	method  string
	first   *url.URL
	body    []byte         // filled body; nil when empty
	bodyObj map[string]any // params_in = body

	records, cursorField, stopWhen, total *jmespath.JMESPath

	pageParam, sizeParam, cursorParam string

	page, offset int
	cursor       string
	next         *url.URL
	lastBody     []byte
	seen         map[string]bool
	firstLen     int
	lastHash     [32]byte
}

func (c *Client) newCall(s *config.Stream, vars map[string]string) (*call, error) {
	if s.Path == "" {
		return nil, errors.New("path is required")
	}
	switch s.Paginate {
	case "", "page", "offset", "link":
	case "cursor":
		if s.CursorField == "" {
			return nil, errors.New("paginate = cursor needs cursor_field")
		}
	default:
		return nil, fmt.Errorf("unknown paginate %q (page, offset, cursor, link)", s.Paginate)
	}
	switch s.ParamsIn {
	case "", "query", "body":
	default:
		return nil, fmt.Errorf("params_in must be query or body, not %q", s.ParamsIn)
	}
	cl := &call{
		s:           s,
		method:      cmp.Or(strings.ToUpper(s.Method), http.MethodGet),
		pageParam:   cmp.Or(s.PageParam, map[string]string{"page": "page", "offset": "offset"}[s.Paginate]),
		sizeParam:   cmp.Or(s.SizeParam, "limit"),
		cursorParam: cmp.Or(s.CursorParam, "cursor"),
		page:        1,
		seen:        map[string]bool{},
	}
	for _, e := range []struct {
		key, expr string
		dst       **jmespath.JMESPath
	}{
		{"records", s.Records, &cl.records},
		{"cursor_field", s.CursorField, &cl.cursorField},
		{"stop_when", s.StopWhen, &cl.stopWhen},
		{"total_field", s.TotalField, &cl.total},
	} {
		if e.expr == "" {
			continue
		}
		jp, err := jmespath.Compile(e.expr)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", e.key, e.expr, err)
		}
		*e.dst = jp
	}

	path, err := fillPath(s.Path, vars)
	if err != nil {
		return nil, err
	}
	target := c.conn.BaseURL + path
	if strings.HasPrefix(path, "/") {
		target = strings.TrimSuffix(c.conn.BaseURL, "/") + path
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("path %q: not a valid URL", s.Path)
	}
	q := u.Query()
	for k, v := range s.Query {
		if strings.Contains(v, "{watermark}") && vars["watermark"] == "" {
			continue
		}
		fv, err := fill(v, vars, nil, false)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", k, err)
		}
		q.Set(k, fv)
	}
	u.RawQuery = q.Encode()
	cl.first = u

	body, err := fill(s.Body, vars, jsonEscape, false)
	if err != nil {
		return nil, fmt.Errorf("body: %w", err)
	}
	if s.ParamsIn == "body" {
		if strings.TrimSpace(body) != "" {
			dec := json.NewDecoder(strings.NewReader(body))
			dec.UseNumber()
			if err := dec.Decode(&cl.bodyObj); err != nil {
				return nil, fmt.Errorf("params_in = body needs a JSON object body: %w", err)
			}
		}
		if cl.bodyObj == nil {
			cl.bodyObj = map[string]any{}
		}
	} else if body != "" {
		cl.body = []byte(body)
	}
	return cl, nil
}

// request returns the URL and body of the next page.
func (cl *call) request() (*url.URL, []byte, error) {
	if cl.next != nil {
		return cl.next, cl.lastBody, nil
	}
	params := map[string]any{}
	switch cl.s.Paginate {
	case "page":
		params[cl.pageParam] = cl.page
	case "offset":
		params[cl.pageParam] = cl.offset
	case "cursor":
		if cl.cursor != "" {
			params[cl.cursorParam] = cl.cursor
		}
	}
	if cl.s.Paginate != "" && cl.s.PageSize > 0 {
		params[cl.sizeParam] = cl.s.PageSize
	}
	u := *cl.first
	body := cl.body
	if cl.bodyObj != nil {
		for k, v := range params {
			if err := setPath(cl.bodyObj, k, v); err != nil {
				return nil, nil, err
			}
		}
		b, err := marshal(cl.bodyObj)
		if err != nil {
			return nil, nil, err
		}
		body = b
	} else if len(params) > 0 {
		q := u.Query()
		for k, v := range params {
			q.Set(k, fmt.Sprint(v))
		}
		u.RawQuery = q.Encode()
	}
	cl.lastBody = body
	return &u, body, nil
}

// setPath sets a dotted key such as variables.after in a JSON object.
func setPath(m map[string]any, path string, v any) error {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			if m[p] != nil {
				return fmt.Errorf("body: %s is not an object", path)
			}
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
	return nil
}

// pick applies records. With paginate set, an object is an error: every page would count as one record.
func (cl *call) pick(data any) (any, error) {
	v := data
	if cl.records != nil {
		var err error
		if v, err = cl.records.Search(data); err != nil {
			return nil, fmt.Errorf("records %q: %w", cl.s.Records, err)
		}
	}
	if _, obj := v.(map[string]any); obj && cl.s.Paginate != "" {
		return nil, errors.New("paginate needs records to select an array; got an object")
	}
	return v, nil
}

// repeated reports a page equal to the previous one. Page 2 equal to a full
// page 1 means the API ignores the page parameter; later, that it repeats its last page.
func (cl *call) repeated(n int, v any, count int) (bool, error) {
	if (cl.s.Paginate != "page" && cl.s.Paginate != "offset") || count == 0 {
		return false, nil
	}
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	if n == 0 || h != cl.lastHash {
		cl.lastHash = h
		return false, nil
	}
	if n == 1 && (cl.s.PageSize == 0 || cl.firstLen >= cl.s.PageSize) {
		return true, fmt.Errorf("page 2 equals page 1: the API ignores %s", cl.pageParam)
	}
	return true, nil
}

// advance moves to the next page and reports whether there is one.
func (cl *call) advance(resp *http.Response, u *url.URL, data any, recs []Record, have int) (bool, error) {
	s := cl.s
	ruled := cl.stopWhen != nil || cl.total != nil
	if ruled {
		if stop, err := cl.stop(data, have); err != nil || stop {
			return false, err
		}
	}
	switch s.Paginate {
	case "page", "offset":
		if len(recs) == 0 {
			return false, nil
		}
		// Servers cap page size quietly: the first page sets the size.
		if cl.firstLen == 0 {
			cl.firstLen = len(recs)
		} else if !ruled && len(recs) < cl.firstLen {
			return false, nil
		}
		cl.page++
		cl.offset += len(recs)
		return true, nil
	case "cursor":
		if len(recs) == 0 {
			return false, nil
		}
		cl.seen[u.String()] = true
		x, err := cl.cursorField.Search(data)
		if err != nil {
			return false, fmt.Errorf("cursor_field %q: %w", s.CursorField, err)
		}
		next := stringOf(x)
		if next == "" {
			return false, nil
		}
		if cl.seen[next] {
			return false, fmt.Errorf("cursor %q came back twice: the API ignores %s", truncate(next, 60), cl.cursorParam)
		}
		cl.seen[next] = true
		if strings.HasPrefix(next, "http://") || strings.HasPrefix(next, "https://") {
			nu, err := url.Parse(next)
			if err != nil {
				return false, errors.New("cursor_field: invalid next URL")
			}
			cl.next = nu
		} else {
			cl.cursor = next
		}
		return true, nil
	case "link":
		if len(recs) == 0 {
			return false, nil
		}
		cl.seen[u.String()] = true
		link := linkNext(resp.Header.Values("Link"))
		if link == "" {
			return false, nil
		}
		nu, err := u.Parse(link)
		if err != nil || (nu.Scheme != "http" && nu.Scheme != "https") {
			return false, errors.New("Link: invalid next URL")
		}
		nu.Fragment = ""
		if cl.seen[nu.String()] {
			return false, errors.New("Link: next URL came back twice")
		}
		cl.next = nu
		return true, nil
	}
	return false, nil
}

// stop evaluates stop_when and total_field. Numbers become float64 so JMESPath can compare them.
func (cl *call) stop(data any, have int) (bool, error) {
	fd := floats(data)
	if cl.stopWhen != nil {
		v, err := cl.stopWhen.Search(fd)
		if err != nil {
			return false, fmt.Errorf("stop_when %q: %w", cl.s.StopWhen, err)
		}
		if truthy(v) {
			return true, nil
		}
	}
	if cl.total != nil {
		v, err := cl.total.Search(fd)
		if err != nil {
			return false, fmt.Errorf("total_field %q: %w", cl.s.TotalField, err)
		}
		if f, ok := v.(float64); ok && float64(have) >= f {
			return true, nil
		}
	}
	return false, nil
}

func floats(v any) any {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = floats(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = floats(e)
		}
		return out
	}
	return v
}

// truthy follows JMESPath: false, null, "", [] and {} are false.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func stringOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

// linkNext returns the rel="next" target of RFC 8288 Link headers.
func linkNext(headers []string) string {
	for _, h := range headers {
		for {
			i := strings.IndexByte(h, '<')
			if i < 0 {
				break
			}
			j := strings.IndexByte(h[i:], '>')
			if j < 0 {
				break
			}
			target := h[i+1 : i+j]
			params, rest := linkParams(h[i+j+1:])
			if relNext(params) {
				return strings.TrimSpace(target)
			}
			h = rest
		}
	}
	return ""
}

// linkParams splits off the parameters of one link value, up to a comma outside quotes.
func linkParams(s string) (params, rest string) {
	quoted := false
	for i, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

func relNext(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "rel") {
			continue
		}
		for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(v), `"`)) {
			if strings.EqualFold(rel, "next") {
				return true
			}
		}
	}
	return false
}
