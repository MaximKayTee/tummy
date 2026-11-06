package tui

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"tummy/internal/db"
	"tummy/internal/steps"
)

func parseSteps(s string) ([]steps.Step, error) { return steps.Parse(s) }
func renderSteps(p []steps.Step) string         { return steps.Render(p) }

// prefixMsg is the preview before one step, for editing it.
type prefixMsg struct {
	idx  int
	seq  int
	Cols []db.Column
	Rows [][]any
	SQL  string
	Args []any
	Err  error
}

func (e *explorer) keySteps(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	n := len(e.pipeline)
	switch msg.String() {
	case "esc", "q":
		e.focus = focusGrid
	case "up", "k":
		if e.panelOff > 0 && (n == 0 || e.stepCur == n-1) {
			e.panelOff-- // scrolled past the last step: scroll back first
		} else {
			e.stepCur = max(e.stepCur-1, 0)
		}
	case "down", "j":
		if e.stepCur+1 < n {
			e.stepCur++
		} else {
			e.panelOff++ // past the last step: scroll the sql
		}
	case "pgup":
		e.panelOff = max(e.panelOff-10, 0)
	case "pgdown":
		e.panelOff += 10
	case "g", "home":
		e.stepCur, e.panelOff = 0, 0
	case "G", "end":
		e.stepCur = max(n-1, 0)
	case "a":
		return wizContinue, e.openPalette(true)
	case "enter", "e":
		if n == 0 {
			return wizContinue, e.openPalette(true)
		}
		return wizContinue, e.editStep(e.stepCur)
	case "d", "backspace":
		if n > 0 {
			i := e.stepCur
			e.ask = &exploreAsk{text: fmt.Sprintf("delete step %d %s? y/n", i+1, e.pipeline[i].Op), yes: func() (wizResult, tea.Cmd) {
				e.pipeline = slices.Delete(e.pipeline, i, i+1)
				e.stepCur = min(e.stepCur, max(len(e.pipeline)-1, 0))
				e.anchor = true
				return wizContinue, e.fetch()
			}}
		}
	case "J":
		if e.stepCur+1 < n {
			e.pipeline[e.stepCur], e.pipeline[e.stepCur+1] = e.pipeline[e.stepCur+1], e.pipeline[e.stepCur]
			e.stepCur++
			e.anchor = true
			return wizContinue, e.fetch()
		}
	case "K":
		if e.stepCur > 0 && e.stepCur < n {
			e.pipeline[e.stepCur], e.pipeline[e.stepCur-1] = e.pipeline[e.stepCur-1], e.pipeline[e.stepCur]
			e.stepCur--
			e.anchor = true
			return wizContinue, e.fetch()
		}
	}
	return wizContinue, nil
}

// editStep loads the preview before step idx, then opens its editor on those columns and values.
func (e *explorer) editStep(idx int) tea.Cmd {
	if idx < 0 || idx >= len(e.pipeline) {
		return nil
	}
	e.stepEdit = idx
	e.stepWait = idx
	e.prefixSeq = nextSeq() // its own sequence: a preview fetch in flight still lands
	seq, q := e.prefixSeq, e.runQueryFor(e.fetchSpec(e.pipeline[:idx]))
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		m := queryPreview(ctx, q, previewLimit, "", 0)
		return prefixMsg{idx: idx, seq: seq, Cols: m.Cols, Rows: m.Rows, SQL: m.SQL, Args: m.Args, Err: m.Err}
	}
}

func (e *explorer) applyPrefix(msg prefixMsg) tea.Cmd {
	if msg.idx != e.stepWait || msg.seq != e.prefixSeq {
		return nil // another edit since
	}
	e.stepWait = -1
	if msg.idx >= len(e.pipeline) || e.page != pagePreview {
		return nil
	}
	ctx := stepContext{cols: msg.Cols, rows: msg.Rows, dl: e.dl, base: msg.SQL, args: msg.Args}
	if msg.Err != nil {
		e.setNote("values: "+txt.Clean(txt.FirstLine(msg.Err.Error())), true)
		ctx = stepContext{dl: e.dl}
		if msg.idx < len(e.stepCols) {
			for _, c := range e.stepCols[msg.idx] {
				ctx.cols = append(ctx.cols, db.Column{Name: c})
			}
		}
	}
	st := e.pipeline[msg.idx]
	e.stepEdit = msg.idx
	return e.startStep(st.Op, &st, ctx)
}

