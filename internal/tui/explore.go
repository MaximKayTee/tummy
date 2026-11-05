package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"tummy/internal/txt"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/steps"
)

// explorePage is the screen the explorer shows.
type explorePage int

const (
	pageTables explorePage = iota
	pageEndpoint
	pagePreview
	pageStep
	pagePalette
	pageTarget
	pageStream
)

// gridFocus is where preview keys go.
type gridFocus int

const (
	focusGrid gridFocus = iota
	focusSteps
)

const (
	previewLimit  = 200 // rows per fetch; m adds another batch
	panelMinWidth = 90  // narrower: no steps panel
)

// explorer browses one connection: tables, previews, steps, and adding streams to a replication.
type explorer struct {
	opts  Options
	conns *config.Connections
	conn  *config.Conn
	isAPI bool
	dl    db.Dialect // database sources
	h     *dbHandle  // database sources

	page  explorePage
	width int // last view width
	ask   *exploreAsk

	// tables
	tables      []string
	loading     bool
	loadErr     string
	cur, offset int
	filter      textinput.Model
	filtering   bool
	tableRows   int                    // table list: rows per screen, set by view
	wildcard    *config.ResolvedStream // tables matching it open with its where, select and steps

	// what is being previewed
	table    string        // table name, or "" for sql / api
	spec     config.Stream // the stream as previewed; select moves to the ticks without steps
	apiBase  config.Stream // api stream the endpoint form starts from
	endpoint *form
	vars     map[string]string // typed {parent.field} values

	// the stream the preview belongs to, for ctrl+s
	fromStream bool   // esc leaves instead of going to the table list
	readOnly   bool   // target data: no steps, no saving
	sample     bool   // opened in place of target data that does not exist yet
	discarded  bool   // leaving was confirmed: dirty reports false from then on
	repName    string // replication file, "" when unknown
	streamName string
	origSelect []string
	stepsErr   string // the stream's steps did not parse; saving would lose them
	baseSteps  string
	baseSQL    string
	tickBase   string
	wantSelect []string // ticks for the first result

	// preview
	cols     []db.Column
	rows     [][]any
	ticked   map[string]bool
	fresh    bool // next result resets cursor and ticks
	colCur   int
	colOff   int
	rowOff   int
	rowCur   int
	fetching bool
	fetchSeq int
	fetchErr string
	note     string
	noteErr  bool
	took     time.Duration
	sqlInput textarea.Model
	editSQL  bool
	limit    int    // rows (or pages for an API) to fetch, in batches
	total    int    // rows the query returns in all; -1 unknown
	counted  string // query total was counted for
	complete bool   // every row is loaded: m has nothing to add
	parents  string // for_each: parents the preview ran for, "3 of 10 users"
	anchor   bool   // next result starts at the first row
	args     []any  // arguments of compiled
	shown    string // the query a run sends, ticks included, arguments inlined
	shownSeq int
	wm       *watermarkInfo // incremental streams: the bound a run starts from
	respKeys []apiKey       // api: top-level keys of the response, for the endpoint form
	srcPK    []string       // primary key of the previewed table
	pkFor    string         // table srcPK was read for
	natural  []int          // per-column width, measured once per fetch
	widths   map[string]int // per-column width overrides
	wide     bool           // no width cap
	showVal  bool           // full value of the current cell

	// add to replication
	repPick  *form
	stream   *form
	draft    config.ReplicationDraft
	editing  string        // stream of draft the form was filled from
	formBase config.Stream // the stream the form shows, the preview's edits applied
	done     statusMsg

	// transformation steps, database sources only
	pipeline []steps.Step
	stepCols [][]string // columns each step sees, from the last compile
	stepCur  int
	focus    gridFocus
	compiled string
	stepEd   *stepEditor
	stepEdit int // index being edited, -1 for new
	stepWait int // step whose prefix preview is loading, -1 for none
	panelOff int // first line of the steps panel shown
	panelCur int // step the panel last scrolled to

	palette    *picker
	paletteCmd map[string]string
}

