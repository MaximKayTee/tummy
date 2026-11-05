package db

import (
	"regexp"
	"strconv"
	"strings"
)

// SQL text shared by every dialect. Per-dialect differences go through Dialect.

// QuoteTable renders schema.name, or name alone without a schema.
func QuoteTable(dl Dialect, t Table) string {
	if t.Schema == "" {
		return dl.Quote(t.Name)
	}
	return dl.Quote(t.Schema) + "." + dl.Quote(t.Name)
}

func quoteList(dl Dialect, names []string) string {
	qs := make([]string, len(names))
	for i, n := range names {
		qs[i] = dl.Quote(n)
	}
	return strings.Join(qs, ", ")
}

func columnNames(cols []Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// CreateTableSQL creates t. Columns named in keys get index-sized types.
func CreateTableSQL(dl Dialect, t Table, cols []Column, keys []string) string {
	isKey := map[string]bool{}
	for _, k := range keys {
		isKey[strings.ToLower(k)] = true
	}
	defs := make([]string, len(cols))
	for i, c := range cols {
		defs[i] = dl.Quote(c.Name) + " " + dl.TypeName(c, isKey[strings.ToLower(c.Name)])
	}
	return "CREATE TABLE " + QuoteTable(dl, t) + " (" + strings.Join(defs, ", ") + ")"
}

func DropTableSQL(dl Dialect, t Table) string { return "DROP TABLE " + QuoteTable(dl, t) }

func AddColumnSQL(dl Dialect, t Table, c Column) string {
	return "ALTER TABLE " + QuoteTable(dl, t) + " ADD " + dl.Quote(c.Name) + " " + dl.TypeName(c, false)
}

func DropColumnSQL(dl Dialect, t Table, col string) string {
	return "ALTER TABLE " + QuoteTable(dl, t) + " DROP COLUMN " + dl.Quote(col)
}

func CreateIndexSQL(dl Dialect, name string, t Table, cols []string) string {
	return "CREATE INDEX " + dl.Quote(name) + " ON " + QuoteTable(dl, t) + " (" + quoteList(dl, cols) + ")"
}

// InsertSQL is a multi-row INSERT with placeholders for rows × len(cols) values.
func InsertSQL(dl Dialect, t Table, cols []Column, rows int) string {
	var sb strings.Builder
	sb.WriteString("INSERT INTO " + QuoteTable(dl, t) + " (" + quoteList(dl, columnNames(cols)) + ") VALUES ")
	p := 1
	for r := 0; r < rows; r++ {
		if r > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('(')
		for c := range cols {
			if c > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(dl.Placeholder(p))
			p++
		}
		sb.WriteByte(')')
	}
	return sb.String()
}

// InsertSelectSQL copies cols from src into target.
func InsertSelectSQL(dl Dialect, target, src Table, cols []Column) string {
	list := quoteList(dl, columnNames(cols))
	return "INSERT INTO " + QuoteTable(dl, target) + " (" + list + ") SELECT " + list + " FROM " + QuoteTable(dl, src)
}

// MaxSQL selects the highest value of c in t. Bytes take the first in descending order:
// postgres before 18 has no MAX for bytea.
func MaxSQL(dl Dialect, t Table, c Column) string {
	q := dl.Quote(c.Name)
	if c.Kind == Bytes {
		return dl.OrderLimit("SELECT "+q+" FROM "+QuoteTable(dl, t)+" WHERE "+q+" IS NOT NULL", q+" DESC", 1)
	}
	return "SELECT MAX(" + q + ") FROM " + QuoteTable(dl, t)
}

// DuplicatesSQL returns one row when two rows of t share keys.
func DuplicatesSQL(dl Dialect, t Table, keys []string) string {
	k := quoteList(dl, keys)
	return dl.OrderLimit("SELECT 1 AS d FROM "+QuoteTable(dl, t)+" GROUP BY "+k+" HAVING COUNT(*) > 1", "", 1)
}

func keysEqual(dl Dialect, a, b string, keys []string) string {
	conds := make([]string, len(keys))
	for i, k := range keys {
		qk := dl.Quote(k)
		conds[i] = a + "." + qk + " = " + b + "." + qk
	}
	return strings.Join(conds, " AND ")
}

// beats is true when row b replaces row a: higher update key (nulls lowest), then higher seq.
func beats(dl Dialect, updateKey, seq string) string {
	s := dl.Quote(seq)
	if updateKey == "" {
		return "_b." + s + " > _a." + s
	}
	u := dl.Quote(updateKey)
	return "(_b." + u + " > _a." + u +
		" OR (_a." + u + " IS NULL AND _b." + u + " IS NOT NULL)" +
		" OR ((_b." + u + " = _a." + u + " OR (_a." + u + " IS NULL AND _b." + u + " IS NULL)) AND _b." + s + " > _a." + s + "))"
}

// Wrap returns SELECT sel FROM (sql) AS alias. A leading WITH clause stays in front, and
// the dialect makes sql legal as a derived table.
func Wrap(dl Dialect, sql, sel, alias string) string {
	with, body := SplitWith(sql)
	out := "SELECT " + sel + " FROM (" + endComment(dl.Derived(body)) + ") AS " + alias
	if with != "" {
		out = with + " " + out
	}
	return out
}

var (
	reWith     = regexp.MustCompile(`(?i)^\s*WITH\s`)
	reMain     = regexp.MustCompile(`(?i)\b(SELECT|VALUES|TABLE)\b`)
	reOrderBy  = regexp.MustCompile(`(?i)\bORDER\s+BY\b`)
	reLimitEnd = regexp.MustCompile(`(?i)\b(LIMIT|OFFSET|FETCH)\b`)
	reOffset   = regexp.MustCompile(`(?i)\bOFFSET\b`)
	reTop      = regexp.MustCompile(`(?i)^\s*SELECT\s+((ALL|DISTINCT)\s+)?TOP\b`)
)

// SplitWith splits a leading WITH clause from the statement it belongs to.
// with is empty when sql has none.
func SplitWith(sql string) (with, body string) {
	w := reWith.FindStringIndex(sql)
	if w == nil {
		return "", sql
	}
	loc := reMain.FindStringIndex(mask(sql)[w[1]:])
	if loc == nil {
		return "", sql
	}
	i := w[1] + loc[0]
	return strings.TrimSpace(sql[:i]), strings.TrimSpace(sql[i:])
}

// topLevel reports whether re matches sql outside quotes, comments and parentheses.
func topLevel(sql string, re *regexp.Regexp) bool { return re.MatchString(mask(sql)) }

// endComment ends a trailing line comment so text can follow.
func endComment(sql string) string {
	if strings.Contains(sql[strings.LastIndexByte(sql, '\n')+1:], "--") {
		return sql + "\n"
	}
	return sql
}

// mask blanks quoted text, comments and everything inside parentheses, keeping the
// outermost parentheses. Byte offsets match sql.
func mask(sql string) string {
	b := []byte(sql)
	depth := 0
	blank := func(from, to int) {
		for k := from; k < to && k < len(b); k++ {
			b[k] = ' '
		}
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == '\'' || c == '"' || c == '`' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			j := i + 1
			for j < len(b) {
				if b[j] == end {
					if end != ']' && j+1 < len(b) && b[j+1] == end {
						j += 2
						continue
					}
					break
				}
				j++
			}
			blank(i, j+1)
			i = j
		case c == '-' && i+1 < len(b) && b[i+1] == '-':
			j := i
			for j < len(b) && b[j] != '\n' {
				j++
			}
			blank(i, j)
			i = j
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			j := strings.Index(sql[i+2:], "*/")
			end := len(b)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			blank(i, end)
			i = end - 1
		case c == '(':
			if depth > 0 {
				b[i] = ' '
			}
			depth++
		case c == ')':
			depth--
			if depth > 0 {
				b[i] = ' '
			}
		default:
			if depth > 0 {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// orderLimit is OrderLimit for dialects with LIMIT.
func orderLimit(dl Dialect, sql, order string, n int) string {
	out := sql
	switch {
	case order != "":
		out = Wrap(dl, sql, "*", "_t") + " ORDER BY " + order
	case n > 0 && topLevel(sql, reLimitEnd):
		out = Wrap(dl, sql, "*", "_t")
	}
	if n > 0 {
		out = endComment(out) + " LIMIT " + strconv.Itoa(n)
	}
	return out
}
