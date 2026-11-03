package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"tummy/internal/db"
	"tummy/internal/steps"
)

// stepContext is what a step sees: the columns before it, sample rows, the dialect,
// the column under the cursor, and the query before it for reading values.
type stepContext struct {
	cols     []db.Column
	rows     [][]any
	dl       db.Dialect
	cur      string // column under the grid cursor; new steps start on it
	base     string // query the step reads from
	args     []any
	distinct func(ctx context.Context, col string) ([]any, error) // values of a column at the source
}

// distinctMax is how many distinct values a filter's value list reads from the source.
const distinctMax = 40

// distinctValues reads the distinct values of a column from the query before a step.
func distinctValues(h *dbHandle, base string, args []any) func(context.Context, string) ([]any, error) {
	return func(ctx context.Context, col string) ([]any, error) {
		d, err := h.get(ctx)
		if err != nil {
			return nil, err
		}
		return d.Distinct(ctx, base, col, distinctMax+1, args...)
	}
}

// valuesMsg brings a column's values from the source to the filter that asked.
type valuesMsg struct {
	ed      *stepEditor
	col     string
	vals    []string
	hasNull bool
	more    bool // the source has more than distinctMax
	err     error
}

func (c stepContext) names() []string {
	out := make([]string, len(c.cols))
	for i, col := range c.cols {
		out[i] = col.Name
	}
	return out
}

func (c stepContext) kinds() map[string]db.Kind {
	out := map[string]db.Kind{}
	for _, col := range c.cols {
		out[col.Name] = col.Kind
	}
	return out
}

func (c stepContext) index(col string) int {
	for i, x := range c.cols {
		if x.Name == col {
			return i
		}
	}
	return -1
}

// values lists the distinct values of a column in the sample, as filter values.
func (c stepContext) values(col string) (vals []string, hasNull bool) {
	i := c.index(col)
	if i < 0 {
		return nil, false
	}
	seen := map[string]bool{}
	for _, r := range c.rows {
		if i >= len(r) {
			continue
		}
		if r[i] == nil {
			hasNull = true
			continue
		}
		s, ok := valueText(r[i], c.cols[i].Kind)
		if !ok || seen[s] {
			continue
		}
		seen[s] = true
		vals = append(vals, s)
	}
	sortValues(vals)
	return vals, hasNull
}

// stepEditor builds one step from the columns it sees: pickers for column steps,
// a form for the rest, a second stage for sort, cast and coalesce.
type stepEditor struct {
	op      string
	ctx     stepContext
	cols    []string // columns step arguments can name
	stage   int
	pick    *picker
	form    *form
	result  steps.Step
	err     string
	lastCol string               // filter: column the value list was built for
	srcVals map[string]valuesMsg // filter: values read from the source, per column
	prior   map[string]string    // sort, cast, coalesce: per-column args of the step being edited
	pairs   int                  // set: existing name = expression rows
}

var castTypeOptions = []string{"int", "float", "decimal", "text", "bool", "date", "timestamp"}

var setBuilds = []string{"expression", "case", "concat", "date part"}

var separators = []string{"space", "none", "comma", "comma space", "dash", "slash", "underscore"}

var separatorText = map[string]string{"space": " ", "none": "", "comma": ",", "comma space": ", ", "dash": "-", "slash": "/", "underscore": "_"}

var dateParts = []string{"year", "month", "day", "hour", "minute", "date"}

func newStepEditor(op string, ctx stepContext, existing *steps.Step) *stepEditor {
	if ctx.dl == nil {
		ctx.dl, _ = db.DialectFor("postgres")
	}
	e := &stepEditor{op: op, ctx: ctx, prior: map[string]string{}, srcVals: map[string]valuesMsg{}}
	for _, c := range ctx.names() {
		if argSafe(c) {
			e.cols = append(e.cols, c)
		}
	}
	args := ""
	if existing != nil && existing.Op == op {
		args = existing.Args
	}
	e.start(args)
	return e
}