// watermarkInfo is the update-key bound of an incremental stream, read from its target.
type watermarkInfo struct {
	val any    // MAX(update_key) of the target; nil: no target yet, every row loads
	err string // the target could not be read
}

// exploreAsk is a pending y/n question.
type exploreAsk struct {
	text string
	yes  func() (wizResult, tea.Cmd)
}

// dbHandle is the explorer's database connection: opened on first use, closed on leaving.
type dbHandle struct {
	mu     sync.Mutex
	params db.Params
	d      *db.DB
	closed bool
}

func (h *dbHandle) get(ctx context.Context) (*db.DB, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errors.New("closed")
	}
	if h.d == nil {
		d, err := db.Open(ctx, h.params)
		if err != nil {
			return nil, err
		}
		h.d = d
	}
	return h.d, nil
}

func (h *dbHandle) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	if h.d != nil {
		h.d.Close()
		h.d = nil
	}
}

func newExplorer(opts Options, connName string) (*explorer, error) {
	conns, err := config.LoadConnections(opts.Connections)
	if err != nil {
		return nil, err
	}
	c, err := conns.Get(connName)
	if err != nil {
		return nil, err
	}
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = "filter"
	e := &explorer{opts: opts, conns: conns, conn: c, isAPI: c.Type == "api", filter: ti, sqlInput: newSQLArea(),
		ticked: map[string]bool{}, widths: map[string]int{}, stepEdit: -1, stepWait: -1, fresh: true, total: -1}
	if e.isAPI {
		e.page = pageEndpoint
		e.endpoint = e.endpointForm(config.Stream{})
		return e, nil
	}
	if e.dl, err = db.DialectFor(c.Type); err != nil {
		return nil, err
	}
	e.h = &dbHandle{params: c.DB()}
	e.page = pageTables
	return e, nil
}

func newSQLArea() textarea.Model {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.Prompt = "  "
	ta.Placeholder = "select …"
	ta.SetHeight(6)
	return ta
}

// newStreamExplorer opens the explorer on one stream's preview. The replication is
// looked up by source and stream name.
func newStreamExplorer(opts Options, connName string, s config.ResolvedStream) (*explorer, error) {
	return newRepStreamExplorer(opts, "", connName, s)
}

// newRepStreamExplorer opens the explorer on a stream of replication rep; ctrl+s writes
// the steps, select and sql back to it.
func newRepStreamExplorer(opts Options, rep, connName string, s config.ResolvedStream) (*explorer, error) {
	e, err := newExplorer(opts, connName)
	if err != nil {
		return nil, err
	}
	var spec config.Stream
	e.repName, spec = findStream(opts.Home, rep, connName, s)
	e.fromStream = true
	e.streamName = s.Name
	e.openSpec(s.Name, spec)
	e.page = pagePreview
	return e, nil
}

// newWildcardExplorer lists the tables a wildcard stream matches; a table opens with the
// wildcard's where, select and steps, and ctrl+s writes it as its own stream.
func newWildcardExplorer(opts Options, rep, connName string, s config.ResolvedStream) (*explorer, error) {
	e, err := newExplorer(opts, connName)
	if err != nil {
		return nil, err
	}
	var spec config.Stream
	e.repName, spec = findStream(opts.Home, rep, connName, s)
	w := s
	w.Stream = spec
	e.wildcard = &w
	e.filter.SetValue(wildPrefix(s.Name))
	return e, nil
}

// newTargetExplorer shows the rows a stream loaded into its target table.
func newTargetExplorer(opts Options, connName, object string) (*explorer, error) {
	e, err := newExplorer(opts, connName)
	if err != nil {
		return nil, err
	}
	e.table = object
	e.page = pagePreview
	e.fromStream = true
	e.readOnly = true
	return e, nil
}

// markSample tags the preview as the source sample shown in place of target data
// that does not exist yet.
func (e *explorer) markSample() { e.sample = true }

func (e *explorer) title() string { return "explore " + e.conn.Name }

func (e *explorer) result() statusMsg { return e.done }
func (e *explorer) saved() string     { return e.conn.Name }

func (e *explorer) help() string {
	left, right := e.footer()
	if right == "" {
		return left
	}
	return left + "  " + right
}

