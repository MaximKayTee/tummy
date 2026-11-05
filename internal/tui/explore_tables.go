package tui

import (
	"cmp"
	"context"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"tummy/internal/config"
	"tummy/internal/db"
)

func (e *explorer) discover() tea.Cmd {
	e.loading, e.listSeq = true, nextSeq()
	h, name, seq := e.h, e.conn.Name, e.listSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		d, err := h.get(ctx)
		if err != nil {
			return discoverMsg{Conn: name, Err: err, seq: seq}
		}
		tables, err := allTables(ctx, d)
		return discoverMsg{Conn: name, Tables: tables, Err: err, seq: seq}
	}
}

// allTables lists the tables of every schema the connection can read, the
// connection's schema first.
func allTables(ctx context.Context, d *db.DB) ([]string, error) {
	ts, err := d.ListAllTables(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return out, nil
}

// listSchemas lists the schemas with tables, the connection's first, system ones left out.
func listSchemas(ctx context.Context, d *db.DB) ([]string, error) { return d.ListSchemas(ctx) }

func (e *explorer) visibleTables() []string {
	f := strings.ToLower(e.filter.Value())
	if f == "" {
		return e.sortTables(e.tables)
	}
	var out []string
	for _, t := range e.tables {
		if strings.Contains(strings.ToLower(t), f) {
			out = append(out, t)
		}
	}
	return e.sortTables(out)
}

func (e *explorer) keyTables(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	if e.filtering {
		switch msg.String() {
		case "enter", "esc":
			e.filtering = false
			e.filter.Blur()
			e.cur = 0
			return wizContinue, nil
		}
		var cmd tea.Cmd
		e.filter, cmd = e.filter.Update(msg)
		e.cur = 0
		return wizContinue, cmd
	}
	vis := e.visibleTables()
	switch msg.String() {
	case "esc", "q", "left", "h":
		return wizCancel, nil
	case "/":
		e.filtering = true
		return wizContinue, e.filter.Focus()
	case "up", "k":
		e.cur = max(e.cur-1, 0)
	case "down", "j":
		e.cur = min(e.cur+1, max(len(vis)-1, 0))
	case "pgdown", " ", "ctrl+d":
		e.cur = min(e.cur+max(e.tableRows, 1), max(len(vis)-1, 0))
	case "pgup", "ctrl+u":
		e.cur = max(e.cur-max(e.tableRows, 1), 0)
	case "g":
		e.cur = 0
	case "G":
		e.cur = max(len(vis)-1, 0)
	case "R":
		return wizContinue, e.discover()
	case "s":
		if e.statsBusy == "" && e.opts.State != nil {
			return wizContinue, e.refreshStats()
		}
	case "c":
		if e.statsBusy == "" && e.opts.State != nil && e.cur < len(vis) {
			return wizContinue, e.countTable(vis[e.cur])
		}
	case "o":
		e.sortBy = (e.sortBy + 1) % 3
		e.cur, e.offset = 0, 0
	case "S":
		e.resetPreview()
		e.table, e.spec, e.streamName = "", config.Stream{}, ""
		e.page = pagePreview
		e.editSQL = true
		e.sqlInput.SetValue("")
		return wizContinue, e.sqlInput.Focus()
	case "enter", "l":
		if e.cur < len(vis) {
			return wizContinue, e.openTable(vis[e.cur])
		}
	}
	return wizContinue, nil
}

// openTable previews a table with a fresh pipeline; under a wildcard, with its settings.
func (e *explorer) openTable(name string) tea.Cmd {
	e.resetPreview()
	e.table = name
	e.streamName = ""
	spec := config.Stream{}
	if w := e.wildcard; w != nil && wildMatch(w.Name, name) {
		spec = w.Stream
		e.streamName = name
	}
	e.openSpec(name, spec)
	e.page = pagePreview
	return e.fetch()
}

// resetPreview drops everything tied to the previous table or query.
func (e *explorer) resetPreview() {
	e.cols, e.rows, e.natural = nil, nil, nil
	e.ticked, e.widths = map[string]bool{}, map[string]int{}
	e.pipeline, e.stepCols, e.compiled, e.args, e.shown = nil, nil, "", nil, ""
	e.stepCur, e.focus, e.stepEdit, e.stepWait, e.panelOff, e.panelCur = 0, focusGrid, -1, -1, 0, 0
	e.colCur, e.colOff, e.rowCur, e.rowOff = 0, 0, 0, 0
	e.fetchErr, e.note, e.noteErr, e.showVal = "", "", false, false
	e.wantSelect, e.origSelect, e.stepsErr = nil, nil, ""
	e.baseSteps, e.baseSQL, e.tickBase = "", "", ""
	e.limit, e.total, e.counted, e.complete, e.parents = 1, -1, "", false, ""
	e.fresh, e.anchor, e.discarded, e.wm = true, false, false, nil
}

// openSpec takes a stream's settings: its steps become the pipeline, and without steps
// its select becomes the ticks so dropped columns can come back.
func (e *explorer) openSpec(name string, spec config.Stream) {
	e.resetPreview()
	e.spec = spec
	e.origSelect = spec.Select
	if spec.Steps != "" {
		p, err := parseSteps(spec.Steps)
		if err != nil {
			e.stepsErr = "steps: " + err.Error()
			e.fetchErr = e.stepsErr
		}
		e.pipeline = p
	}
	switch {
	case e.isAPI:
		e.apiBase = spec
		e.endpoint = e.endpointForm(spec)
		e.wantSelect, e.spec.Select = spec.Select, nil
	case spec.SQL == "":
		e.table = name
		if len(e.pipeline) == 0 && len(spec.Select) > 0 {
			e.wantSelect, e.spec.Select = spec.Select, nil
		}
	}
	e.baseSteps = renderSteps(e.pipeline)
	e.baseSQL = spec.SQL
}

func (e *explorer) tablesView(width, height int) []string {
	out := []string{" " + sTitle.Render(e.conn.Name) + "  " + sDim.Render(e.conn.Type+"  "+config.Redact(cmp.Or(e.conn.URL, e.conn.Path)))}
	if e.filtering || e.filter.Value() != "" {
		out = append(out, " "+e.filter.View())
	}
	switch {
	case e.statsBusy != "":
		out = append(out, "  "+sWarn.Render("◐ "+txt.Clean(e.statsBusy)))
	case e.statsNote != "" && e.statsErr:
		out = append(out, "  "+sErr.Render(ansi.Truncate(glyphErr+" "+txt.Clean(e.statsNote), max(width-2, 10), "…")))
	case e.statsNote != "":
		out = append(out, "  "+sDim.Render(txt.Clean(e.statsNote)))
	default:
		out = append(out, "")
	}
	switch {
	case e.loading:
		return append(out, "  "+sWarn.Render("◐ tables"))
	case e.loadErr != "":
		return append(out, errBox(e.loadErr, width, max(height-len(out), 3))...)
	}
	vis := e.visibleTables()
	if len(vis) == 0 {
		return append(out, "  "+sDim.Render("none"))
	}
	withStats := len(e.stats) > 0 && width >= statsWidth+24
	nameW := width
	if withStats {
		nameW = width - statsWidth - 2
		out = append(out, sDim.Render(pad("  TABLE", nameW, false)+"  "+statsHeader()))
	}
	rows := max(height-len(out), 1)
	e.tableRows = rows
	if e.cur < e.offset {
		e.offset = e.cur
	}
	if e.cur >= e.offset+rows {
		e.offset = e.cur - rows + 1
	}
	for i := e.offset; i < min(e.offset+rows, len(vis)); i++ {
		line := pad("  "+txt.Clean(vis[i]), width, false)
		if withStats {
			s, ok := e.stats[vis[i]]
			line = pad("  "+txt.Clean(vis[i]), nameW, false) + "  " + statsColumns(s, ok)
		}
		if i == e.cur {
			line = selectedLine(line, width)
		}
		out = append(out, line)
	}
	return out
}

// ---------- the stream behind a preview ----------

// findStream looks the stream up in the replication files of the source: by name, or
// through a wildcard that matches it. rep narrows the search to one file. It returns
// the replication, or "" when none or several fit, and the stream's values.
func findStream(home, rep, connName string, s config.ResolvedStream) (string, config.Stream) {
	type hit struct {
		rep   string
		spec  config.Stream
		exact bool
	}
	var hits []hit
	files, _ := config.ListReplicationFiles(home)
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".toml")
		if rep != "" && name != rep {
			continue
		}
		r, err := config.LoadReplication(f)
		if err != nil || r.Source != connName {
			continue
		}
		found := false
		for _, rs := range r.Resolved {
			if rs.Name == s.Name {
				hits = append(hits, hit{r.Name, rs.Stream, reflect.DeepEqual(rs.Stream, s.Stream)})
				found = true
			}
		}
		if found || s.IsWildcard() || r.Excluded(s.Name) {
			continue
		}
		for _, rs := range r.Resolved {
			if rs.IsWildcard() && wildMatch(rs.Name, s.Name) {
				hits = append(hits, hit{rep: r.Name, spec: rs.Stream})
				break
			}
		}
	}
	if len(hits) > 1 {
		var exact []hit
		for _, h := range hits {
			if h.exact {
				exact = append(exact, h)
			}
		}
		hits = exact
	}
	if len(hits) != 1 {
		return "", s.Stream
	}
	return hits[0].rep, hits[0].spec
}

// wildMatch matches a table against a stream pattern the way Expand does.
func wildMatch(pattern, table string) bool {
	ps, pn := splitLast(pattern)
	ts, tn := splitLast(table)
	if ps != "" && !strings.EqualFold(ps, ts) {
		return false
	}
	ok, _ := path.Match(pn, tn)
	return ok
}

func splitLast(s string) (string, string) {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

// wildPrefix is the table-list filter for a pattern: the text before the first wildcard.
func wildPrefix(pattern string) string {
	if i := strings.IndexAny(pattern, "*?["); i >= 0 {
		return pattern[:i]
	}
	return pattern
}