// textOps work on text only.
var textOps = []string{"trim", "upper", "lower"}

// columnsFor are the columns an op can take: text ones for trim, upper and lower.
func (e *stepEditor) columnsFor(op string, keep []string) []string {
	if !slices.Contains(textOps, op) {
		return e.cols
	}
	kinds := e.ctx.kinds()
	var out []string
	for _, c := range e.cols {
		if kinds[c] == db.String || slices.Contains(keep, c) {
			out = append(out, c)
		}
	}
	return out
}

func (e *stepEditor) start(args string) {
	switch e.op {
	case "keep", "remove", "trim", "upper", "lower", "sort", "cast", "coalesce":
		var pre []string
		for _, a := range splitArgs(args) {
			name, val := strings.TrimPrefix(a, "-"), ""
			switch e.op {
			case "cast":
				name, val, _ = cutFold(a, " as ")
			case "coalesce":
				name, val, _ = strings.Cut(a, "=")
			case "sort":
				if strings.HasPrefix(a, "-") {
					val = "desc"
				}
			}
			if c, ok := matchCol(e.cols, name); ok && !slices.Contains(pre, c) {
				pre = append(pre, c)
				e.prior[c] = strings.TrimSpace(val)
			}
		}
		items := e.columnsFor(e.op, pre)
		cur := args == "" && e.ctx.cur != "" && slices.Contains(items, e.ctx.cur)
		if cur && e.op != "keep" {
			pre = []string{e.ctx.cur} // a new step starts on the cursor column
		}
		e.pick = newPicker(e.op, items, pre)
		if cur {
			e.pick.cur = slices.Index(items, e.ctx.cur)
		}
	case "rename":
		to := map[string]string{}
		for _, p := range splitArgs(args) {
			from, dest, _ := strings.Cut(p, "=")
			if c, ok := matchCol(e.cols, from); ok {
				to[c] = strings.TrimSpace(dest)
			}
		}
		var fields []*field
		for _, c := range e.cols {
			fields = append(fields, text(c, c, "").def(to[c]))
		}
		e.form = newForm("rename  blank: unchanged", fields...)
		if fl := e.form.field(e.ctx.cur); fl != nil && args == "" {
			e.form.focused = fl
			e.form.focus()
		}
	case "set":
		e.form = e.setForm(args)
	case "filter":
		e.form = e.filterForm(args)
	case "limit":
		e.form = newForm("limit", text("n", "Rows", "").def(args).required().check(func(v string) error {
			if n, err := strconv.Atoi(v); err != nil || n <= 0 {
				return errors.New("positive number")
			}
			return nil
		}))
	case "distinct":
		e.result = steps.Step{Op: "distinct"}
	}
}

func cutFold(s, word string) (string, string, bool) {
	i := strings.Index(strings.ToLower(s), word)
	if i < 0 {
		return strings.TrimSpace(s), "", false
	}
	return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(word):]), true
}

func (e *stepEditor) focus() tea.Cmd {
	if e.form != nil {
		return e.form.focus()
	}
	return nil
}

// done reports whether the step is complete (distinct needs no input).
func (e *stepEditor) done() bool { return e.op == "distinct" }

// valueDrop reports the open value list of the filter.
func (e *stepEditor) valueDrop() *picker {
	if e.form == nil || e.form.drop == nil {
		return nil
	}
	vis := e.form.visible()
	if e.form.cur >= len(vis) || vis[e.form.cur].Key != "val" {
		return nil
	}
	return e.form.drop
}