// footer is the key line: left keys in order of importance, cut as whole keys to the
// width; right is never cut.
func (e *explorer) footer() (string, string) {
	back := helpLine("esc", "back")
	if e.ask != nil {
		return sWarn.Render(e.ask.text), ""
	}
	var pairs []string
	var off map[string]bool
	switch e.page {
	case pageTables:
		if e.filtering {
			return helpLine("type", "filter", "enter", "done"), helpLine("esc", "done")
		}
		pairs = []string{"enter", "preview", "S", "sql", "/", "filter", "pgup/pgdn", "page", "R", "refresh"}
	case pageEndpoint:
		pairs = append(e.endpoint.hints(), "ctrl+s", "fetch")
	case pagePreview:
		pairs, off, back = e.previewKeys()
	case pageStep:
		pairs, back = e.stepEd.keys()
	case pagePalette:
		pairs, back = []string{"type", "filter", "↑/↓", "move", "enter", "run"}, helpLine("esc", "close")
	case pageTarget:
		pairs = append(e.repPick.hints(), "ctrl+s", "next")
	case pageStream:
		pairs = append(withoutEnterDone(e.stream), "ctrl+s", "save")
	}
	if back == "" {
		return fitKeys(e.width, off, pairs...), ""
	}
	room := e.width - lipgloss.Width(back) - lipgloss.Width(helpLine("?", "help")) - 6
	if e.width == 0 {
		room = 1 << 10
	}
	return fitKeys(room, off, pairs...), back
}

// withoutEnterDone drops "enter apply" from a form's keys: only ctrl+s saves it.
func withoutEnterDone(f *form) []string {
	h := f.hints()
	var out []string
	for i := 0; i+1 < len(h); i += 2 {
		if strings.HasPrefix(h[i], "enter") && f.drop == nil && f.onLast() {
			continue
		}
		out = append(out, h[i], h[i+1])
	}
	return out
}

// fitKeys renders key pairs while they fit in width; keys in off are shown dim.
func fitKeys(width int, off map[string]bool, pairs ...string) string {
	var parts []string
	used := 0
	for i := 0; i+1 < len(pairs); i += 2 {
		k, d := pairs[i], pairs[i+1]
		part := sKey.Render(k) + " " + sDim.Render(d)
		if off[k] {
			part = sDim.Render(k + " " + d)
		}
		w := lipgloss.Width(part)
		if len(parts) > 0 {
			w += 2
		}
		if width > 0 && used+w > width {
			break
		}
		parts = append(parts, part)
		used += w
	}
	return strings.Join(parts, "  ")
}

// previewKeys are the footer keys of the preview, most used first.
func (e *explorer) previewKeys() ([]string, map[string]bool, string) {
	back := helpLine("esc", "back")
	off := map[string]bool{}
	if e.complete {
		off["m"] = true
	}
	switch {
	case e.editSQL:
		return []string{"ctrl+s", "run", "enter", "new line"}, nil, helpLine("esc", "cancel")
	case e.showVal:
		return []string{"any key", "close"}, nil, ""
	case e.readOnly:
		return []string{"←/→ ↑/↓", "move", "v", "value", "m", "more", "> <", "width", "w", "wide", "R", "refresh"}, off, back
	case e.focus == focusSteps:
		return []string{"↑/↓", "step", "enter", "edit", "a", "add", "d", "delete", "J/K", "move", "pgup/pgdn", "sql"}, nil, helpLine("tab", "grid")
	}
	pairs := []string{":", "commands"}
	if e.steppable() {
		pairs = append(pairs, "t", "step", "tab", "steps")
	}
	pairs = append(pairs, "space", "tick")
	if e.canSaveBack() {
		pairs = append(pairs, "ctrl+s", "save")
	}
	pairs = append(pairs, "+", "add", "m", "more", "v", "value")
	if e.steppable() {
		pairs = append(pairs, "S", "sql")
	}
	return pairs, off, back
}

