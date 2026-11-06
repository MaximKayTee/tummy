package tui

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"tummy/internal/txt"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/engine"
	"tummy/internal/steps"
)

type previewMsg struct {
	Cols     []db.Column
	Rows     [][]any
	SQL      string     // query sent, database sources
	Args     []any      // its arguments
	Steps    [][]string // columns each step sees
	Total    int        // rows the query returns in all; -1 unknown
	Counted  string     // query Total was counted for
	Complete bool       // no rows beyond these
	Parents  string     // for_each: "3 of 10 users"
	Keys     []apiKey   // api: top-level keys of the response
	WM       *watermarkInfo
	PK       []string
	PKFor    string
	Note     string
	Err      error
	Took     time.Duration
	seq      int
}

// shownMsg is the query a run sends, for the sql panel.
type shownMsg struct {
	seq int
	SQL string
}

// ---------- fetching ----------

// fetchSpec is the stream as previewed with a pipeline in place of its steps.
func (e *explorer) fetchSpec(pipeline []steps.Step) config.Stream {
	s := e.spec
	s.Steps = renderSteps(pipeline)
	if e.stepsErr != "" && len(e.pipeline) == 0 {
		s.Steps = e.spec.Steps // show the parse error, not unfiltered rows
	}
	if s.Mode == "backfill" && s.Range == "" {
		s.Mode = "" // no range yet: every row
	}
	return s
}

// runName is the stream name a run would use: the table, or the stream of a query.
func (e *explorer) runName() string {
	if e.table != "" && e.spec.SQL == "" {
		return e.table
	}
	return cmpOr(e.streamName, "query")
}

// needsWatermark: an incremental stream of a replication reads its target's high mark.
func (e *explorer) needsWatermark() bool {
	return !e.isAPI && e.repName != "" && e.streamName != "" &&
		e.spec.Mode == "incremental" && e.spec.UpdateKey != ""
}

// runQuery is a stream's query as a run sends it, with the preview's spec.
type runQuery struct {
	h     *dbHandle
	spec  config.Stream
	name  string
	wm    any
	steps bool // also compile the columns each step sees
}

func (e *explorer) runQueryFor(spec config.Stream) runQuery {
	q := runQuery{h: e.h, spec: spec, name: e.runName(), steps: spec.Steps != ""}
	if e.wm != nil {
		q.wm = e.wm.val
	}
	return q
}

func (q runQuery) compile(ctx context.Context, d *db.DB) (string, []any, [][]string, error) {
	var cols [][]string
	if q.steps {
		c, err := engine.CompileStream(ctx, d, q.spec, q.name)
		if err != nil {
			return c.SQL, nil, nil, err
		}
		cols = c.Columns
	}
	sql, args, err := engine.RunQuery(ctx, d, config.ResolvedStream{Name: q.name, Stream: q.spec}, q.wm)
	return sql, args, cols, err
}

func (e *explorer) fetch() tea.Cmd {
	e.fetching = true
	e.fetchErr = ""
	e.benchText = ""
	e.fetchSeq = nextSeq()
	if e.limit == 0 {
		e.limit = 1
	}
	seq, n := e.fetchSeq, e.limit
	if e.isAPI {
		run := e.apiFetch(e.spec, n)
		return func() tea.Msg {
			msg := run()
			msg.seq = seq
			return msg
		}
	}
	q := e.runQueryFor(e.fetchSpec(e.pipeline))
	counted, total := e.counted, e.total
	var readWM func(context.Context) *watermarkInfo
	if e.wm == nil && e.needsWatermark() {
		opts, conns, rep, rs := e.opts, e.conns, e.repName, config.ResolvedStream{Name: e.streamName, Stream: e.spec}
		readWM = func(ctx context.Context) *watermarkInfo { return readWatermark(ctx, opts, conns, rep, rs) }
	}
	pkTable := ""
	if e.table != "" && e.spec.SQL == "" && e.pkFor != e.table {
		pkTable = e.table
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var wm *watermarkInfo
		if readWM != nil {
			wm = readWM(ctx)
			q.wm = wm.val
		}
		msg := queryPreview(ctx, q, previewLimit*n, counted, total)
		msg.WM, msg.seq = wm, seq
		if pkTable != "" && msg.Err == nil {
			if d, err := q.h.get(ctx); err == nil {
				pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
				msg.PK, _ = d.PrimaryKey(pctx, db.ParseTable(pkTable))
				pcancel()
			}
			msg.PKFor = pkTable
		}
		return msg
	}
}