// keys are the footer keys of the editor and the key that goes back.
func (e *stepEditor) keys() ([]string, string) {
	back := helpLine("esc", "cancel")
	switch {
	case e.pick != nil && e.stage == 0:
		enter := "apply"
		if e.op == "sort" || e.op == "cast" || e.op == "coalesce" {
			enter = "next"
		}
		if e.pick.typing {
			return []string{"type", "filter", "enter", "done"}, helpLine("esc", "clear")
		}
		return []string{"space", "tick", "enter", enter, "/", "filter", "ctrl+a/n", "all/none"}, back
	case e.valueDrop() != nil:
		d := e.valueDrop()
		tick := "space"
		if d.filter.Value() != "" {
			tick = "tab"
		}
		enter := "done"
		if e.form.onLast() {
			enter = "apply"
		}
		pairs := []string{"type", "filter", tick, "tick", "enter", enter}
		if !singleOp(e.form.get("op")) {
			pairs = append(pairs, "ctrl+a/n", "all/none")
		}
		return pairs, helpLine("esc", "close")
	case e.form != nil && e.form.drop != nil:
		return e.form.hints(), helpLine("esc", "close")
	case e.stage == 1 && e.op == "sort":
		return []string{"space", "desc", "ctrl+s", "apply"}, helpLine("esc", "back")
	case e.form != nil:
		if e.stage == 1 {
			back = helpLine("esc", "back")
		}
		h := e.form.hints()
		if e.form.focused != nil && e.form.focused.Key == "val" && singleOp(e.form.get("op")) {
			h = dropPair(h, "ctrl+a/n")
		}
		return append(h, "ctrl+s", "apply"), back
	}
	return []string{"ctrl+s", "apply"}, back
}

// dropPair removes the pair for key from footer pairs.
func dropPair(h []string, key string) []string {
	var out []string
	for i := 0; i+1 < len(h); i += 2 {
		if h[i] != key {
			out = append(out, h[i], h[i+1])
		}
	}
	return out
}

// typing reports whether keys go to text or an open list.
func (e *stepEditor) typing() bool {
	if e.pick != nil && e.stage == 0 {
		return e.pick.typing
	}
	return e.form != nil && e.form.typing()
}

func (e *stepEditor) update(msg tea.KeyMsg) (formResult, tea.Cmd) {
	switch {
	case e.pick != nil && e.stage == 0:
		res, cmd := e.pick.update(msg)
		if res != formSubmit {
			return res, cmd
		}
		chosen := e.pick.chosenInOrder()
		if len(chosen) == 0 {
			return formContinue, nil
		}
		switch e.op {
		case "sort", "cast", "coalesce":
			return formContinue, e.second()
		case "keep":
			e.result = steps.Step{Op: "keep", Args: strings.Join(chosen, ", ")}
		default:
			e.result = steps.Step{Op: e.op, Args: strings.Join(e.pick.chosen(), ", ")}
		}
		return formSubmit, nil
	case e.form != nil:
		if res, cmd, ok := e.valueKeys(msg); ok {
			return res, cmd
		}
		if msg.String() == "ctrl+a" && e.op == "filter" && e.form.focused != nil && e.form.focused.Key == "val" && singleOp(e.form.get("op")) {
			return formContinue, nil // one value
		}
		res, cmd := e.form.update(msg)
		if e.op == "filter" {
			cmd = tea.Batch(cmd, e.follow())
		}
		if res == formCancel && e.stage == 1 {
			e.stage, e.form, e.err = 0, nil, ""
			return formContinue, nil
		}
		if res != formSubmit {
			return res, cmd
		}
		st, err := e.build()
		if err != nil {
			e.err = err.Error()
			for _, fl := range e.form.visible() {
				if fl.err != "" {
					e.err = "" // shown under the field
				}
			}
			return formContinue, nil
		}
		e.err = ""
		e.result = st
		return formSubmit, nil
	}
	return formCancel, nil
}