// typing reports whether keys go to text, so ? types instead of opening help.
func (e *explorer) typing() bool {
	if e.ask != nil {
		return true
	}
	switch e.page {
	case pageTables:
		return e.filtering
	case pageEndpoint:
		return e.endpoint.typing()
	case pagePreview:
		return e.editSQL
	case pageStep:
		return e.stepEd.typing()
	case pagePalette:
		return true
	case pageTarget:
		return e.repPick.typing()
	case pageStream:
		return e.stream.typing()
	}
	return false
}

// keyHelp is the explorer section of the help window for the page on screen.
func (e *explorer) keyHelp() (string, [][2]string) {
	switch e.page {
	case pageTables:
		return "tables", [][2]string{
			{"enter", "preview"}, {"S", "sql query"}, {"/", "filter"}, {"R", "refresh"}, {"esc", "back"},
		}
	case pageEndpoint:
		return "endpoint", [][2]string{{"enter", "next field"}, {"ctrl+s", "fetch"}, {"esc", "back"}}
	case pagePreview:
		if e.focus == focusSteps {
			return "steps", [][2]string{
				{"↑/↓", "step; past the last: sql"}, {"pgup/pgdn", "scroll"}, {"enter  e", "edit step"},
				{"a", "add step"}, {"d", "delete step (y/n)"}, {"J/K", "move step down / up"}, {"tab  esc", "grid"},
			}
		}
		rows := [][2]string{
			{"←/→ ↑/↓", "move"}, {"g/G", "first / last row"}, {"pgup/pgdn", "page"},
		}
		if !e.readOnly {
			rows = append(rows, [2]string{"space", "tick column"}, [2]string{"a / n", "tick all / none"})
		}
		if e.steppable() {
			rows = append(rows, [2]string{"t", "add step"}, [2]string{"tab", "steps and sql"}, [2]string{"S", "edit sql"})
		}
		rows = append(rows, [2]string{":", "commands"}, [2]string{"v  enter", "full value"}, [2]string{"m", "more rows"},
			[2]string{"R", "refresh"}, [2]string{"> <", "column width"}, [2]string{"=", "column width reset"}, [2]string{"w", "wide columns"})
		if !e.readOnly {
			rows = append(rows, [2]string{"+", "add to replication"})
		}
		if e.canSaveBack() {
			rows = append(rows, [2]string{"ctrl+s", "save to " + e.repName + ".toml › " + e.streamName})
		}
		return "preview", append(rows, [2]string{"esc", "back"})
	case pageStep:
		return "step " + e.stepEd.op, [][2]string{
			{"tab  ↓", "next field"}, {"space", "tick; open list"}, {"enter", "next; on the last field: apply"},
			{"ctrl+s", "apply"}, {"esc", "back"},
		}
	case pagePalette:
		return "commands", [][2]string{{"type", "filter"}, {"↑/↓", "move"}, {"enter", "run"}, {"esc", "close"}}
	case pageTarget, pageStream:
		return "add to replication", [][2]string{{"tab  ↓", "next field"}, {"ctrl+s", "save"}, {"esc", "back"}}
	}
	return "", nil
}

// start kicks off the first load.
func (e *explorer) start() tea.Cmd {
	if e.page == pagePreview {
		return e.fetch()
	}
	if e.isAPI {
		return e.endpoint.focus()
	}
	if len(e.tables) > 0 {
		return nil
	}
	return e.discover()
}

// update closes the connection when the explorer is left.
func (e *explorer) update(msg tea.Msg) (wizResult, tea.Cmd) {
	res, cmd := e.dispatch(msg)
	if res != wizContinue {
		e.h.close()
	}
	return res, cmd
}

func (e *explorer) dispatch(msg tea.Msg) (wizResult, tea.Cmd) {
	switch msg := msg.(type) {
	case discoverMsg:
		e.loading = false
		e.loadErr = ""
		if msg.Err != nil {
			e.loadErr = msg.Err.Error()
		} else {
			e.tables = msg.Tables
		}
		return wizContinue, nil
	case previewMsg:
		return wizContinue, e.applyPreview(msg)
	case prefixMsg:
		return wizContinue, e.applyPrefix(msg)
	case shownMsg:
		if msg.seq == e.shownSeq {
			e.shown = msg.SQL
		}
		return wizContinue, nil
	case valuesMsg:
		if e.stepEd != nil && e.stepEd == msg.ed {
			e.stepEd.setValues(msg)
		}
		return wizContinue, nil
	case tea.KeyMsg:
		if e.ask != nil {
			ask := e.ask
			e.ask = nil
			if msg.String() == "y" {
				return ask.yes()
			}
			return wizContinue, nil
		}
		return e.key(msg)
	}
	return wizContinue, nil
}