// readWatermark reads MAX(update_key) of the stream's target, as a run starts from.
func readWatermark(ctx context.Context, opts Options, conns *config.Connections, rep string, s config.ResolvedStream) *watermarkInfo {
	fail := func(err error) *watermarkInfo { return &watermarkInfo{err: txt.FirstLine(err.Error())} }
	path, err := config.ReplicationPath(opts.Home, rep)
	if err != nil {
		return fail(err)
	}
	r, err := config.LoadReplication(path)
	if err != nil {
		return fail(err)
	}
	c, err := conns.Get(r.Target)
	if err != nil {
		return fail(err)
	}
	if c.Type == "api" {
		return &watermarkInfo{}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tgt, err := db.Open(ctx, c.DB())
	if err != nil {
		return fail(err)
	}
	defer tgt.Close()
	wm, err := engine.Watermark(ctx, tgt, s)
	if err != nil {
		return fail(err)
	}
	return &watermarkInfo{val: wm}
}

// queryPreview reads the first n rows of the run query and counts them all when that
// is quick. counted and total are the last count, kept while the query is the same.
func queryPreview(ctx context.Context, q runQuery, n int, counted string, total int) previewMsg {
	start := time.Now()
	d, err := q.h.get(ctx)
	if err != nil {
		return previewMsg{Err: err, Total: -1}
	}
	sql, args, cols, err := q.compile(ctx, d)
	msg := previewMsg{SQL: sql, Args: args, Steps: cols, Total: -1}
	if err != nil {
		msg.Err = err
		return msg
	}
	rd, err := d.Query(ctx, d.Dialect.OrderLimit(sql, "", n), args...)
	if err != nil {
		msg.Err = err
		return msg
	}
	defer rd.Close()
	for {
		row, err := rd.Next()
		if err != nil {
			msg.Err = err
			return msg
		}
		if row == nil {
			break
		}
		msg.Rows = append(msg.Rows, row)
	}
	msg.Cols, msg.Took = rd.Columns, time.Since(start)
	key := sql + "\x00" + fmt.Sprint(args...)
	switch {
	case len(msg.Rows) < n:
		msg.Total = len(msg.Rows)
	case key == counted && total >= 0:
		msg.Total = total
	default:
		msg.Total = countRows(ctx, d, sql, args)
	}
	msg.Counted = key
	msg.Complete = msg.Total >= 0 && len(msg.Rows) >= msg.Total
	return msg
}

// countRows is COUNT(*) of a query, or -1 when it takes more than a few seconds.
func countRows(ctx context.Context, d *db.DB, sql string, args []any) int {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	n, err := d.Count(ctx, sql, args...)
	if err != nil {
		return -1
	}
	return int(n)
}

// shownSQL is the query on the sql panel: the one a run sends, arguments inlined.
func (e *explorer) shownSQL() string {
	if e.shown != "" {
		return e.shown
	}
	return inlineArgs(e.compiled, e.args, e.dl)
}

// showCmd compiles the query a save would make the run send: ticks as select or a
// trailing keep, the watermark of an incremental stream.
func (e *explorer) showCmd() tea.Cmd {
	if e.isAPI || len(e.cols) == 0 || e.dl == nil {
		return nil
	}
	e.shownSeq = nextSeq()
	seq := e.shownSeq
	spec := e.fetchSpec(e.pipeline)
	if err := e.applyEdits(&spec, e.tickedCols(), false); err != nil {
		e.shown = ""
		return nil
	}
	q := e.runQueryFor(spec)
	dl := e.dl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		d, err := q.h.get(ctx)
		if err != nil {
			return shownMsg{seq: seq}
		}
		q.steps = false
		sql, args, _, err := q.compile(ctx, d)
		if err != nil {
			return shownMsg{seq: seq}
		}
		return shownMsg{seq: seq, SQL: inlineArgs(sql, args, dl)}
	}
}