// valueKeys: in the open value list, space ticks until text is typed, then types; tab
// always ticks. With = and != a second value switches to in and not in; the other
// operators take one value, so a tick replaces the last.
func (e *stepEditor) valueKeys(msg tea.KeyMsg) (formResult, tea.Cmd, bool) {
	d := e.valueDrop()
	if d == nil {
		return formContinue, nil, false
	}
	switch msg.String() {
	case " ":
		if d.filter.Value() != "" {
			var cmd tea.Cmd
			d.filter, cmd = d.filter.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
			d.cur = 0
			return formContinue, cmd, true
		}
		e.tickValue(d)
		return formContinue, nil, true
	case "tab":
		it, ok := d.current()
		if !ok {
			return formContinue, nil, true
		}
		e.tickValue(d) // a typed value becomes a row of its own
		if d.filter.Value() != "" {
			d.filter.SetValue("")
			d.cur = max(slices.Index(d.visible(), it), 0)
		}
		return formContinue, nil, true
	case "ctrl+a":
		if singleOp(e.form.get("op")) {
			return formContinue, nil, true
		}
	}
	return formContinue, nil, false
}

// tickValue ticks the value under the cursor of the open value list.
func (e *stepEditor) tickValue(d *picker) {
	it, ok := d.current()
	if !ok {
		return
	}
	op := e.form.get("op")
	if d.selected[it] || !singleOp(op) {
		d.toggle(it)
		return
	}
	values := 0
	for _, o := range d.chosen() {
		if o != nullItem {
			values++
		}
	}
	switch {
	case (op == "=" || op == "!=") && it != nullItem && values > 0:
		e.form.set("op", map[string]string{"=": "in", "!=": "not in"}[op]) // a second value
	case op != "=" && op != "!=":
		for _, o := range d.chosen() {
			d.toggle(o) // one value: the new tick replaces the old
		}
	}
	d.toggle(it)
}

// second opens the per-column stage of sort, cast and coalesce for the ticked columns.
func (e *stepEditor) second() tea.Cmd {
	var fields []*field
	for _, c := range e.pick.chosenInOrder() {
		switch e.op {
		case "sort":
			fields = append(fields, toggle(c, c, "", e.prior[c] == "desc"))
		case "cast":
			typ := strings.ToLower(e.prior[c])
			if !slices.Contains(castTypeOptions, typ) {
				typ = "text"
			}
			fields = append(fields, choice(c, c, "", castTypeOptions...).def(typ))
		case "coalesce":
			fields = append(fields, text(c, c, "0   ''   'unknown'").def(e.prior[c]).required())
		}
	}
	title := map[string]string{"sort": "sort  on: desc", "cast": "cast", "coalesce": "coalesce  default"}[e.op]
	e.form = newForm(title, fields...)
	e.stage = 1
	return e.form.focus()
}

func (e *stepEditor) build() (steps.Step, error) {
	f := e.form
	switch e.op {
	case "sort", "cast", "coalesce":
		var parts []string
		for _, c := range e.pick.chosenInOrder() {
			switch e.op {
			case "sort":
				if f.on(c) {
					c = "-" + c
				}
				parts = append(parts, c)
			case "cast":
				parts = append(parts, c+" as "+f.get(c))
			case "coalesce":
				parts = append(parts, c+" = "+f.get(c))
			}
		}
		return steps.Step{Op: e.op, Args: strings.Join(parts, ", ")}, nil
	case "rename":
		return e.renameResult()
	case "set":
		return e.setResult()
	case "filter":
		if f.get("mode") == "raw" {
			return steps.Step{Op: "filter", Args: f.get("expr")}, nil
		}
		cond, err := e.filterSpec().sql(e.ctx.dl)
		if err != nil {
			if vf := f.field("val"); vf != nil {
				vf.err = err.Error()
			}
			return steps.Step{}, err
		}
		return steps.Step{Op: "filter", Args: cond}, nil
	case "limit":
		return steps.Step{Op: "limit", Args: f.get("n")}, nil
	}
	return steps.Step{Op: e.op}, nil
}

