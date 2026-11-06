package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"tummy/internal/txt"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/state"
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
	pageBookmark
	pageDrop
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
	listSeq     int // the table list being read
	loadErr     string
	cur, offset int
	filter      textinput.Model
	filtering   bool
	tableRows   int                        // table list: rows per screen, set by view
	stats       map[string]state.TableStat // stored row counts and sizes, by table
	statsSeq    int
	statsBusy   string // what the refresh or count is doing; "" when idle
	statsNote   string
	statsErr    bool
	sortBy      statsSort
	wildcard    *config.ResolvedStream // tables matching it open with its where, select and steps

	// what is being previewed
	table    string        // table name, or "" for sql / api
	spec     config.Stream // the stream as previewed; select moves to the ticks without steps
	apiBase  config.Stream // api stream the endpoint form starts from
	endpoint *form
	vars     map[string]string // typed {parent.field} values

	// the stream the preview belongs to, for ctrl+s
	fromStream bool   // esc leaves instead of going to the table list
	sample     bool   // opened in place of target data that does not exist yet
	discarded  bool   // leaving was confirmed: dirty reports false from then on
	repName    string // replication file, "" when unknown
	streamName string
	bound      *config.ReplicationDraft // a wizard's draft: saves go there, not to a file
	draftSaved bool                     // a save landed in bound
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
	noteAt   time.Time // notes fade after noteFor
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
	showCols bool           // schema: every column, its type and first value

	// bookmarks: saved queries on this connection
	marks    []*config.Bookmark
	bookmark string // the bookmark the preview was opened from or saved as
	markForm *form

	// statements off the preview: full run, execute, drop table; one at a time
	busy      string // what is running, for the head; "" when idle
	busySeq   int
	benchText string // the last full run: rows and time
	dropTable string
	dropForm  *form
	dropFrom  explorePage // page to return to

	// add to replication
	repPick  *form
	stream   *form
	draft    config.ReplicationDraft
	editing  string        // stream of draft the form was filled from
	formBase config.Stream // the stream the form shows, the preview's edits applied
	done     statusMsg

	// transformation steps, database sources only
	pipeline  []steps.Step
	stepCols  [][]string // columns each step sees, from the last compile
	stepCur   int
	focus     gridFocus
	compiled  string
	stepEd    *stepEditor
	stepEdit  int // index being edited, -1 for new
	stepWait  int // step whose prefix preview is loading, -1 for none
	prefixSeq int
	panelOff  int // first line of the steps panel shown
	panelCur  int // step the panel last scrolled to

	palette    *picker
	paletteCmd map[string]string
}

// asyncSeq numbers the requests of every screen in the process: a result matches only
// the request that asked for it, never one of a screen opened since.
var asyncSeq atomic.Int64

func nextSeq() int { return int(asyncSeq.Add(1)) }

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
	if err := e.loadMarks(); err != nil {
		return nil, err
	}
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