// inlineArgs puts the arguments of a query in place of its placeholders, for reading.
func inlineArgs(sql string, args []any, dl db.Dialect) string {
	if len(args) == 0 || dl == nil {
		return sql
	}
	var b strings.Builder
	next := 0
	var quote byte
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '?' && dl.Placeholder(1) == "?":
			if next < len(args) {
				b.WriteString(argLiteral(dl, args[next]))
				next++
				continue
			}
		case c == '$' || c == '@':
			j := i + 1
			if c == '@' && j < len(sql) && sql[j] == 'p' {
				j++
			}
			k := j
			for k < len(sql) && sql[k] >= '0' && sql[k] <= '9' {
				k++
			}
			if k > j {
				if n := atoi(sql[j:k]); n >= 1 && n <= len(args) && sql[i:k] == dl.Placeholder(n) {
					b.WriteString(argLiteral(dl, args[n-1]))
					i = k - 1
					continue
				}
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// argLiteral is an argument as SQL text.
func argLiteral(dl db.Dialect, v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case time.Time:
		if x.Nanosecond() == 0 {
			return sqlString(dl, x.Format("2006-01-02 15:04:05"))
		}
		return sqlString(dl, x.Format("2006-01-02 15:04:05.999999"))
	case string:
		return sqlString(dl, x)
	case []byte:
		return sqlString(dl, string(x))
	case bool:
		return boolSQL(dl, x)
	}
	return fmt.Sprint(v)
}

// boundText is the update-key condition of the run for the header.
func (e *explorer) boundText() string {
	switch {
	case e.wm == nil || !e.needsWatermark():
		return ""
	case e.wm.err != "":
		return "watermark: " + e.wm.err
	case e.wm.val == nil:
		return "no watermark: every row"
	}
	op := ">"
	if len(e.spec.PrimaryKey) > 0 {
		op = ">="
	}
	v := strings.Trim(argLiteral(e.dl, e.wm.val), "'")
	return e.spec.UpdateKey + " " + op + " " + v
}

// applyPreview takes a fetch result. The first result of a table sets the ticks and
// cursor; later ones keep them, clamped to the new columns and rows.
func (e *explorer) applyPreview(msg previewMsg) tea.Cmd {
	if msg.seq != e.fetchSeq {
		return nil // superseded
	}
	e.fetching = false
	e.took = msg.Took
	e.stepCols = msg.Steps
	if msg.WM != nil {
		e.wm = msg.WM
	}
	if msg.PKFor != "" {
		e.srcPK, e.pkFor = msg.PK, msg.PKFor
	}
	if len(msg.Keys) > 0 {
		e.setRespKeys(msg.Keys)
	}
	if msg.Note != "" {
		e.setNote(msg.Note, false)
	}
	e.compiled, e.args = msg.SQL, msg.Args
	if msg.Err != nil {
		e.fetchErr = strings.TrimSpace(msg.Err.Error())
		e.shown = ""
		return nil
	}
	curName := ""
	if e.colCur < len(e.cols) {
		curName = e.cols[e.colCur].Name
	}
	e.cols, e.rows, e.fetchErr = msg.Cols, msg.Rows, ""
	e.total, e.counted, e.complete, e.parents = msg.Total, msg.Counted, msg.Complete, msg.Parents
	if e.page == pageEndpoint {
		e.page = pagePreview // errors stay on the endpoint form
	}
	e.measure()
	if e.fresh {
		e.fresh, e.anchor = false, false
		want := map[string]bool{}
		if len(e.wantSelect) > 0 {
			for _, c := range (config.Stream{Select: e.wantSelect}).Columns(e.colNames()) {
				want[strings.ToLower(c)] = true
			}
		}
		e.ticked = map[string]bool{}
		for _, c := range e.cols {
			e.ticked[c.Name] = len(want) == 0 || want[strings.ToLower(c.Name)]
		}
		e.tickBase = strings.Join(e.tickedCols(), "\x00")
		e.colCur, e.colOff, e.rowOff, e.rowCur = 0, 0, 0, 0
		e.showVal, e.showCols = false, false
		return e.showCmd()
	}
	for _, c := range e.cols {
		if _, ok := e.ticked[c.Name]; !ok {
			e.ticked[c.Name] = true // new or renamed by a step
		}
	}
	if e.anchor {
		// a step changed the rows: back to the first row, on the same column if it is still there
		e.anchor = false
		e.rowCur, e.rowOff, e.colOff = 0, 0, 0
		e.colCur = max(slices.IndexFunc(e.cols, func(c db.Column) bool { return c.Name == curName }), 0)
	}
	e.rowCur = min(e.rowCur, max(len(e.rows)-1, 0))
	e.rowOff = min(e.rowOff, e.rowCur)
	e.colCur = min(e.colCur, max(len(e.cols)-1, 0))
	e.colOff = min(e.colOff, e.colCur)
	if len(e.rows) == 0 {
		e.showVal = false
	}
	if len(e.cols) == 0 {
		e.showCols = false
	}
	return e.showCmd()
}

// noteFor is how long a note stays; errors stay until the next action.
const noteFor = 5 * time.Second

func (e *explorer) setNote(s string, isErr bool) { e.note, e.noteErr, e.noteAt = s, isErr, time.Now() }

// currentNote is the note while it is fresh; an error note does not fade.
func (e *explorer) currentNote() string {
	if e.note == "" || (!e.noteErr && time.Since(e.noteAt) > noteFor) {
		return ""
	}
	return e.note
}

// ---------- preview keys ----------

func (e *explorer) steppable() bool { return !e.isAPI }

// panelBeside: wide enough for the steps panel next to the grid; narrower, tab shows it
// in place of the grid. Before the first frame the width is unknown.
func (e *explorer) panelBeside() bool {
	return e.width == 0 || e.width >= panelMinWidth
}

func (e *explorer) keyPreview(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	if e.editSQL {
		return e.keySQL(msg)
	}
	if e.showVal || e.showCols {
		e.showVal, e.showCols = false, false
		return wizContinue, nil
	}
	if e.focus == focusSteps && !e.steppable() {
		e.focus = focusGrid
	}
	switch msg.String() {
	case "tab":
		switch {
		case e.focus == focusSteps:
			e.focus = focusGrid
		case e.steppable():
			e.focus = focusSteps
		}
		return wizContinue, nil
	case "t":
		if e.steppable() {
			return wizContinue, e.openPalette(true)
		}
		return wizContinue, nil
	case ":", "ctrl+p":
		return wizContinue, e.openPalette(false)
	case "ctrl+s":
		if e.canSaveBack() {
			return wizContinue, e.saveBack()
		}
		return wizContinue, e.saveBookmark()
	case "y":
		if e.steppable() {
			return wizContinue, e.copySQL()
		}
	case "b":
		return wizContinue, e.bench()
	case "i":
		e.showCols = len(e.cols) > 0
		return wizContinue, nil
	}
	if e.focus == focusSteps {
		return e.keySteps(msg)
	}
	switch msg.String() {
	case "esc", "q":
		return e.leave()
	case "left", "h":
		e.colCur = max(e.colCur-1, 0)
	case "right", "l":
		e.colCur = min(e.colCur+1, max(len(e.cols)-1, 0))
	case "up", "k":
		e.rowCur = max(e.rowCur-1, 0)
	case "down", "j":
		e.rowCur = min(e.rowCur+1, max(len(e.rows)-1, 0))
	case "pgup":
		e.rowCur = max(e.rowCur-10, 0)
	case "pgdown":
		e.rowCur = min(e.rowCur+10, max(len(e.rows)-1, 0))
	case "g", "home":
		e.rowCur = 0
	case "G", "end":
		e.rowCur = max(len(e.rows)-1, 0)
	case ">":
		if e.colCur < len(e.cols) {
			e.widths[e.cols[e.colCur].Name] = e.colWidth(e.colCur) + 8
		}
	case "<":
		if e.colCur < len(e.cols) {
			e.widths[e.cols[e.colCur].Name] = max(e.colWidth(e.colCur)-8, 4)
		}
	case "=":
		if e.colCur < len(e.cols) {
			delete(e.widths, e.cols[e.colCur].Name)
		}
	case "+":
		return wizContinue, e.toTargetPick()
	case "w":
		e.wide = !e.wide
		e.widths = map[string]int{}
	case "v", "enter":
		if len(e.rows) > 0 && len(e.cols) > 0 {
			e.showVal = true
		}
	case " ", "x":
		if e.colCur < len(e.cols) {
			n := e.cols[e.colCur].Name
			e.ticked[n] = !e.ticked[n]
			return wizContinue, e.showCmd()
		}
	case "a", "n":
		for _, c := range e.cols {
			e.ticked[c.Name] = msg.String() == "a"
		}
		return wizContinue, e.showCmd()
	case "S":
		return wizContinue, e.openSQL()
	case "R":
		e.wm, e.counted = nil, ""
		return wizContinue, e.fetch()
	case "m":
		switch {
		case e.fetching:
		case e.complete:
			e.setNote("all rows loaded", false)
		default:
			e.limit++
			return wizContinue, e.fetch()
		}
	}
	return wizContinue, nil
}

// leave goes back, asking first when steps, ticks or sql would be lost.
func (e *explorer) leave() (wizResult, tea.Cmd) {
	if e.dirty() {
		e.ask = &exploreAsk{text: "discard changes? y/n", yes: func() (wizResult, tea.Cmd) {
			e.discarded = true // asked once: the wizard wrapper must not ask again
			return e.leaveNow()
		}}
		return wizContinue, nil
	}
	return e.leaveNow()
}

func (e *explorer) leaveNow() (wizResult, tea.Cmd) {
	switch {
	case e.fromStream:
		return wizCancel, nil
	case e.isAPI:
		e.page = pageEndpoint
		return wizContinue, e.endpoint.focus()
	}
	e.resetPreview()
	e.page = pageTables
	return wizContinue, nil
}

// dirty: a stream or bookmark preview with unsaved steps, ticks or sql; a table preview
// with steps.
func (e *explorer) dirty() bool {
	if e.discarded {
		return false
	}
	if renderSteps(e.pipeline) != e.baseSteps {
		return true
	}
	if e.streamName == "" && e.bookmark == "" {
		return false
	}
	return e.spec.SQL != e.baseSQL || (e.tickBase != "" && strings.Join(e.tickedCols(), "\x00") != e.tickBase)
}

func (e *explorer) openSQL() tea.Cmd {
	if e.isAPI {
		return nil
	}
	e.editSQL = true
	v := e.spec.SQL
	if v == "" && e.table != "" {
		v = "select * from " + db.QuoteTable(e.dl, db.ParseTable(e.table))
	}
	e.sqlInput.SetValue(v)
	return e.sqlInput.Focus()
}

func (e *explorer) keySQL(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	switch msg.String() {
	case "esc":
		e.editSQL = false
		e.sqlInput.Blur()
		if e.cols == nil && e.table == "" && e.spec.SQL == "" && !e.fromStream {
			e.page = pageTables
		}
		return wizContinue, nil
	case "ctrl+e":
		return wizContinue, e.execSQL()
	case "ctrl+s":
		sql := strings.TrimSpace(e.sqlInput.Value())
		if sql == "" {
			return wizContinue, nil
		}
		e.editSQL = false
		e.sqlInput.Blur()
		if e.table != "" && e.spec.SQL == "" {
			e.wantSelect, e.spec.Select = nil, nil // the query picks its columns
		}
		e.table = ""
		e.spec.SQL = sql
		e.limit, e.anchor = 1, true
		return wizContinue, e.fetch()
	}
	var cmd tea.Cmd
	e.sqlInput, cmd = e.sqlInput.Update(msg)
	return wizContinue, cmd
}

// ---------- widths ----------

// measure caches each column's natural width from the first rows of a fetch.
func (e *explorer) measure() {
	e.natural = make([]int, len(e.cols))
	for i, c := range e.cols {
		w := max(ansi.StringWidth(txt.Clean(c.Name))+2, len(c.Kind.String())+2)
		for r := 0; r < min(len(e.rows), 50); r++ {
			if i < len(e.rows[r]) {
				w = max(w, ansi.StringWidth(cell(e.rows[r][i], c.Kind)))
			}
		}
		e.natural[i] = w
	}
}

// colWidth is a column's width: an override, else its natural width, capped unless wide.
func (e *explorer) colWidth(i int) int {
	if i >= len(e.cols) {
		return 4
	}
	if w, ok := e.widths[e.cols[i].Name]; ok {
		return w
	}
	w := 4
	if i < len(e.natural) {
		w = max(e.natural[i], 4)
	}
	if !e.wide {
		w = min(w, 28)
	}
	return w
}

func (e *explorer) tickedCols() []string {
	var out []string
	for _, c := range e.cols {
		if e.ticked[c.Name] {
			out = append(out, c.Name)
		}
	}
	return out
}

func (e *explorer) colNames() []string {
	out := make([]string, len(e.cols))
	for i, c := range e.cols {
		out[i] = c.Name
	}
	return out
}

// ---------- views ----------

func (e *explorer) previewView(width, height int) []string {
	out := []string{e.previewHead(width)}
	if e.editSQL {
		e.sqlInput.SetWidth(max(width-4, 20))
		out = append(out, strings.Split(e.sqlInput.View(), "\n")...)
	} else if e.spec.SQL != "" {
		out = append(out, " "+sDim.Render(ansi.Truncate("sql› "+txt.Clean(txt.FirstLine(e.spec.SQL)), width-2, "…")))
	}
	if note := e.currentNote(); note != "" {
		style := sDim
		if e.noteErr {
			style = sErr
		}
		lines := strings.Split(ansi.Wrap(txt.CleanText(note), max(width-2, 10), " "), "\n")
		for _, l := range lines[:min(len(lines), 3)] {
			out = append(out, " "+style.Render(l))
		}
	}
	out = append(out, "")
	rest := height - len(out)
	var body []string
	switch {
	case e.fetching && len(e.cols) == 0:
		body = []string{"  " + sWarn.Render("◐ fetching")}
	case e.fetchErr != "":
		body = errBox(e.fetchErr, min(width, 100), max(rest/2, 4))
	}
	switch {
	case !e.steppable():
		if body != nil || len(e.cols) == 0 {
			return append(out, body...)
		}
		return append(out, e.grid(width, rest)...)
	case !e.panelBeside():
		// narrow: tab shows the steps panel in place of the grid
		if e.focus == focusSteps {
			return append(out, e.stepsPanel(width, rest)...)
		}
		if body != nil || len(e.cols) == 0 {
			return append(out, body...)
		}
		return append(out, e.grid(width, rest)...)
	}
	panelW := max(min(width*2/5, 60), 34)
	gridW := width - panelW - 1
	var grid []string
	switch {
	case e.fetchErr != "":
		grid = errBox(e.fetchErr, gridW, max(rest/2, 4))
	case body != nil:
		grid = body
	case len(e.cols) > 0:
		grid = e.grid(gridW, rest)
	}
	panel := e.stepsPanel(panelW, rest)
	for len(grid) < len(panel) {
		grid = append(grid, "")
	}
	for i := range grid {
		line := pad(grid[i], gridW, false) + sRule.Render("│")
		if i < len(panel) {
			line += panel[i]
		}
		out = append(out, line)
	}
	return out
}

// previewHead is the title line: what is previewed, marks, rows of how many, columns.
func (e *explorer) previewHead(width int) string {
	what := e.table
	switch {
	case e.isAPI:
		what = e.spec.Path
	case e.spec.SQL != "" || e.editSQL:
		what = cmpOr(e.streamName, "query")
	}
	parts := []string{sTitle.Render(txt.Clean(what))}
	if e.sample {
		parts = append(parts, sWarn.Render("source sample"))
	}
	if e.fromStream {
		parts = append(parts, sDim.Render(e.conn.Name))
	}
	if e.bookmark != "" {
		parts = append(parts, sDim.Render("bookmark "+txt.Clean(e.bookmark)))
	}
	switch {
	case e.fetching && len(e.cols) > 0:
		parts = append(parts, sWarn.Render("◐ more"))
	case e.stepWait >= 0:
		parts = append(parts, sWarn.Render(fmt.Sprintf("◐ step %d", e.stepWait+1)))
	case e.busy != "":
		parts = append(parts, sWarn.Render("◐ "+txt.Clean(e.busy)))
	case e.benchText != "":
		parts = append(parts, sDim.Render(e.benchText))
	}
	if e.dirty() {
		parts = append(parts, sWarn.Render("●"))
	}
	if len(e.cols) > 0 {
		parts = append(parts, sDim.Render(e.rowsText()))
		if e.parents != "" {
			parts = append(parts, sDim.Render(e.parents))
		}
		parts = append(parts, sDim.Render(fmt.Sprintf("%d/%d columns", len(e.tickedCols()), len(e.cols))))
	}
	if b := e.boundText(); b != "" {
		parts = append(parts, sDim.Render(txt.Clean(b)))
	}
	head := " " + strings.Join(parts, "  ")
	if t := sDim.Render(txt.Dur(e.took)); len(e.cols) > 0 && lipgloss.Width(head)+2+lipgloss.Width(t) <= width {
		head += "  " + t
	}
	return head
}

// rowsText says how many rows are shown of how many the query returns.
func (e *explorer) rowsText() string {
	n := len(e.rows)
	rows := func(n int) string {
		if n == 1 {
			return "1 row"
		}
		return txt.Thousands(int64(n)) + " rows"
	}
	switch {
	case e.total >= 0 && e.total > n:
		return txt.Thousands(int64(n)) + " of " + rows(e.total)
	case e.total >= 0 || e.complete:
		return rows(n)
	}
	return "first " + rows(n)
}

// grid renders the preview with a column cursor and horizontal scrolling.
func (e *explorer) grid(width, height int) []string {
	if e.showVal && e.rowCur < len(e.rows) && e.colCur < len(e.cols) {
		return e.valueBox(width, height)
	}
	if e.showCols && len(e.cols) > 0 {
		return e.schemaBox(width, height)
	}
	widths := make([]int, len(e.cols))
	for i := range e.cols {
		widths[i] = min(e.colWidth(i), max(width-4, 8))
	}
	// keep the cursor row visible
	rowsShown := max(height-2, 1)
	if e.rowCur < e.rowOff {
		e.rowOff = e.rowCur
	}
	if e.rowCur >= e.rowOff+rowsShown {
		e.rowOff = e.rowCur - rowsShown + 1
	}
	// keep the cursor column visible
	e.colCur = min(e.colCur, max(len(e.cols)-1, 0))
	e.colOff = min(e.colOff, e.colCur)
	for {
		used := 2
		for i := e.colOff; i <= e.colCur && i < len(e.cols); i++ {
			used += widths[i] + 2
		}
		if used <= width || e.colOff >= e.colCur {
			break
		}
		e.colOff++
	}
	last := e.colOff
	used := 2
	for last < len(e.cols) && used+widths[last]+2 <= width {
		used += widths[last] + 2
		last++
	}
	if last == e.colOff {
		last = min(e.colOff+1, len(e.cols))
	}

	render := func(i int, s string, header bool) string {
		s = pad(s, widths[i], false)
		switch {
		case header && i == e.colCur:
			return sSelected.Render(s)
		case header && e.ticked[e.cols[i].Name]:
			return sHeader.Render(s)
		case header:
			return sDim.Render(s)
		case i == e.colCur:
			return sText.Render(s)
		case !e.ticked[e.cols[i].Name]:
			return sDim.Render(s)
		}
		return s
	}
	var hdr, typ []string
	for i := e.colOff; i < last; i++ {
		mark := "○ "
		if e.ticked[e.cols[i].Name] {
			mark = "● "
		}
		hdr = append(hdr, render(i, mark+txt.Clean(e.cols[i].Name), true))
		typ = append(typ, sDim.Render(pad("  "+e.cols[i].Kind.String(), widths[i], false)))
	}
	more := ""
	if last < len(e.cols) {
		more = sDim.Render(fmt.Sprintf(" … %d more →", len(e.cols)-last))
	}
	if e.colOff > 0 {
		more = sDim.Render(" ←") + more
	}
	out := []string{"  " + strings.Join(hdr, "  ") + more, "  " + strings.Join(typ, "  ")}
	end := min(e.rowOff+rowsShown, len(e.rows))
	for r := e.rowOff; r < end; r++ {
		var cells []string
		for i := e.colOff; i < last; i++ {
			var v any
			if i < len(e.rows[r]) {
				v = e.rows[r][i]
			}
			s := cell(v, e.cols[i].Kind)
			if r == e.rowCur && i == e.colCur {
				cells = append(cells, sSelected.Render(pad(s, widths[i], false)))
			} else {
				cells = append(cells, render(i, s, false))
			}
		}
		mark := "  "
		if r == e.rowCur {
			mark = sKey.Render("›") + " "
		}
		out = append(out, mark+strings.Join(cells, "  "))
	}
	if len(e.rows) == 0 {
		out = append(out, "  "+sDim.Render("none"))
	}
	return out
}

// valueBox shows the full value of the current cell, wrapped to the width.
// schemaBox lists every column with its kind, the driver's type and its first non-null
// value in the loaded rows.
func (e *explorer) schemaBox(width, height int) []string {
	cols := []column{{Title: "COLUMN"}, {Title: "KIND", Width: 10}, {Title: "TYPE", Width: 16}, {Title: "FIRST VALUE"}}
	rows := make([][]string, len(e.cols))
	for i, c := range e.cols {
		first := "∅"
		for _, r := range e.rows {
			if i < len(r) && r[i] != nil {
				first = cell(r[i], c.Kind)
				break
			}
		}
		if len(e.rows) == 0 {
			first = "—"
		}
		rows[i] = []string{txt.Clean(c.Name), c.Kind.String(), sDim.Render(txt.Clean(strings.ToLower(c.Type))), first}
	}
	out := []string{"  " + sHeader.Render("schema") + sDim.Render(fmt.Sprintf("  %d columns  first values from %d rows", len(e.cols), len(e.rows))), ""}
	tbl := table(width-2, cols, rows, -1)
	for _, l := range tbl {
		out = append(out, "  "+l)
	}
	if len(out) > height {
		out = append(out[:max(height-1, 0)], "  "+sDim.Render("…"))
	}
	return out
}

func (e *explorer) valueBox(width, height int) []string {
	c := e.cols[e.colCur]
	var raw any
	if e.colCur < len(e.rows[e.rowCur]) {
		raw = e.rows[e.rowCur][e.colCur]
	}
	text := cellFull(raw, c.Kind)
	out := []string{"  " + sHeader.Render(txt.Clean(c.Name)) + sDim.Render(fmt.Sprintf("  %s  row %d of %d  %d chars", c.Kind, e.rowCur+1, len(e.rows), utf8.RuneCountInString(text))), ""}
	w := max(width-4, 10)
	// no more text than fits
	if limit := w * max(height, 1); utf8.RuneCountInString(text) > limit {
		text = string([]rune(text)[:limit])
	}
	for _, para := range strings.Split(text, "\n") {
		for _, l := range strings.Split(ansi.Hardwrap(para, w, true), "\n") {
			out = append(out, "  "+l)
		}
		if len(out) > height {
			break
		}
	}
	if len(out) > height {
		out = append(out[:max(height-1, 0)], "  "+sDim.Render("…"))
	}
	return out
}

// ---------- values ----------

const cellMax = 256 // runes of a value the grid looks at

// cell is a value for the grid: one line, control characters escaped, cut at cellMax.
func cell(v any, k db.Kind) string {
	switch x := v.(type) {
	case nil:
		return "∅"
	case time.Time:
		if k == db.Date {
			return x.Format("2006-01-02")
		}
		return x.Format("2006-01-02 15:04:05")
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(x))
	case string:
		if len(x) > cellMax*4 {
			x = x[:cellMax*4]
		}
		if utf8.RuneCountInString(x) > cellMax {
			x = string([]rune(x)[:cellMax])
		}
		return txt.Clean(x)
	}
	return txt.Clean(fmt.Sprint(v))
}