func (e *explorer) key(msg tea.KeyMsg) (wizResult, tea.Cmd) {
	switch e.page {
	case pageTables:
		return e.keyTables(msg)
	case pageEndpoint:
		res, cmd := e.endpoint.update(msg)
		switch res {
		case formCancel:
			return wizCancel, nil
		case formSubmit:
			e.resetPreview()
			e.spec = e.specFromEndpoint()
			e.table = ""
			return wizContinue, e.fetch()
		}
		return wizContinue, cmd
	case pagePreview:
		return e.keyPreview(msg)
	case pagePalette:
		res, cmd := e.palette.update(msg)
		switch res {
		case formCancel:
			e.page = pagePreview
			return wizContinue, nil
		case formSubmit:
			e.page = pagePreview
			vis := e.palette.visible()
			if e.palette.cur >= len(vis) {
				return wizContinue, nil
			}
			return e.runCommand(e.paletteCmd[vis[e.palette.cur]])
		}
		return wizContinue, cmd
	case pageStep:
		res, cmd := e.stepEd.update(msg)
		switch res {
		case formCancel:
			e.page = pagePreview
			e.stepEdit = -1
			return wizContinue, nil
		case formSubmit:
			if _, err := steps.Parse(e.stepEd.result.String()); err != nil {
				e.stepEd.err = err.Error()
				return wizContinue, nil
			}
			return wizContinue, e.applyStep(e.stepEd.result)
		}
		return wizContinue, cmd
	case pageTarget:
		res, cmd := e.repPick.update(msg)
		switch res {
		case formCancel:
			e.page = pagePreview
		case formSubmit:
			return wizContinue, e.toStreamForm()
		}
		return wizContinue, cmd
	case pageStream:
		res, cmd := e.stream.update(msg)
		switch res {
		case formCancel:
			e.page = pageTarget
			return wizContinue, e.repPick.focus()
		case formSubmit:
			if msg.String() != "ctrl+s" {
				return wizContinue, cmd // enter on the last field: only ctrl+s writes the file
			}
			if err := e.saveStream(); err != nil {
				e.stream.err = txt.FirstLine(err.Error())
				return wizContinue, nil
			}
			e.page = pagePreview
			return wizContinue, nil
		}
		return wizContinue, cmd
	}
	return wizContinue, nil
}

// ---------- views ----------

func (e *explorer) view(width, height int) []string {
	e.width = width
	switch e.page {
	case pageTables:
		return e.tablesView(width, height)
	case pageEndpoint:
		out := e.endpoint.view(width)
		if e.fetching {
			out = append(out, "", "  "+sWarn.Render("◐ fetching"))
		} else if e.fetchErr != "" {
			out = append(append(out, ""), errBox(e.fetchErr, width, max(height-len(out)-1, 3))...)
		}
		return out
	case pagePreview:
		return e.previewView(width, height)
	case pageStep:
		return e.stepEd.view(width, height)
	case pagePalette:
		return e.palette.view(width, height)
	case pageTarget:
		return e.repPick.view(width, height)
	case pageStream:
		return e.stream.view(width, height)
	}
	return nil
}

// errBox shows an error in full, wrapped in a box, at most height lines.
func errBox(msg string, width, height int) []string {
	w := max(width-4, 16)
	box := sErrBox.Width(w).Render(glyphErr + " " + strings.TrimSpace(txt.CleanText(msg)))
	lines := strings.Split(box, "\n")
	if height > 2 && len(lines) > height {
		lines = append(lines[:height-1], lines[len(lines)-1])
	}
	for i, l := range lines {
		lines[i] = " " + l
	}
	return lines
}