func (e *stepEditor) renameResult() (steps.Step, error) {
	final := map[string]string{}
	for _, c := range e.ctx.names() {
		final[strings.ToLower(c)] = c
	}
	var parts []string
	for _, c := range e.cols {
		fl := e.form.field(c)
		v := e.form.get(c)
		if v == "" || v == c {
			continue
		}
		if !argSafe(v) {
			fl.err = "no | , = ' \" ( ) [ ]"
			return steps.Step{}, errors.New(fl.err)
		}
		delete(final, strings.ToLower(c))
		if _, taken := final[strings.ToLower(v)]; taken {
			fl.err = "exists"
			return steps.Step{}, errors.New("exists")
		}
		final[strings.ToLower(v)] = v
		parts = append(parts, c+" = "+v)
	}
	if len(parts) == 0 {
		return steps.Step{}, errors.New("no change")
	}
	return steps.Step{Op: "rename", Args: strings.Join(parts, ", ")}, nil
}

// ---------- set ----------

func (e *stepEditor) setForm(args string) *form {
	var fields []*field
	parts := splitArgs(args)
	var guided map[string]string
	var gcols []string
	if len(parts) == 1 {
		name, expr, _ := strings.Cut(parts[0], "=")
		if g, cols, ok := e.guidedSet(strings.TrimSpace(expr)); ok {
			guided, gcols, parts = g, cols, nil
			guided["name"] = strings.TrimSpace(name)
		}
	}
	for _, p := range parts {
		name, expr, _ := strings.Cut(p, "=")
		i := strconv.Itoa(e.pairs)
		fields = append(fields,
			text("name"+i, "Column", "").def(strings.TrimSpace(name)),
			text("expr"+i, "Expression", "").def(strings.TrimSpace(expr)),
		)
		e.pairs++
	}
	builds := setBuilds
	if e.pairs > 0 {
		builds = append([]string{"none"}, setBuilds...)
	}
	is := func(b string) func(*form) bool { return func(f *form) bool { return f.get("build") == b } }
	adding := func(f *form) bool { return f.get("build") != "none" }
	needsValue := func(f *form) bool {
		o := f.get("wop")
		return is("case")(f) && o != "is null" && o != "is not null"
	}
	var dateCols []string
	for _, c := range e.ctx.cols {
		if argSafe(c.Name) && (c.Kind == db.Date || c.Kind == db.Timestamp) {
			dateCols = append(dateCols, c.Name)
		}
	}
	if len(dateCols) == 0 {
		dateCols = e.cols
	}
	caseOps := []string{"=", "!=", ">", ">=", "<", "<=", "is null", "is not null"}
	fields = append(fields,
		choice("build", "Add", "", builds...),
		text("name", "Column", "new or existing").when(adding).required(),
		text("expr", "Expression", strings.Join(e.cols, ", ")).when(is("expression")).required(),
		choice("wcol", "When", "", e.cols...).when(is("case")),
		choice("wop", "Is", "", caseOps...).when(is("case")),
		text("wval", "Value", "").when(needsValue).required(),
		text("then", "Then", "").when(is("case")).required(),
		text("else", "Else", "blank: null").when(is("case")),
		multi("ccols", "Columns", "", e.cols, nil).when(is("concat")).required(),
		choice("sep", "Separator", "", separators...).when(is("concat")),
		choice("dcol", "Column", "", dateCols...).when(is("date part")),
		choice("part", "Part", "", dateParts...).when(is("date part")),
	)
	f := newForm("set", fields...)
	if guided == nil && args == "" && e.ctx.cur != "" {
		guided = map[string]string{"wcol": e.ctx.cur}
		if slices.Contains(dateCols, e.ctx.cur) {
			guided["dcol"] = e.ctx.cur
		}
	}
	for k, v := range guided {
		f.set(k, v)
	}
	for _, c := range gcols {
		f.field("ccols").Picked[c] = true
	}
	return f
}