// newDraftStreamExplorer opens a stream of a wizard's draft; saves land in the draft.
func newDraftStreamExplorer(opts Options, d *config.ReplicationDraft, sd config.StreamDraft) (*explorer, error) {
	e, err := newExplorer(opts, d.Source)
	if err != nil {
		return nil, err
	}
	e.bound, e.repName, e.streamName, e.fromStream = d, d.Name, sd.Name, true
	if strings.ContainsAny(sd.Name, "*?[") {
		w := config.ResolvedStream{Name: sd.Name, Stream: sd.Stream}
		e.wildcard = &w
		e.filter.SetValue(wildPrefix(sd.Name))
		return e, nil
	}
	e.openSpec(sd.Name, sd.Stream)
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

// newTargetExplorer shows the rows stream of rep loaded into its target table object.
// Steps apply on top; B bookmarks them under the stream.
func newTargetExplorer(opts Options, rep, stream, connName, object string) (*explorer, error) {
	e, err := newExplorer(opts, connName)
	if err != nil {
		return nil, err
	}
	e.table = object
	e.page = pagePreview
	e.fromStream = true
	e.repName, e.streamName = rep, stream
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
		pairs = []string{"enter", "preview", "s", "sizes", "c", "count", "o", "sort: " + e.sortBy.String(), "S", "sql", "/", "filter", "pgup/pgdn", "page", "R", "refresh"}
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
		pairs := []string{"ctrl+s", "preview"}
		if e.writable() {
			pairs = append(pairs, "ctrl+e", "execute")
		}
		return append(pairs, "enter", "new line"), nil, helpLine("esc", "cancel")
	case e.showVal || e.showCols:
		return []string{"any key", "close"}, nil, ""
	case e.focus == focusSteps:
		return []string{"↑/↓", "step", "enter", "edit", "a", "add", "d", "delete", "J/K", "move", "y", "copy sql", "b", "full run", "pgup/pgdn", "sql"}, nil, helpLine("tab", "grid")
	}
	pairs := []string{":", "commands"}
	if e.steppable() {
		pairs = append(pairs, "t", "step", "tab", "steps")
	}
	pairs = append(pairs, "space", "tick")
	switch {
	case e.canSaveBack() || e.bookmark != "":
		pairs = append(pairs, "ctrl+s", "save")
	case e.canBookmark():
		pairs = append(pairs, "ctrl+s", "bookmark")
	}
	pairs = append(pairs, "+", "add", "m", "more", "v", "value")
	if e.steppable() {
		pairs = append(pairs, "S", "sql", "y", "copy sql", "b", "full run")
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
			{"enter", "preview"}, {"s", "row counts and sizes of every table from the catalog, kept as history"},
			{"c", "exact COUNT(*) of the table"}, {"o", "sort by name, rows, size"},
			{"S", "sql query"}, {"d", "delete bookmark (y/n)"}, {"D", "drop the table; its name is typed to confirm"}, {"/", "filter"}, {"R", "refresh the table list"}, {"esc", "back"},
		}
	case pageEndpoint:
		return "endpoint", [][2]string{{"enter", "next field"}, {"ctrl+s", "fetch"}, {"esc", "back"}}
	case pagePreview:
		if e.focus == focusSteps {
			return "steps", [][2]string{
				{"↑/↓", "step; past the last: sql"}, {"pgup/pgdn", "scroll"}, {"enter  e", "edit step"},
				{"a", "add step"}, {"d", "delete step (y/n)"}, {"J/K", "move step down / up"}, {"y", "copy sql"}, {"b", "full run: every row read, timed"}, {"tab  esc", "grid"},
			}
		}
		rows := [][2]string{
			{"←/→ ↑/↓", "move"}, {"g/G", "first / last row"}, {"pgup/pgdn", "page"},
			{"space", "tick column"}, {"a / n", "tick all / none"},
		}
		if e.steppable() {
			rows = append(rows, [2]string{"t", "add step"}, [2]string{"tab", "steps and sql"}, [2]string{"S", "edit sql; ctrl+s previews, ctrl+e executes as written"}, [2]string{"y", "copy sql"}, [2]string{"b", "full run: every row read, timed"})
		}
		rows = append(rows, [2]string{":", "commands"}, [2]string{"v  enter", "full value"}, [2]string{"i", "schema: columns, types, first values"}, [2]string{"m", "more rows"},
			[2]string{"R", "refresh"}, [2]string{"> <", "column width"}, [2]string{"=", "column width reset"}, [2]string{"w", "wide columns"},
			[2]string{"+", "add to replication"})
		switch {
		case e.canSaveBack():
			rows = append(rows, [2]string{"ctrl+s", "save to " + e.repName + ".toml › " + e.streamName})
		case e.bookmark != "":
			rows = append(rows, [2]string{"ctrl+s", "save bookmark " + e.bookmark})
		case e.canBookmark():
			rows = append(rows, [2]string{"ctrl+s", "bookmark: the table or query and the steps, in bookmarks.toml; the replication is not changed"})
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
	case pageBookmark:
		return "bookmark", [][2]string{{"enter", "save"}, {"esc", "back"}}
	case pageDrop:
		return "drop table", [][2]string{{"enter", "drop; the table name must match"}, {"esc", "back"}}
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
		return e.loadStats()
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
		if !e.loading || msg.seq != e.listSeq || msg.Conn != e.conn.Name {
			return wizContinue, nil // another screen's list, or one asked again since
		}
		e.loading = false
		e.loadErr = ""
		if msg.Err != nil {
			e.loadErr = msg.Err.Error()
			return wizContinue, nil
		}
		e.tables = msg.Tables
		return wizContinue, e.loadStats()
	case statsMsg:
		e.applyStats(msg)
		return wizContinue, nil
	case previewMsg:
		return wizContinue, e.applyPreview(msg)
	case prefixMsg:
		return wizContinue, e.applyPrefix(msg)
	case execMsg:
		return wizContinue, e.applyExec(msg)
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
	case pageDrop:
		res, cmd := e.dropForm.update(msg)
		switch res {
		case formCancel:
			e.page = e.dropFrom
			return wizContinue, nil
		case formSubmit:
			return wizContinue, e.drop()
		}
		return wizContinue, cmd
	case pageBookmark:
		res, cmd := e.markForm.update(msg)
		switch res {
		case formCancel:
			e.page = pagePreview
			return wizContinue, nil
		case formSubmit:
			if err := e.writeBookmark(e.markForm.get("name")); err != nil {
				e.markForm.err = txt.FirstLine(err.Error())
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
	case pageBookmark:
		return e.markForm.view(width, height)
	case pageDrop:
		return e.dropForm.view(width, height)
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
