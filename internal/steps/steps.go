// Package steps compiles a pipeline of transformation steps into SQL for a dialect.
//
//	keep id, name | filter total > 10 | rename name = customer | sort -updated_at | limit 100
//
// Each step is "op args"; steps are separated by "|" ("||" is concatenation). Column names
// are quoted for the dialect; expressions (filter, set, coalesce values) are passed through
// as written. Each step applies to the output of the one before it.
package steps

import (
	"fmt"
	"strconv"
	"strings"

	"tummy/internal/db"
)

// op is one step kind: palette entry, argument check and SQL.
type op struct {
	name    string
	example string
	check   func(args string) error
	apply   func(b *builder, args string) error
}

// registry in palette order.
var registry = []op{
	{"keep", "id, name, total", needArgs("keep"), (*builder).keep},
	{"remove", "internal_notes, legacy_flag", needArgs("remove"), (*builder).remove},
	{"rename", "name = customer, total = amount", checkPairs("rename", true), (*builder).rename},
	{"filter", "total > 10 and status = 'paid'", needArgs("filter"), (*builder).filter},
	{"sort", "-updated_at, id", needArgs("sort"), (*builder).sort},
	{"limit", "1000", checkLimit, (*builder).limit},
	{"distinct", "", checkNoArgs, (*builder).distinct},
	{"trim", "name, email", needArgs("trim"), fn("TRIM")},
	{"upper", "country", needArgs("upper"), fn("UPPER")},
	{"lower", "email", needArgs("lower"), fn("LOWER")},
	{"cast", "total as float, placed as date", checkCast, (*builder).cast},
	{"set", "spent = total * qty", checkPairs("set", true), (*builder).set},
	{"coalesce", "note = '', qty = 0", checkPairs("coalesce", true), (*builder).coalesce},
}

var byName = map[string]*op{}

func init() {
	for i := range registry {
		byName[registry[i].name] = &registry[i]
	}
	for _, o := range registry {
		Ops = append(Ops, o.name)
		Examples[o.name] = o.example
	}
}

// Ops in palette order. A copy: changing it does not change Parse or Compile.
var Ops []string

// Examples of args per op, for forms. A copy, like Ops.
var Examples = map[string]string{}

// aliases accepted when parsing; rendered with the canonical name.
var aliases = map[string]string{"drop": "remove", "where": "filter"}

// Types accepted by cast.
var castTypes = map[string]db.Kind{
	"int": db.Int, "integer": db.Int, "bigint": db.Int,
	"float": db.Float, "double": db.Float, "real": db.Float,
	"decimal": db.Decimal, "numeric": db.Decimal,
	"text": db.String, "string": db.String, "varchar": db.String,
	"bool": db.Bool, "boolean": db.Bool,
	"date": db.Date, "timestamp": db.Timestamp, "datetime": db.Timestamp,
}

type Step struct {
	Op   string
	Args string
}

func (s Step) String() string {
	if s.Args == "" {
		return s.Op
	}
	return s.Op + " " + s.Args
}

// Parse splits a pipeline string into steps. Pipes inside quotes or parentheses, and "||", are kept.
func Parse(pipeline string) ([]Step, error) {
	var out []Step
	for _, part := range splitPipes(pipeline) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, args, _ := strings.Cut(part, " ")
		name = strings.ToLower(strings.TrimSpace(name))
		if a, ok := aliases[name]; ok {
			name = a
		}
		o, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown step %q", name)
		}
		st := Step{Op: name, Args: strings.TrimSpace(args)}
		if err := o.check(st.Args); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// Render joins steps back into a pipeline string.
func Render(steps []Step) string {
	parts := make([]string, len(steps))
	for i, s := range steps {
		parts[i] = s.String()
	}
	return strings.Join(parts, " | ")
}

// ---------- checks ----------

func needArgs(name string) func(string) error {
	return func(args string) error {
		if len(names(args)) == 0 {
			return fmt.Errorf("%s: arguments", name)
		}
		if name == "keep" {
			return noDuplicates(name, names(args))
		}
		return nil
	}
}

func checkNoArgs(args string) error {
	if args != "" {
		return fmt.Errorf("distinct: no arguments")
	}
	return nil
}