// guidedSet recovers the guided build that wrote expr: concat, date part or case. It
// returns the build's field values and, for concat, the columns.
func (e *stepEditor) guidedSet(expr string) (map[string]string, []string, bool) {
	dl := e.ctx.dl
	up := strings.ToUpper(expr)
	switch {
	case strings.HasPrefix(up, "CONCAT_WS(") && strings.HasSuffix(expr, ")"):
		args := splitArgs(expr[len("CONCAT_WS(") : len(expr)-1])
		if len(args) < 2 {
			return nil, nil, false
		}
		sep, ok := parseLiterals(args[0], dl)
		if !ok || len(sep) != 1 {
			return nil, nil, false
		}
		name := ""
		for n, t := range separatorText {
			if t == sep[0] {
				name = n
			}
		}
		if name == "" {
			return nil, nil, false
		}
		var cols []string
		for _, a := range args[1:] {
			i := slices.IndexFunc(e.cols, func(c string) bool { return dl.Quote(c) == a })
			if i < 0 {
				return nil, nil, false
			}
			cols = append(cols, e.cols[i])
		}
		return map[string]string{"build": "concat", "sep": name}, cols, true
	case strings.HasPrefix(up, "CASE WHEN ") && strings.HasSuffix(up, " END"):
		body := expr[len("CASE WHEN ") : len(expr)-len(" END")]
		parts := splitTopWord(body, " THEN ")
		if len(parts) != 2 {
			return nil, nil, false
		}
		spec, ok := parseFilter(strings.TrimSpace(parts[0]), e.cols, e.ctx.kinds(), dl)
		if !ok || len(spec.vals) > 1 || slices.Contains(spec.vals, nullItem) || !slices.Contains([]string{"=", "!=", ">", ">=", "<", "<=", "is null", "is not null"}, spec.op) {
			return nil, nil, false
		}
		out := map[string]string{"build": "case", "wcol": spec.col, "wop": spec.op}
		if len(spec.vals) == 1 {
			out["wval"] = spec.vals[0]
		}
		results := splitTopWord(parts[1], " ELSE ")
		for i, key := range []string{"then", "else"} {
			if i >= len(results) {
				break
			}
			v, ok := parseLiterals(strings.TrimSpace(results[i]), dl)
			if !ok || len(v) != 1 {
				return nil, nil, false
			}
			out[key] = v[0]
		}
		return out, nil, true
	}
	for _, c := range e.cols {
		for _, part := range dateParts {
			if datePart(dl, part, dl.Quote(c)) == expr {
				return map[string]string{"build": "date part", "dcol": c, "part": part}, nil, true
			}
		}
	}
	return nil, nil, false
}

func (e *stepEditor) setResult() (steps.Step, error) {
	f := e.form
	var parts []string
	seen := map[string]bool{}
	add := func(fl *field, name, expr string) error {
		switch {
		case !argSafe(name):
			fl.err = "no | , = ' \" ( ) [ ]"
		case seen[strings.ToLower(name)]:
			fl.err = "twice"
		case expr == "":
			fl.err = "expression"
		default:
			seen[strings.ToLower(name)] = true
			parts = append(parts, name+" = "+expr)
			return nil
		}
		return errors.New(fl.err)
	}
	for i := 0; i < e.pairs; i++ {
		n := strconv.Itoa(i)
		name, expr := f.get("name"+n), f.get("expr"+n)
		if name == "" && expr == "" {
			continue
		}
		if err := add(f.field("name"+n), name, expr); err != nil {
			return steps.Step{}, err
		}
	}
	if b := f.get("build"); b != "none" {
		expr, err := e.built(b)
		if err != nil {
			return steps.Step{}, err
		}
		if err := add(f.field("name"), f.get("name"), expr); err != nil {
			return steps.Step{}, err
		}
	}
	if len(parts) == 0 {
		return steps.Step{}, errors.New("no change")
	}
	return steps.Step{Op: "set", Args: strings.Join(parts, ", ")}, nil
}

