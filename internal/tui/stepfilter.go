package tui

import (
	"errors"
	"regexp"
	"strings"
	"unicode"

	"tummy/internal/db"
)

// nullItem is the value-list entry for NULL.
const nullItem = "∅ null"

var filterOps = []string{"=", "!=", ">", ">=", "<", "<=", "contains", "starts with", "ends with", "in", "not in", "is null", "is not null"}

var numberRe = regexp.MustCompile(`^-?(\d+(\.\d*)?|\.\d+)([eE][-+]?\d+)?$`)

func isNumeric(k db.Kind) bool { return k == db.Int || k == db.Float || k == db.Decimal }

// opsFor lists the guided operators that work on a column kind. LIKE only on text.
func opsFor(k db.Kind) []string {
	if k == db.String {
		return filterOps
	}
	var out []string
	for _, o := range filterOps {
		switch o {
		case "contains", "starts with", "ends with":
		default:
			out = append(out, o)
		}
	}
	return out
}

// argSafe reports column names step arguments can carry as written: no separators,
// quotes, brackets or control characters.
func argSafe(name string) bool {
	if name == "" || strings.TrimSpace(name) != name || strings.HasPrefix(name, "-") {
		return false
	}
	if strings.ContainsAny(name, "|,='\"`()[];") || strings.Contains(strings.ToLower(name), " as ") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// sqlString quotes s as a string literal. MySQL also reads backslash escapes.
func sqlString(dl db.Dialect, s string) string {
	if dl.Name() == "mysql" {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "t", "1", "yes", "y":
		return true, true
	case "false", "f", "0", "no", "n":
		return false, true
	}
	return false, false
}

func boolSQL(dl db.Dialect, b bool) string {
	switch dl.Name() {
	case "mssql", "sqlite":
		if b {
			return "1"
		}
		return "0"
	}
	if b {
		return "TRUE"
	}
	return "FALSE"
}

// literal renders a value for a column of kind k: numbers bare on numeric columns,
// booleans per dialect, everything else a quoted string.
func literal(dl db.Dialect, k db.Kind, v string) string {
	switch {
	case isNumeric(k) && numberRe.MatchString(v):
		return v
	case k == db.Bool:
		if b, ok := parseBool(v); ok {
			return boolSQL(dl, b)
		}
	}
	return sqlString(dl, v)
}

// looseLiteral is a typed value with no column kind: numbers bare, the rest quoted.
func looseLiteral(dl db.Dialect, v string) string {
	if numberRe.MatchString(v) {
		return v
	}
	return sqlString(dl, v)
}

// filterSpec is a guided filter: one column, one operator, picked values.
type filterSpec struct {
	col       string
	kind      db.Kind
	op        string
	vals      []string // raw values; nullItem stands for NULL
	keepNulls bool     // != also keeps NULL rows
}

var errOneValue = errors.New("one value")

// sql renders the filter for the dialect.
func (f filterSpec) sql(dl db.Dialect) (string, error) {
	q := dl.Quote(f.col)
	var lits, raws []string
	hasNull := false
	for _, v := range f.vals {
		if v == nullItem {
			hasNull = true
			continue
		}
		lits = append(lits, literal(dl, f.kind, v))
		raws = append(raws, v)
	}
	switch f.op {
	case "is null":
		return q + " IS NULL", nil
	case "is not null":
		return q + " IS NOT NULL", nil
	case "=", "in":
		if f.op == "=" && len(lits) > 1 {
			return "", errOneValue // several values: in
		}
		var c string
		switch len(lits) {
		case 0:
			if !hasNull {
				return "", errors.New("required")
			}
			return q + " IS NULL", nil
		case 1:
			c = q + " = " + lits[0]
		default:
			c = q + " IN (" + strings.Join(lits, ", ") + ")"
		}
		if hasNull {
			c = "(" + c + " OR " + q + " IS NULL)"
		}
		return c, nil
	case "!=", "not in":
		if f.op == "!=" && len(lits) > 1 {
			return "", errOneValue // several values: not in
		}
		var c string
		switch len(lits) {
		case 0:
			if !hasNull {
				return "", errors.New("required")
			}
			return q + " IS NOT NULL", nil
		case 1:
			c = q + " != " + lits[0]
		default:
			c = q + " NOT IN (" + strings.Join(lits, ", ") + ")"
		}
		if f.keepNulls && !hasNull {
			c = "(" + c + " OR " + q + " IS NULL)"
		}
		return c, nil
	case ">", ">=", "<", "<=":
		if len(lits) != 1 || hasNull {
			return "", errOneValue
		}
		return q + " " + f.op + " " + lits[0], nil
	case "contains", "starts with", "ends with":
		if len(raws) != 1 || hasNull {
			return "", errOneValue
		}
		pat, escaped := likeEscape(raws[0])
		switch f.op {
		case "contains":
			pat = "%" + pat + "%"
		case "starts with":
			pat += "%"
		default:
			pat = "%" + pat
		}
		c := q + " LIKE " + sqlString(dl, pat)
		if escaped || (dl.Name() == "mysql" && strings.Contains(pat, `\`)) {
			c += " ESCAPE '!'"
		}
		return c, nil
	}
	return "", errors.New("operator")
}

// likeEscape escapes LIKE wildcards with '!'. [ is a wildcard on SQL Server.
func likeEscape(v string) (string, bool) {
	if !strings.ContainsAny(v, "%_![") {
		return v, false
	}
	var b strings.Builder
	for _, r := range v {
		if strings.ContainsRune("%_![", r) {
			b.WriteByte('!')
		}
		b.WriteRune(r)
	}
	return b.String(), true
}

func likeUnescape(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '!' && i+1 < len(v) {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// parseFilter recovers a guided filter from a condition it rendered, or from the
// older unquoted form. ok is false for anything else.
func parseFilter(cond string, cols []string, kinds map[string]db.Kind, dl db.Dialect) (filterSpec, bool) {
	cond = strings.TrimSpace(cond)
	// (x OR col IS NULL)
	if strings.HasPrefix(cond, "(") && strings.HasSuffix(cond, ")") {
		parts := splitTopWord(cond[1:len(cond)-1], " OR ")
		if len(parts) == 2 {
			left, lok := parseFilter(parts[0], cols, kinds, dl)
			right, rok := parseFilter(parts[1], cols, kinds, dl)
			if lok && rok && right.op == "is null" && right.col == left.col {
				switch left.op {
				case "=", "in":
					left.vals = append(left.vals, nullItem)
					return left, true
				case "!=", "not in":
					left.keepNulls = true
					return left, true
				}
			}
		}
		return filterSpec{}, false
	}
	not := false
	if up := strings.ToUpper(cond); strings.HasPrefix(up, "NOT ") {
		not = true
		cond = strings.TrimSpace(cond[4:])
	}
	col, rest, ok := cutColumn(cond, cols, dl)
	if !ok {
		return filterSpec{}, false
	}
	f := filterSpec{col: col, kind: kinds[col]}
	up := strings.ToUpper(rest)
	switch {
	case not && !strings.HasPrefix(up, "IN "):
		return filterSpec{}, false
	case up == "IS NULL":
		f.op = "is null"
		return f, true
	case up == "IS NOT NULL":
		f.op = "is not null"
		return f, true
	case strings.HasPrefix(up, "IN ") || strings.HasPrefix(up, "NOT IN "):
		f.op = "in"
		if not || strings.HasPrefix(up, "NOT ") {
			f.op = "not in"
		}
		list := strings.TrimSpace(rest[strings.Index(up, "IN ")+3:])
		if !strings.HasPrefix(list, "(") || !strings.HasSuffix(list, ")") {
			return filterSpec{}, false
		}
		vals, ok := parseLiterals(list[1:len(list)-1], dl)
		if !ok || len(vals) == 0 {
			return filterSpec{}, false
		}
		f.vals = f.fromSQL(vals)
		return f, true
	case strings.HasPrefix(up, "LIKE "):
		lit := strings.TrimSpace(rest[5:])
		escaped := false
		if i := strings.LastIndex(strings.ToUpper(lit), " ESCAPE '!'"); i >= 0 && i+len(" ESCAPE '!'") == len(lit) {
			lit, escaped = strings.TrimSpace(lit[:i]), true
		}
		vals, ok := parseLiterals(lit, dl)
		if !ok || len(vals) != 1 {
			return filterSpec{}, false
		}
		v := vals[0]
		unesc := func(s string) string {
			if escaped {
				return likeUnescape(s)
			}
			return s
		}
		lead := strings.HasPrefix(v, "%")
		trail := len(v) >= 2 && strings.HasSuffix(v, "%") && (!escaped || (len(v)-1-len(strings.TrimRight(v[:len(v)-1], "!")))%2 == 0)
		switch {
		case lead && trail:
			f.op, f.vals = "contains", []string{unesc(v[1 : len(v)-1])}
		case trail:
			f.op, f.vals = "starts with", []string{unesc(v[:len(v)-1])}
		case lead:
			f.op, f.vals = "ends with", []string{unesc(v[1:])}
		default:
			return filterSpec{}, false
		}
		return f, true
	}
	for _, o := range []string{">=", "<=", "!=", "<>", "=", ">", "<"} {
		if !strings.HasPrefix(rest, o+" ") {
			continue
		}
		vals, ok := parseLiterals(strings.TrimSpace(rest[len(o):]), dl)
		if !ok || len(vals) != 1 {
			return filterSpec{}, false
		}
		if o == "<>" {
			o = "!="
		}
		f.op, f.vals = o, f.fromSQL(vals)
		return f, true
	}
	return filterSpec{}, false
}

// fromSQL turns parsed literals back into the values the value list shows.
func (f filterSpec) fromSQL(vals []string) []string {
	if f.kind != db.Bool {
		return vals
	}
	out := make([]string, len(vals))
	for i, v := range vals {
		if b, ok := parseBool(v); ok {
			v = map[bool]string{true: "true", false: "false"}[b]
		}
		out[i] = v
	}
	return out
}

// cutColumn finds the column a condition starts with: quoted for the dialect, in double
// quotes, or bare. Longer names first, so "a b" wins over "a".
func cutColumn(cond string, cols []string, dl db.Dialect) (string, string, bool) {
	best, rest := "", ""
	for _, c := range cols {
		for _, p := range []string{dl.Quote(c), `"` + strings.ReplaceAll(c, `"`, `""`) + `"`, c} {
			if strings.HasPrefix(cond, p+" ") && len(p) > len(best) {
				best, rest = c, strings.TrimSpace(cond[len(p):])
				if p != c {
					return c, rest, true
				}
			}
		}
	}
	return best, rest, best != ""
}

// parseLiterals reads a comma-separated list of SQL literals: quoted strings, numbers,
// TRUE and FALSE. Strings come back unquoted.
func parseLiterals(s string, dl db.Dialect) ([]string, bool) {
	var out []string
	s = strings.TrimSpace(s)
	for s != "" {
		var v string
		switch {
		case s[0] == '\'':
			var b strings.Builder
			i, closed := 1, false
			for i < len(s) {
				c := s[i]
				if c == '\\' && dl.Name() == "mysql" && i+1 < len(s) {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					closed = true
					i++
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed {
				return nil, false
			}
			v, s = b.String(), s[i:]
		default:
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			tok := strings.TrimSpace(s[:end])
			up := strings.ToUpper(tok)
			if !numberRe.MatchString(tok) && up != "TRUE" && up != "FALSE" {
				return nil, false
			}
			if up == "TRUE" || up == "FALSE" {
				tok = strings.ToLower(tok)
			}
			v, s = tok, s[end:]
		}
		out = append(out, v)
		s = strings.TrimSpace(s)
		if s == "" {
			break
		}
		if s[0] != ',' {
			return nil, false
		}
		s = strings.TrimSpace(s[1:])
		if s == "" {
			return nil, false
		}
	}
	return out, true
}

// splitTopWord splits on sep (matched case-insensitively) outside quotes and parentheses.
func splitTopWord(s, sep string) []string {
	var out []string
	depth, start := 0, 0
	var quote byte
	up := strings.ToUpper(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(up[i:], sep):
			out = append(out, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	return append(out, s[start:])
}

// splitArgs splits step arguments on commas outside quotes and parentheses.
func splitArgs(s string) []string {
	var out []string
	for _, p := range splitTopWord(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// matchCol returns the column named n, ignoring case.
func matchCol(cols []string, n string) (string, bool) {
	n = strings.TrimSpace(n)
	for _, c := range cols {
		if c == n {
			return c, true
		}
	}
	for _, c := range cols {
		if strings.EqualFold(c, n) {
			return c, true
		}
	}
	return "", false
}