// here is the context of a new step at the end: the preview as shown, the cursor column.
func (e *explorer) here() stepContext {
	ctx := stepContext{cols: e.cols, rows: e.rows, dl: e.dl, base: e.compiled, args: e.args}
	if e.colCur < len(e.cols) {
		ctx.cur = e.cols[e.colCur].Name
	}
	return ctx
}

// startStep opens the editor for op; steps without input apply at once.
func (e *explorer) startStep(op string, existing *steps.Step, ctx stepContext) tea.Cmd {
	if ctx.base != "" && e.h != nil {
		ctx.distinct = distinctValues(e.h, ctx.base, ctx.args)
	}
	e.stepEd = newStepEditor(op, ctx, existing)
	if e.stepEd.done() {
		return e.applyStep(e.stepEd.result)
	}
	e.page = pageStep
	return tea.Batch(e.stepEd.focus(), e.stepEd.loadValues())
}

// applyStep adds or replaces a step and re-runs the preview from its first row. Focus
// stays where it was.
func (e *explorer) applyStep(st steps.Step) tea.Cmd {
	if e.stepEdit >= 0 && e.stepEdit < len(e.pipeline) {
		e.pipeline[e.stepEdit] = st
	} else {
		e.pipeline = append(e.pipeline, st)
		e.stepCur = len(e.pipeline) - 1
	}
	e.stepEdit = -1
	e.page = pagePreview
	e.anchor = true
	e.setNote("", false)
	return e.fetch()
}

// openPalette lists the commands; t lists the steps only. Typing filters, enter runs.
func (e *explorer) openPalette(stepsOnly bool) tea.Cmd {
	e.stepEdit = -1
	title := "command"
	if stepsOnly {
		title = "step"
	}
	e.palette = newPicker(title, nil, nil)
	e.paletteCmd = map[string]string{}
	add := func(label, cmd, note string) {
		e.palette.items = append(e.palette.items, label)
		e.paletteCmd[label] = cmd
		if note != "" {
			e.palette.note[label] = note
		}
	}
	if e.steppable() {
		for _, op := range steps.Ops {
			add(op, "step:"+op, e.example(op))
		}
		add("detect types", "detect", "cast text columns")
	}
	if !stepsOnly {
		if e.steppable() {
			add("sql", "sql", "S")
		}
		add("more", "more", "m")
		add("refresh", "refresh", "R")
		add("wide", "wide", "w")
		add("value", "value", "v")
		add("schema", "schema", "i")
		if e.steppable() {
			add("copy sql", "copy", "y")
			add("full run", "bench", "b")
		}
		add("add to replication", "add", "+")
		if e.canSaveBack() {
			add("save to "+e.repName+" › "+e.streamName, "save", "ctrl+s")
		}
		switch {
		case e.bookmark != "":
			add("save bookmark "+e.bookmark, "bookmark", "ctrl+s")
		case e.canBookmark() && !e.canSaveBack():
			add("bookmark", "bookmark", "ctrl+s")
		}
		if e.table != "" && e.spec.SQL == "" && e.writable() {
			add("drop table "+e.table, "drop", "")
		}
	}
	e.palette.single = true
	e.palette.typing = true
	e.palette.filter.Width = 40
	e.page = pagePalette
	return e.palette.filter.Focus()
}

