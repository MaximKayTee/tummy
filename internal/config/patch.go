package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// The files tummy writes are flat: top-level keys, then [prefix.name] tables of keys.
// patchTOML applies the keys and sections of desired onto existing, keeping the comments,
// blank lines, ordering and value spelling of existing. Comment lines directly above a key
// belong to it and go when it goes; blank and comment lines above a header belong to the
// section. New keys are appended to their section, new sections to the file.

type entry struct {
	key     string   // decoded key
	lead    []string // comment lines directly above the key
	lines   []string // the key's own lines
	value   string   // raw value text, may span lines, without the trailing comment
	comment string   // trailing "# ..." on the last line
}

type item struct {
	line string // a blank or comment line, when e is nil
	e    *entry
}

type section struct {
	id     string   // "" for the top level, else prefix + "\x00" + name
	lead   []string // blank and comment lines above the header
	header string
	items  []item
}

type flatFile struct {
	sections []*section
	crlf     bool
}

func (s *section) find(key string) *entry {
	for _, it := range s.items {
		if it.e != nil && it.e.key == key {
			return it.e
		}
	}
	return nil
}

func isComment(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "#") }

// parseFlat splits text into a top section and [prefix.name] sections.
func parseFlat(text string) (*flatFile, error) {
	f := &flatFile{crlf: strings.Contains(text, "\r\n")}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var lines []string
	if text != "" {
		lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	cur := &section{}
	f.sections = append(f.sections, cur)
	for i := 0; i < len(lines); {
		line := lines[i]
		t := strings.TrimSpace(line)
		switch {
		case t == "" || t[0] == '#':
			cur.items = append(cur.items, item{line: line})
			i++
		case strings.HasPrefix(t, "[["):
			return nil, fmt.Errorf("line %d: arrays of tables are not supported", i+1)
		case t[0] == '[':
			parts, err := parseHeader(t)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			if len(parts) != 2 {
				return nil, fmt.Errorf("line %d: %s: only [table.name] headers are supported", i+1, t)
			}
			s := &section{id: parts[0] + "\x00" + parts[1], header: line}
			n := len(cur.items)
			for n > 0 && cur.items[n-1].e == nil {
				n--
			}
			for _, it := range cur.items[n:] {
				s.lead = append(s.lead, it.line)
			}
			cur.items = cur.items[:n]
			f.sections = append(f.sections, s)
			cur = s
			i++
		default:
			e, n, err := parseEntry(lines, i)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			m := len(cur.items)
			for m > 0 && cur.items[m-1].e == nil && isComment(cur.items[m-1].line) {
				m--
			}
			for _, it := range cur.items[m:] {
				e.lead = append(e.lead, it.line)
			}
			cur.items = append(cur.items[:m], item{e: e})
			i += n
		}
	}
	return f, nil
}

// String renders the file. An unmodified parse renders the input unchanged.
func (f *flatFile) String() string {
	var out []string
	for _, s := range f.sections {
		if s.header != "" {
			out = append(out, s.lead...)
			out = append(out, s.header)
		}
		for _, it := range s.items {
			if it.e == nil {
				out = append(out, it.line)
				continue
			}
			out = append(out, it.e.lead...)
			out = append(out, it.e.lines...)
		}
	}
	if len(out) == 0 {
		return ""
	}
	text := strings.Join(out, "\n") + "\n"
	if f.crlf {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

// parseKeyPart reads one bare or quoted key at the start of s and returns it decoded with
// the rest of s.
func parseKeyPart(s string) (string, string, error) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", errors.New("missing key")
	}
	switch s[0] {
	case '"', '\'':
		end := 1
		for end < len(s) && s[end] != s[0] {
			if s[0] == '"' && s[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(s) {
			return "", "", errors.New("unterminated quoted key")
		}
		var v map[string]string
		if _, err := toml.Decode("k = "+s[:end+1], &v); err != nil {
			return "", "", fmt.Errorf("key %s: %w", s[:end+1], err)
		}
		return v["k"], s[end+1:], nil
	}
	end := 0
	for end < len(s) && isBare(s[end]) {
		end++
	}
	if end == 0 {
		return "", "", fmt.Errorf("unexpected %q", s[:1])
	}
	return s[:end], s[end:], nil
}

func isBare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// parseHeader returns the dotted parts of a [a.b] header line.
func parseHeader(t string) ([]string, error) {
	parts, _, err := splitHeader(t)
	return parts, err
}

// splitHeader returns the dotted parts of a [a.b] header line and the text after "]".
func splitHeader(t string) ([]string, string, error) {
	s := strings.TrimPrefix(strings.TrimLeft(t, " \t"), "[")
	var parts []string
	for {
		k, rest, err := parseKeyPart(s)
		if err != nil {
			return nil, "", err
		}
		parts = append(parts, k)
		rest = strings.TrimLeft(rest, " \t")
		switch {
		case strings.HasPrefix(rest, "."):
			s = rest[1:]
		case strings.HasPrefix(rest, "]"):
			if r := strings.TrimSpace(rest[1:]); r != "" && r[0] != '#' {
				return nil, "", fmt.Errorf("unexpected %q after header", r)
			}
			return parts, rest[1:], nil
		default:
			return nil, "", fmt.Errorf("malformed header %s", t)
		}
	}
}

// renamedHeader is header with want's name, keeping header's indent and trailing comment.
func renamedHeader(header, want string) string {
	_, tail, err := splitHeader(header)
	if err != nil {
		return want
	}
	indent := header[:len(header)-len(strings.TrimLeft(header, " \t"))]
	return indent + strings.TrimSpace(want) + tail
}

// parseEntry reads the key/value starting at lines[i] and returns it with the number of
// lines it spans.
func parseEntry(lines []string, i int) (*entry, int, error) {
	k, rest, err := parseKeyPart(lines[i])
	if err != nil {
		return nil, 0, err
	}
	rest = strings.TrimLeft(rest, " \t")
	if strings.HasPrefix(rest, ".") {
		return nil, 0, fmt.Errorf("dotted key %s.…: not supported", k)
	}
	if !strings.HasPrefix(rest, "=") {
		return nil, 0, fmt.Errorf("key %s: missing =", k)
	}
	value, comment, n, err := scanValue(lines, i, strings.TrimLeft(rest[1:], " \t"))
	if err != nil {
		return nil, 0, fmt.Errorf("key %s: %w", k, err)
	}
	return &entry{key: k, lines: append([]string(nil), lines[i:i+n]...), value: value, comment: comment}, n, nil
}

// scanValue reads a TOML value that starts with s on lines[i] and may continue on the
// following lines (multi-line strings, arrays, inline tables). It returns the value text,
// a trailing comment and the number of lines used.
func scanValue(lines []string, i int, s string) (string, string, int, error) {
	const (
		plain = iota
		basic
		literal
		mlBasic
		mlLiteral
	)
	var b strings.Builder
	state, depth, n := plain, 0, 1
	for {
		comment := -1
	scan:
		for j := 0; j < len(s); j++ {
			c := s[j]
			switch state {
			case plain:
				switch {
				case strings.HasPrefix(s[j:], `"""`):
					state, j = mlBasic, j+2
				case strings.HasPrefix(s[j:], `'''`):
					state, j = mlLiteral, j+2
				case c == '"':
					state = basic
				case c == '\'':
					state = literal
				case c == '[' || c == '{':
					depth++
				case c == ']' || c == '}':
					depth--
				case c == '#':
					comment = j
					break scan
				}
			case basic:
				if c == '\\' {
					j++
				} else if c == '"' {
					state = plain
				}
			case literal:
				if c == '\'' {
					state = plain
				}
			case mlBasic:
				if c == '\\' {
					j++
				} else if strings.HasPrefix(s[j:], `"""`) {
					j += 2
					for k := 0; k < 2 && j+1 < len(s) && s[j+1] == '"'; k++ {
						j++
					}
					state = plain
				}
			case mlLiteral:
				if strings.HasPrefix(s[j:], `'''`) {
					j += 2
					for k := 0; k < 2 && j+1 < len(s) && s[j+1] == '\''; k++ {
						j++
					}
					state = plain
				}
			}
		}
		if state == basic || state == literal {
			return "", "", 0, errors.New("unterminated string")
		}
		if comment >= 0 && depth == 0 {
			b.WriteString(strings.TrimRight(s[:comment], " \t"))
			return b.String(), strings.TrimSpace(s[comment:]), n, nil
		}
		b.WriteString(s)
		if state == plain && depth == 0 {
			return strings.TrimRight(b.String(), " \t"), "", n, nil
		}
		if i+n >= len(lines) {
			return "", "", 0, errors.New("unterminated value")
		}
		b.WriteString("\n")
		s = lines[i+n]
		n++
	}
}

func renderKV(k, value, comment string) []string {
	lines := strings.Split(key(k)+" = "+value, "\n")
	if comment != "" {
		lines[len(lines)-1] += "  " + comment
	}
	return lines
}

// sameValue reports whether two raw TOML values decode to the same thing.
func sameValue(a, b string) bool {
	if a == b {
		return true
	}
	var va, vb map[string]any
	if _, err := toml.Decode("v = "+a, &va); err != nil {
		return false
	}
	if _, err := toml.Decode("v = "+b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func decodeMap(text string) (map[string]any, error) {
	m := map[string]any{}
	_, err := toml.Decode(text, &m)
	return m, err
}

// sameTOML reports an error unless a and b decode to the same data.
func sameTOML(a, b string) error {
	ma, err := decodeMap(a)
	if err != nil {
		return err
	}
	mb, err := decodeMap(b)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(ma, mb) {
		return errors.New("patched file differs from the rendered one")
	}
	return nil
}

// patchTOML merges desired into existing. It fails rather than lose formatting it cannot
// follow; the result always decodes to the same data as desired.
func patchTOML(existing, desired string) (string, error) {
	return patchRenamed(existing, desired, nil)
}

// patchRenamed is patchTOML where renames maps old section ids to new ones: a renamed
// section keeps its place, comments and layout under the new header.
func patchRenamed(existing, desired string, renames map[string]string) (string, error) {
	if strings.TrimSpace(existing) == "" {
		return desired, nil
	}
	have, err := parseFlat(existing)
	if err != nil {
		return "", fmt.Errorf("cannot keep the existing layout: %w", err)
	}
	want, err := parseFlat(desired)
	if err != nil {
		return "", err
	}
	wantByID := map[string]*section{}
	for _, w := range want.sections {
		wantByID[w.id] = w
	}
	out := &flatFile{crlf: have.crlf}
	seen := map[string]bool{}
	for _, h := range have.sections {
		id, header := h.id, h.header
		if to, ok := renames[h.id]; ok && wantByID[h.id] == nil && wantByID[to] != nil {
			id, header = to, renamedHeader(h.header, wantByID[to].header)
		}
		w, ok := wantByID[id]
		if !ok || seen[id] {
			continue // section removed, together with its comments
		}
		seen[id] = true
		ns := &section{id: id, lead: h.lead, header: header, items: patchItems(h, w)}
		if ns.header != "" && (len(ns.lead) == 0 || strings.TrimSpace(ns.lead[0]) != "") {
			ns.lead = append([]string{""}, ns.lead...)
		}
		out.sections = append(out.sections, ns)
	}
	for _, w := range want.sections {
		if !seen[w.id] {
			out.sections = append(out.sections, &section{id: w.id, lead: []string{""}, header: w.header, items: w.items})
		}
	}
	text := out.String()
	for strings.HasPrefix(text, "\n") || strings.HasPrefix(text, "\r\n") {
		text = text[strings.IndexByte(text, '\n')+1:]
	}
	if err := sameTOML(text, desired); err != nil {
		return "", fmt.Errorf("cannot keep the existing layout: %w", err)
	}
	return text, nil
}

// patchItems rewrites one section: kept keys updated in place, unwanted keys dropped with
// their comments, missing keys appended before the section's trailing blank lines.
func patchItems(h, w *section) []item {
	done := map[string]bool{}
	var items []item
	for _, it := range h.items {
		if it.e == nil {
			items = append(items, it)
			continue
		}
		we := w.find(it.e.key)
		if we == nil || done[it.e.key] {
			continue
		}
		done[it.e.key] = true
		if sameValue(it.e.value, we.value) {
			items = append(items, it)
			continue
		}
		items = append(items, item{e: &entry{
			key: it.e.key, lead: it.e.lead, value: we.value, comment: it.e.comment,
			lines: renderKV(it.e.key, we.value, it.e.comment),
		}})
	}
	n := len(items)
	for n > 0 && items[n-1].e == nil && strings.TrimSpace(items[n-1].line) == "" {
		n--
	}
	tail := append([]item(nil), items[n:]...)
	items = items[:n]
	for _, it := range w.items {
		if it.e != nil && !done[it.e.key] {
			done[it.e.key] = true
			items = append(items, item{e: &entry{key: it.e.key, value: it.e.value, lines: renderKV(it.e.key, it.e.value, "")}})
		}
	}
	return append(items, tail...)
}

var topEnabledRe = regexp.MustCompile(`(?m)^([ \t]*)enabled[ \t]*=[ \t]*(?:true|false)([ \t]*(?:#[^\r\n]*)?)(\r?)$`)

// setTopKey sets one top-level key to the raw TOML value v, keeping everything else.
func setTopKey(text, k, v string) (string, error) {
	out, err := setTopKeyFlat(text, k, v)
	if err != nil && k == "enabled" {
		out, err = setTopKeyRegex(text, v)
	}
	if err != nil {
		return "", err
	}
	before, err := decodeMap(text)
	if err != nil {
		return "", err
	}
	nv, err := decodeMap("v = " + v)
	if err != nil {
		return "", err
	}
	before[k] = nv["v"]
	after, err := decodeMap(out)
	if err != nil || !reflect.DeepEqual(before, after) {
		return "", fmt.Errorf("cannot set %s in place", k)
	}
	return out, nil
}

func setTopKeyFlat(text, k, v string) (string, error) {
	f, err := parseFlat(text)
	if err != nil {
		return "", err
	}
	top := f.sections[0]
	if e := top.find(k); e != nil {
		e.value, e.lines = v, renderKV(k, v, e.comment)
		return f.String(), nil
	}
	n := len(top.items)
	for n > 0 && top.items[n-1].e == nil && strings.TrimSpace(top.items[n-1].line) == "" {
		n--
	}
	ne := item{e: &entry{key: k, value: v, lines: renderKV(k, v, "")}}
	top.items = append(top.items[:n], append([]item{ne}, top.items[n:]...)...)
	return f.String(), nil
}

// setTopKeyRegex handles files the flat parser does not follow.
func setTopKeyRegex(text, v string) (string, error) {
	head, tail := text, ""
	if i := strings.Index(text, "\n["); i >= 0 {
		head, tail = text[:i+1], text[i+1:]
	}
	if topEnabledRe.MatchString(head) {
		return topEnabledRe.ReplaceAllString(head, "${1}enabled = "+v+"${2}${3}") + tail, nil
	}
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	head = strings.TrimRight(head, "\r\n") + eol + "enabled = " + v + eol
	if tail != "" {
		head += eol
	}
	return head + tail, nil
}