// built renders the guided expression of a set row.
func (e *stepEditor) built(b string) (string, error) {
	f, dl := e.form, e.ctx.dl
	switch b {
	case "case":
		col := f.get("wcol")
		cond, err := filterSpec{col: col, kind: e.ctx.kinds()[col], op: f.get("wop"), vals: []string{f.get("wval")}}.sql(dl)
		if err != nil {
			return "", err
		}
		out := "CASE WHEN " + cond + " THEN " + looseLiteral(dl, f.get("then"))
		if v := f.get("else"); v != "" {
			out += " ELSE " + looseLiteral(dl, v)
		}
		return out + " END", nil
	case "concat":
		cols := f.picked("ccols")
		q := make([]string, len(cols))
		for i, c := range cols {
			q[i] = dl.Quote(c)
		}
		return "CONCAT_WS(" + sqlString(dl, separatorText[f.get("sep")]) + ", " + strings.Join(q, ", ") + ")", nil
	case "date part":
		return datePart(dl, f.get("part"), dl.Quote(f.get("dcol"))), nil
	}
	return f.get("expr"), nil
}

// datePart extracts part of a date or timestamp; "date" drops the time.
func datePart(dl db.Dialect, part, q string) string {
	switch dl.Name() {
	case "sqlite":
		if part == "date" {
			return "date(" + q + ")"
		}
		f := map[string]string{"year": "%Y", "month": "%m", "day": "%d", "hour": "%H", "minute": "%M"}[part]
		return "CAST(strftime('" + f + "', " + q + ") AS INTEGER)"
	case "mssql":
		if part == "date" {
			return "CAST(" + q + " AS date)"
		}
		return "DATEPART(" + part + ", " + q + ")"
	case "mysql":
		if part == "date" {
			return "DATE(" + q + ")"
		}
		return "EXTRACT(" + strings.ToUpper(part) + " FROM " + q + ")"
	}
	if part == "date" {
		return "CAST(" + q + " AS date)"
	}
	return "CAST(EXTRACT(" + strings.ToUpper(part) + " FROM " + q + ") AS integer)"
}

// ---------- filter ----------

func (e *stepEditor) filterForm(args string) *form {
	kinds := e.ctx.kinds()
	spec, ok := filterSpec{}, true
	if args != "" {
		spec, ok = parseFilter(args, e.cols, kinds, e.ctx.dl)
	}
	mode := "guided"
	if !ok {
		mode = "raw"
	}
	guided := func(f *form) bool { return f.get("mode") == "guided" }
	raw := func(f *form) bool { return f.get("mode") == "raw" }
	needsValue := func(f *form) bool {
		o := f.get("op")
		return guided(f) && o != "is null" && o != "is not null"
	}
	col := spec.col
	if col == "" && slices.Contains(e.cols, e.ctx.cur) {
		col = e.ctx.cur
	}
	if col == "" && len(e.cols) > 0 {
		col = e.cols[0]
	}
	e.lastCol = col
	keep := spec.keepNulls || args == "" || !ok
	f := newForm("filter rows",
		choice("mode", "Mode", "", "guided", "raw").def(mode),
		choice("col", "Column", "", e.cols...).def(col).when(guided),
		choice("op", "Is", "", opsFor(kinds[col])...).def(cmpOr(spec.op, "=")).when(guided).notes(opNotes),
		multi("val", "Value", "", e.valueItems(col), spec.vals).when(needsValue).required(),
		toggle("nulls", "Keep nulls", "", keep).when(func(f *form) bool { return guided(f) && (f.get("op") == "!=" || f.get("op") == "not in") }),
		text("expr", "Condition", strings.Join(e.cols, ", ")).def(args).when(raw).required(),
	)
	f.note = e.sampleNote()
	return f
}

// opNotes describe the guided operators in their list.
var opNotes = map[string]string{
	"contains":    "LIKE %x%",
	"starts with": "LIKE x%",
	"ends with":   "LIKE %x",
	"in":          "one of several values",
	"not in":      "none of several values",
	"is null":     "no value",
	"is not null": "any value",
}

// singleOp: the operator takes one value.
func singleOp(op string) bool {
	switch op {
	case "in", "not in", "is null", "is not null":
		return false
	}
	return true
}