func (e *explorer) runCommand(cmd string) (wizResult, tea.Cmd) {
	switch {
	case strings.HasPrefix(cmd, "step:"):
		e.stepEdit = -1
		return wizContinue, e.startStep(strings.TrimPrefix(cmd, "step:"), nil, e.here())
	case cmd == "detect":
		return wizContinue, e.detectTypes()
	case cmd == "sql":
		return wizContinue, e.openSQL()
	case cmd == "more":
		return e.keyPreview(keyRune('m'))
	case cmd == "refresh":
		return wizContinue, e.fetch()
	case cmd == "wide":
		return e.keyPreview(keyRune('w'))
	case cmd == "value":
		return e.keyPreview(keyRune('v'))
	case cmd == "schema":
		return e.keyPreview(keyRune('i'))
	case cmd == "add":
		return wizContinue, e.toTargetPick()
	case cmd == "save":
		return wizContinue, e.saveBack()
	case cmd == "copy":
		return wizContinue, e.copySQL()
	case cmd == "bench":
		return wizContinue, e.bench()
	case cmd == "drop":
		if e.spec.SQL == "" {
			return wizContinue, e.askDrop(e.table)
		}
	case cmd == "bookmark":
		return wizContinue, e.toBookmarkForm()
	}
	return wizContinue, nil
}