func checkLimit(args string) error {
	if n, err := strconv.Atoi(args); err != nil || n <= 0 {
		return fmt.Errorf("limit: positive number")
	}
	return nil
}

// checkPairs wants "name = value" pairs with both sides set and no name twice.
func checkPairs(name string, needValue bool) func(string) error {
	return func(args string) error {
		var seen []string
		for _, p := range splitTop(args, ',') {
			k, v, ok := strings.Cut(p, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if !ok || k == "" || (needValue && v == "") {
				return fmt.Errorf("%s: %q: name = value", name, strings.TrimSpace(p))
			}
			seen = append(seen, k)
		}
		return noDuplicates(name, seen)
	}
}

func checkCast(args string) error {
	for _, p := range splitTop(args, ',') {
		col, typ, ok := cutWord(p, " as ")
		if !ok || strings.TrimSpace(col) == "" {
			return fmt.Errorf("cast: %q: column as type", strings.TrimSpace(p))
		}
		if _, known := castTypes[strings.ToLower(strings.TrimSpace(typ))]; !known {
			return fmt.Errorf("cast: unknown type %q", strings.TrimSpace(typ))
		}
	}
	return nil
}

func noDuplicates(name string, list []string) error {
	seen := map[string]bool{}
	for _, n := range list {
		if seen[strings.ToLower(n)] {
			return fmt.Errorf("%s: %q twice", name, n)
		}
		seen[strings.ToLower(n)] = true
	}
	return nil
}

// ---------- compile ----------

// Compiled is a pipeline compiled for one dialect.
type Compiled struct {
	SQL string
	// Columns[i] are the columns step i sees; the last entry is the output.
	Columns [][]string
	origin  map[string]string
}

// Origin returns the input column whose values reach output column col unchanged,
// renamed or not.
func (c Compiled) Origin(col string) (string, bool) {
	o, ok := c.origin[strings.ToLower(col)]
	return o, ok
}

// Compile builds a SELECT for the dialect. base is a table name, or a query when isSQL.
// cols are the base's column names, needed to project drops, renames and functions.
func Compile(base string, isSQL bool, cols []string, pipeline []Step, dl db.Dialect) (string, error) {
	c, err := CompileWithColumns(base, isSQL, cols, pipeline, dl)
	return c.SQL, err
}

// CompileWithColumns is Compile plus the columns at each step. On error, Columns runs up to
// the failing step.
func CompileWithColumns(base string, isSQL bool, cols []string, pipeline []Step, dl db.Dialect) (Compiled, error) {
	b := &builder{dl: dl, layer: 1}
	if isSQL {
		q := strings.TrimSuffix(strings.TrimSpace(base), ";")
		if bareScan(q) {
			b.sql = q
		} else {
			b.sql = db.Wrap(dl, q, "*", "_t0")
		}
	} else {
		b.sql = "SELECT * FROM " + db.QuoteTable(dl, db.ParseTable(base))
	}
	for _, c := range cols {
		b.cur = append(b.cur, column{name: c, origin: c})
	}
	var out Compiled
	for _, st := range pipeline {
		out.Columns = append(out.Columns, b.names())
		o, ok := byName[st.Op]
		if !ok {
			return out, fmt.Errorf("unknown step %q", st.Op)
		}
		if err := o.apply(b, st.Args); err != nil {
			return out, err
		}
	}
	out.Columns = append(out.Columns, b.names())
	b.flush()
	out.SQL = b.sql
	out.origin = map[string]string{}
	for _, c := range b.cur {
		if c.origin != "" {
			out.origin[strings.ToLower(c.name)] = c.origin
		}
	}
	return out, nil
}

type column struct {
	name   string
	origin string // input column with the same values; "" once changed
}

// builder layers one SELECT per step over the previous one.
type builder struct {
	dl    db.Dialect
	sql   string
	cur   []column
	layer int
	order string // pending ORDER BY on the top layer
	rows  int    // pending row cap on the top layer
}

func (b *builder) names() []string {
	out := make([]string, len(b.cur))
	for i, c := range b.cur {
		out[i] = c.name
	}
	return out
}

func (b *builder) find(name string) (int, bool) {
	for i, c := range b.cur {
		if strings.EqualFold(c.name, name) {
			return i, true
		}
	}
	return -1, false
}

func (b *builder) need(op, name string) (int, error) {
	i, ok := b.find(name)
	if !ok {
		return -1, fmt.Errorf("%s: unknown column %q", op, name)
	}
	return i, nil
}

// flush closes a pending sort and limit into its own layer.
func (b *builder) flush() {
	if b.order != "" || b.rows > 0 {
		b.sql = b.dl.OrderLimit(b.sql, b.order, b.rows)
		b.order, b.rows = "", 0
	}
}

func (b *builder) wrap(sel, where string) {
	b.flush()
	s := db.Wrap(b.dl, b.sql, sel, "_t"+strconv.Itoa(b.layer))
	if where != "" {
		s += " WHERE " + where
	}
	b.sql = s
	b.layer++
}

// project selects the current columns with per-column expressions; changed columns lose their origin.
func (b *builder) project(exprs map[int]string) {
	sel := make([]string, len(b.cur))
	for i, c := range b.cur {
		if e, ok := exprs[i]; ok {
			sel[i] = e + " AS " + b.dl.Quote(c.name)
			b.cur[i].origin = ""
		} else {
			sel[i] = b.dl.Quote(c.name)
		}
	}
	b.wrap(strings.Join(sel, ", "), "")
}

func (b *builder) selectCols(next []column) {
	sel := make([]string, len(next))
	for i, c := range next {
		sel[i] = b.dl.Quote(c.name)
	}
	b.wrap(strings.Join(sel, ", "), "")
	b.cur = next
}

func (b *builder) keep(args string) error {
	var next []column
	for _, n := range names(args) {
		i, err := b.need("keep", n)
		if err != nil {
			return err
		}
		next = append(next, b.cur[i])
	}
	b.selectCols(next)
	return nil
}

func (b *builder) remove(args string) error {
	gone := map[int]bool{}
	for _, n := range names(args) {
		i, err := b.need("remove", n)
		if err != nil {
			return err
		}
		gone[i] = true
	}
	var next []column
	for i, c := range b.cur {
		if !gone[i] {
			next = append(next, c)
		}
	}
	if len(next) == 0 {
		return fmt.Errorf("remove: no columns left")
	}
	b.selectCols(next)
	return nil
}

func (b *builder) rename(args string) error {
	to := map[int]string{}
	for _, p := range splitTop(args, ',') {
		from, dest, _ := strings.Cut(p, "=")
		i, err := b.need("rename", strings.TrimSpace(from))
		if err != nil {
			return err
		}
		to[i] = strings.TrimSpace(dest)
	}
	next := make([]column, len(b.cur))
	sel := make([]string, len(b.cur))
	seen := map[string]bool{}
	for i, c := range b.cur {
		next[i] = c
		sel[i] = b.dl.Quote(c.name)
		if n, ok := to[i]; ok {
			next[i].name = n
			sel[i] += " AS " + b.dl.Quote(n)
		}
		if seen[strings.ToLower(next[i].name)] {
			return fmt.Errorf("rename: column %q exists", next[i].name)
		}
		seen[strings.ToLower(next[i].name)] = true
	}
	b.wrap(strings.Join(sel, ", "), "")
	b.cur = next
	return nil
}

func (b *builder) filter(args string) error {
	b.wrap("*", args)
	return nil
}

func (b *builder) sort(args string) error {
	var parts []string
	for _, n := range names(args) {
		desc := strings.HasPrefix(n, "-")
		i, err := b.need("sort", strings.TrimPrefix(n, "-"))
		if err != nil {
			return err
		}
		p := b.dl.Quote(b.cur[i].name)
		if desc {
			p += " DESC"
		}
		parts = append(parts, p)
	}
	b.flush()
	b.order = strings.Join(parts, ", ")
	return nil
}

func (b *builder) limit(args string) error {
	n, _ := strconv.Atoi(args)
	if b.rows > 0 {
		b.flush()
	}
	b.rows = n
	return nil
}

func (b *builder) distinct(string) error {
	b.wrap("DISTINCT *", "")
	return nil
}

func fn(name string) func(*builder, string) error {
	op := strings.ToLower(name)
	return func(b *builder, args string) error {
		exprs := map[int]string{}
		for _, n := range names(args) {
			i, err := b.need(op, n)
			if err != nil {
				return err
			}
			exprs[i] = name + "(" + b.dl.Quote(b.cur[i].name) + ")"
		}
		b.project(exprs)
		return nil
	}
}

func (b *builder) cast(args string) error {
	exprs := map[int]string{}
	for _, p := range splitTop(args, ',') {
		col, typ, _ := cutWord(p, " as ")
		i, err := b.need("cast", strings.TrimSpace(col))
		if err != nil {
			return err
		}
		kind := castTypes[strings.ToLower(strings.TrimSpace(typ))]
		exprs[i] = "CAST(" + b.dl.Quote(b.cur[i].name) + " AS " + b.dl.CastType(kind) + ")"
	}
	b.project(exprs)
	return nil
}

func (b *builder) coalesce(args string) error {
	exprs := map[int]string{}
	for _, p := range splitTop(args, ',') {
		col, val, _ := strings.Cut(p, "=")
		i, err := b.need("coalesce", strings.TrimSpace(col))
		if err != nil {
			return err
		}
		exprs[i] = "COALESCE(" + b.dl.Quote(b.cur[i].name) + ", " + strings.TrimSpace(val) + ")"
	}
	b.project(exprs)
	return nil
}

// set replaces an existing column in place or appends a new one.
func (b *builder) set(args string) error {
	exprs := map[int]string{}
	var added []column
	var addedExpr []string
	for _, p := range splitTop(args, ',') {
		name, expr, _ := strings.Cut(p, "=")
		name, expr = strings.TrimSpace(name), strings.TrimSpace(expr)
		if i, ok := b.find(name); ok {
			exprs[i] = expr
		} else {
			added = append(added, column{name: name})
			addedExpr = append(addedExpr, expr)
		}
	}
	sel := make([]string, 0, len(b.cur)+len(added))
	for i, c := range b.cur {
		if e, ok := exprs[i]; ok {
			sel = append(sel, e+" AS "+b.dl.Quote(c.name))
			b.cur[i].origin = ""
		} else {
			sel = append(sel, b.dl.Quote(c.name))
		}
	}
	for i, c := range added {
		sel = append(sel, addedExpr[i]+" AS "+b.dl.Quote(c.name))
	}
	b.wrap(strings.Join(sel, ", "), "")
	b.cur = append(b.cur, added...)
	return nil
}

// ---------- text helpers ----------

func names(args string) []string {
	var out []string
	for _, p := range splitTop(args, ',') {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitPipes splits on "|" outside quotes and parentheses; "||" stays.
func splitPipes(s string) []string {
	var out []string
	start := 0
	scan(s, func(i int) bool {
		switch {
		case s[i] != '|':
		case i+1 < len(s) && s[i+1] == '|':
			return true // skip the second bar
		default:
			out = append(out, s[start:i])
			start = i + 1
		}
		return false
	})
	return append(out, s[start:])
}

// splitTop splits on sep outside quotes and parentheses.
func splitTop(s string, sep byte) []string {
	var out []string
	start := 0
	scan(s, func(i int) bool {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
		return false
	})
	return append(out, s[start:])
}

// scan calls f for each byte outside quotes and parentheses. f returns true to skip the next byte.
func scan(s string, f func(i int) bool) {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0:
			if f(i) {
				i++
			}
		}
	}
}

func cutWord(s, word string) (string, string, bool) {
	i := strings.Index(strings.ToLower(s), word)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(word):], true
}

// bareScan reports a plain "SELECT * FROM <one quoted name>" with nothing after it.
func bareScan(sql string) bool {
	up := strings.ToUpper(sql)
	if !strings.HasPrefix(up, "SELECT * FROM ") {
		return false
	}
	rest := strings.TrimSpace(sql[len("SELECT * FROM "):])
	for _, kw := range []string{" WHERE ", " JOIN ", " GROUP ", " ORDER ", " LIMIT ", " UNION ", "(", ","} {
		if strings.Contains(strings.ToUpper(rest), kw) {
			return false
		}
	}
	return !strings.Contains(rest, " ")
}