// sampleNote labels a value list built from the loaded rows.
func (e *stepEditor) sampleNote() string {
	return fmt.Sprintf("values from %d-row sample", len(e.ctx.rows))
}

// loadValues reads the values of the filter's column from the source, once per column.
func (e *stepEditor) loadValues() tea.Cmd {
	if e.op != "filter" || e.form == nil || e.ctx.distinct == nil {
		return nil
	}
	col := e.form.get("col")
	if col == "" {
		return nil
	}
	if m, ok := e.srcVals[col]; ok {
		e.setValues(m)
		return nil
	}
	read, kind := e.ctx.distinct, e.ctx.kinds()[col]
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		raw, err := read(ctx, col)
		msg := valuesMsg{ed: e, col: col, err: err}
		for _, v := range raw {
			if v == nil {
				msg.hasNull = true
				continue
			}
			if s, ok := valueText(v, kind); ok {
				msg.vals = append(msg.vals, s)
			}
		}
		if len(raw) > distinctMax {
			msg.more = true
			if len(msg.vals) > distinctMax {
				msg.vals = msg.vals[:distinctMax]
			}
		}
		sortValues(msg.vals)
		return msg
	}
}

// setValues puts values read from the source into the value list of their column.
func (e *stepEditor) setValues(m valuesMsg) {
	e.srcVals[m.col] = m
	if e.form == nil || e.form.get("col") != m.col {
		return
	}
	if m.err != nil {
		e.form.note = e.sampleNote()
		return
	}
	items := m.vals
	if m.hasNull {
		items = append([]string{nullItem}, items...)
	}
	e.form.field("val").Options = items
	e.form.note = "values from source"
	if m.more {
		e.form.note = fmt.Sprintf("first %d values from source", distinctMax)
	}
	if d := e.valueDrop(); d != nil {
		keep := slices.Clone(items)
		for _, it := range d.items {
			if d.selected[it] && !slices.Contains(keep, it) {
				keep = append(keep, it)
			}
		}
		d.items = keep
		d.cur = min(d.cur, max(len(d.visible())-1, 0))
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// follow keeps the operator and value lists in step with the column; values of a new
// column are read from the source.
func (e *stepEditor) follow() tea.Cmd {
	col := e.form.get("col")
	if col == e.lastCol {
		return nil
	}
	e.lastCol = col
	ops := opsFor(e.ctx.kinds()[col])
	of := e.form.field("op")
	of.Options = ops
	if !slices.Contains(ops, of.Value) {
		of.Value = "="
	}
	vf := e.form.field("val")
	vf.Options = e.valueItems(col)
	vf.Picked = map[string]bool{}
	e.form.note = e.sampleNote()
	return e.loadValues()
}

// valueItems lists a column's sample values, NULL first when the sample has it.
func (e *stepEditor) valueItems(col string) []string {
	vals, hasNull := e.ctx.values(col)
	if hasNull {
		return append([]string{nullItem}, vals...)
	}
	return vals
}

func (e *stepEditor) filterSpec() filterSpec {
	f := e.form
	col := f.get("col")
	vals := f.picked("val")
	op := f.get("op")
	if op == "is null" || op == "is not null" {
		vals = nil
	}
	return filterSpec{col: col, kind: e.ctx.kinds()[col], op: op, vals: vals, keepNulls: f.on("nulls")}
}

func (e *stepEditor) view(width, height int) []string {
	var out []string
	switch {
	case e.pick != nil && e.stage == 0:
		out = e.pick.view(width, height)
	case e.form != nil:
		out = e.form.view(width)
	default:
		out = []string{" " + sDim.Render(fmt.Sprintf("%s: no input", e.op))}
	}
	if e.err != "" {
		out = append(out, "")
		for _, l := range strings.Split(ansi.Wrap(glyphErr+" "+txt.Clean(e.err), max(width-4, 10), " "), "\n") {
			out = append(out, "  "+sErr.Render(l))
		}
	}
	return out
}