func keyRune(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// example shows an op's arguments with the preview's own columns and values.
func (e *explorer) example(op string) string {
	var names []string
	first := map[db.Kind]int{}
	for i, c := range e.cols {
		if !argSafe(c.Name) {
			continue
		}
		names = append(names, c.Name)
		if _, ok := first[c.Kind]; !ok {
			first[c.Kind] = i
		}
	}
	if len(names) == 0 {
		return ""
	}
	pick := func(kinds ...db.Kind) string {
		for _, k := range kinds {
			if i, ok := first[k]; ok {
				return e.cols[i].Name
			}
		}
		return names[0]
	}
	num, str := pick(db.Int, db.Decimal, db.Float), pick(db.String)
	switch op {
	case "keep":
		return strings.Join(names[:min(2, len(names))], ", ")
	case "remove":
		return names[len(names)-1]
	case "rename":
		return names[0] + " = …"
	case "filter":
		i := slices.IndexFunc(e.cols, func(c db.Column) bool { return c.Name == num })
		for _, r := range e.rows {
			if i >= 0 && i < len(r) && r[i] != nil {
				if v, ok := valueText(r[i], e.cols[i].Kind); ok {
					return e.dl.Quote(num) + " = " + literal(e.dl, e.cols[i].Kind, v)
				}
			}
		}
		return e.dl.Quote(num) + " IS NOT NULL"
	case "sort":
		return "-" + names[0]
	case "limit":
		return "100"
	case "trim", "upper", "lower":
		if _, ok := first[db.String]; !ok {
			return ""
		}
		return str
	case "cast":
		return num + " as text"
	case "set":
		return num + " = " + e.dl.Quote(num) + " * 2"
	case "coalesce":
		return str + " = ''"
	}
	return ""
}

var (
	intRe  = regexp.MustCompile(`^-?(0|[1-9]\d{0,17})$`)
	dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	tsRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2})?$`)
)

// detectType proposes a cast type for text values: int, float, date or timestamp.
func detectType(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	all := func(ok func(string) bool) bool {
		for _, v := range vals {
			if !ok(v) {
				return false
			}
		}
		return true
	}
	switch {
	case all(intRe.MatchString):
		return "int"
	case all(func(v string) bool { return numberRe.MatchString(v) && !leadingZero(v) }):
		return "float"
	case all(func(v string) bool { _, err := time.Parse("2006-01-02", v); return dateRe.MatchString(v) && err == nil }):
		return "date"
	case all(tsRe.MatchString):
		return "timestamp"
	}
	return ""
}

// leadingZero: 007 is a code, not a number.
func leadingZero(v string) bool {
	v = strings.TrimPrefix(v, "-")
	return len(v) > 1 && v[0] == '0' && v[1] != '.'
}

// detectTypes proposes one cast step for the text columns whose sample values all
// read as one type, and opens it for review.
func (e *explorer) detectTypes() tea.Cmd {
	var parts []string
	for i, c := range e.cols {
		if c.Kind != db.String || !argSafe(c.Name) {
			continue
		}
		var vals []string
		for _, r := range e.rows {
			if i < len(r) && r[i] != nil {
				if s, ok := r[i].(string); ok {
					vals = append(vals, s)
				}
			}
		}
		if t := detectType(vals); t != "" {
			parts = append(parts, c.Name+" as "+t)
		}
	}
	if len(parts) == 0 {
		e.setNote("detect types: none", false)
		return nil
	}
	e.stepEdit = -1
	e.stepEd = newStepEditor("cast", e.here(), &steps.Step{Op: "cast", Args: strings.Join(parts, ", ")})
	e.page = pageStep
	return e.stepEd.second()
}

// finalSteps is the pipeline plus a keep for the ticks when steps or sql decide the columns.
func (e *explorer) finalSteps(ticked []string) ([]steps.Step, error) {
	p := slices.Clone(e.pipeline)
	if len(ticked) == len(e.cols) || len(e.cols) == 0 {
		return p, nil
	}
	for _, c := range ticked {
		if !argSafe(c) {
			return nil, fmt.Errorf("column %q: untick in a keep step", txt.Clean(c))
		}
	}
	return append(p, steps.Step{Op: "keep", Args: strings.Join(ticked, ", ")}), nil
}

// ticksAsKeep: with steps or sql, the ticks save as a trailing keep step.
func (e *explorer) ticksAsKeep() bool {
	return !e.isAPI && (len(e.pipeline) > 0 || e.spec.SQL != "" || len(e.spec.Select) > 0)
}

// stepsPanel lists the steps and the query a run sends, scrolled with the panel offset.
func (e *explorer) stepsPanel(width, height int) []string {
	title := " " + sHeader.Render("steps")
	if e.focus == focusSteps {
		title = " " + sKey.Render("steps")
	}
	lines := []string{title}
	if len(e.pipeline) == 0 {
		lines = append(lines, " "+sDim.Render("none  t: add"))
	}
	w := max(width-1, 10)
	curAt := -1
	for i, st := range e.pipeline {
		text := fmt.Sprintf(" %d  %s", i+1, txt.Clean(st.String()))
		wrapped := strings.Split(ansi.Hardwrap(text, w, false), "\n")
		for j, l := range wrapped {
			if j > 0 {
				l = "    " + l
			}
			if e.focus == focusSteps && i == e.stepCur {
				if j == 0 {
					curAt = len(lines)
				}
				l = sSelected.Width(w).Render(l)
			}
			lines = append(lines, l)
		}
	}
	if t := e.tickedCols(); e.ticksAsKeep() && len(t) < len(e.cols) {
		for _, l := range strings.Split(ansi.Hardwrap(" ·  keep "+txt.Clean(strings.Join(t, ", "))+"  ticks", w, false), "\n") {
			lines = append(lines, sDim.Render(l))
		}
	}
	lines = append(lines, "", " "+sHeader.Render("sql"))
	for _, l := range wrapSQL(txt.CleanText(e.shownSQL()), width-2) {
		lines = append(lines, " "+sDim.Render(l))
	}
	if len(lines) <= height {
		e.panelOff = 0
		return lines
	}
	rows := max(height-1, 1)
	if curAt >= 0 && e.panelCur != e.stepCur {
		// the step cursor moved: bring it into view
		e.panelCur = e.stepCur
		if curAt < e.panelOff {
			e.panelOff = curAt
		}
		if curAt >= e.panelOff+rows {
			e.panelOff = curAt - rows + 1
		}
	}
	e.panelOff = min(max(e.panelOff, 0), len(lines)-rows)
	out := slices.Clone(lines[e.panelOff : e.panelOff+rows])
	var more []string
	if e.panelOff > 0 {
		more = append(more, fmt.Sprintf("↑ %d", e.panelOff))
	}
	if below := len(lines) - e.panelOff - rows; below > 0 {
		more = append(more, fmt.Sprintf("↓ %d", below))
	}
	return append(out, " "+sDim.Render(strings.Join(more, "  ")+" more"))
}

// wrapSQL breaks a one-line query at keywords, then hard-wraps to width.
func wrapSQL(sql string, width int) []string {
	width = max(width, 10)
	for _, kw := range []string{" FROM ", " WHERE ", " ORDER BY ", " LIMIT "} {
		sql = strings.ReplaceAll(sql, kw, "\n"+strings.TrimLeft(kw, " "))
	}
	var out []string
	for _, line := range strings.Split(sql, "\n") {
		out = append(out, strings.Split(ansi.Hardwrap(line, width, false), "\n")...)
	}
	return out
}