// cellFull is a value for the value box: line breaks kept, control characters escaped.
func cellFull(v any, k db.Kind) string {
	switch x := v.(type) {
	case nil:
		return "∅ (null)"
	case time.Time:
		if k == db.Date {
			return x.Format("2006-01-02")
		}
		return x.Format(time.RFC3339Nano)
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(x))
	case string:
		return txt.CleanText(x)
	}
	return txt.CleanText(fmt.Sprint(v))
}

// valueText is a value as filters compare it; ok is false for bytes.
func valueText(v any, k db.Kind) (string, bool) {
	switch x := v.(type) {
	case time.Time:
		switch {
		case k == db.Date:
			return x.Format("2006-01-02"), true
		case x.Nanosecond() == 0:
			return x.Format("2006-01-02 15:04:05"), true
		}
		return x.Format("2006-01-02 15:04:05.999999"), true
	case []byte:
		return "", false
	case string:
		return x, true
	}
	return fmt.Sprint(v), true
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// sortValues orders numerically when every value is a number, else as text.
func sortValues(vs []string) {
	for _, v := range vs {
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			sort.Strings(vs)
			return
		}
	}
	sort.SliceStable(vs, func(i, j int) bool {
		a, _ := strconv.ParseFloat(vs[i], 64)
		b, _ := strconv.ParseFloat(vs[j], 64)
		return a < b
	})
}
