// Package tui is the interactive view over the config directory and the state database.
package tui

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"
	"tummy/internal/txt"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
	"tummy/internal/state"
)

type Options struct {
	Home        string
	Connections string
	State       *state.Store
	// Refresh is the poll interval; zero means one second.
	Refresh time.Duration
	// RunCommand, when set, runs a replication as its own process: the command and
	// its arguments, then NAME and --streams. The process outlives the TUI.
	RunCommand []string
}

type view int

const (
	viewList view = iota
	viewDetail
	viewHistory
	viewRun
	viewConns
	viewWizard
)

type confirm struct {
	text  string
	onYes func() tea.Cmd
}

type tickMsg time.Time

// wizCloseMsg closes the open wizard without saving.
type wizCloseMsg struct{}

// Optional wizard parts: whether keys go to text, a footer with a part that is never cut,
// rows for the ? window, and whether leaving loses changes.
type (
	typer     interface{ typing() bool }
	footerer  interface{ footer() (left, right string) }
	keyHelper interface {
		keyHelp() (title string, rows [][2]string)
	}
	dirtier interface{ dirty() bool }
)

type helpState struct {
	page   int
	offset int
}

type Model struct {
	opts   Options
	cache  *fileCache
	snap   snapshot
	loaded bool
	width  int
	height int

	view      view
	cursor    map[view]int
	offset    map[view]int
	filter    textinput.Model
	filtering bool

	repName    string // selected replication
	streamName string // selected stream in history
	runs       []state.Run
	runsErr    error
	run        state.Run
	runFrom    view      // where esc on a run goes
	planName   string    // the run page shows a dry run of this replication
	planning   bool      // the dry run is still resolving
	planSample bool      // the dry run being resolved has the test load
	planStart  time.Time // for the elapsed time while planning
	vp         viewport.Model

	connResults map[string]connTestMsg
	testing     map[string]bool
	tableLists  map[string][]string // per connection: the last table list, reused by explore
	inflight    map[string]bool
	cancels     map[string]context.CancelFunc // stops the in-process runs
	quitting    bool                          // leaving once the in-process runs have stopped

	status   statusMsg
	statusAt time.Time
	spin     spinner.Model
	help     *helpState

	wiz     wizard
	wizFrom view
	ask     *confirm
	focusOn string // name to select on the next snapshot
	thenRep bool   // the first connection is being made: a new replication follows
}

func New(opts Options) Model {
	if opts.Refresh == 0 {
		opts.Refresh = time.Second
	}
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = "filter"
	ti.CharLimit = 64
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = sWarn
	return Model{
		opts:        opts,
		cache:       newFileCache(),
		cursor:      map[view]int{},
		offset:      map[view]int{},
		filter:      ti,
		connResults: map[string]connTestMsg{},
		testing:     map[string]bool{},
		tableLists:  map[string][]string{},
		inflight:    map[string]bool{},
		cancels:     map[string]context.CancelFunc{},
		spin:        sp,
		width:       80,
		height:      24,
	}
}

func Run(opts Options) error {
	p := tea.NewProgram(New(opts), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadSnapshot(), m.tick(), m.spin.Tick)
}

func (m Model) tick() tea.Cmd {
	return tea.Tick(m.opts.Refresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(msg.Width, 1), max(msg.Height, 1) // a pty may report 0 before its size is known
		m.vp.Width = max(m.width-4, 10)
		m.vp.Height = max(m.bodyHeight()-2, 3)
		m.clampCursor()
		return m, nil

	case tickMsg:
		cmds := []tea.Cmd{m.tick(), m.loadSnapshot()}
		if m.view == viewHistory {
			cmds = append(cmds, m.loadRuns(m.repName, m.streamName))
		}
		return m, tea.Batch(cmds...)

	case snapshotMsg:
		m.snap = snapshot(msg)
		m.loaded = true
		if m.focusOn != "" {
			for i, r := range m.visibleReps() {
				if r.Name == m.focusOn {
					m.cursor[viewList] = i
				}
			}
			for i, c := range m.visibleConns() {
				if c.Name == m.focusOn {
					m.cursor[viewConns] = i
				}
			}
			m.focusOn = ""
		}
		if m.view == viewDetail && m.snap.Err == nil {
			if _, ok := m.rep(m.repName); !ok {
				m.view = viewList
				m.status, m.statusAt = statusMsg{Text: m.repName + ".toml not found", IsErr: true}, time.Now()
			}
		}
		m.clampCursor()
		return m, nil

	case runsMsg:
		if msg.Replication == m.repName && msg.Stream == m.streamName {
			m.runs, m.runsErr = msg.Runs, msg.Err
			m.clampCursor()
		}
		return m, nil

	case statusMsg:
		m.status, m.statusAt = msg, time.Now()
		return m, nil

	case deletedMsg:
		m.status, m.statusAt = statusMsg(msg), time.Now()
		clear(m.tableLists) // a deleted connection's name may be reused
		m.clampCursor()
		return m, m.loadSnapshot()

	case quitMsg:
		// stop the runs of this process, leave when they have recorded their end
		for _, cancel := range m.cancels {
			cancel()
		}
		if len(m.cancels) == 0 { // detached runs carry on without this process
			return m, tea.Quit
		}
		m.quitting = true
		m.status, m.statusAt = statusMsg{Text: "stopping"}, time.Now()
		return m, tea.Tick(quitGrace, func(time.Time) tea.Msg { return forceQuitMsg{} })

	case forceQuitMsg:
		return m, tea.Quit

	case planMsg:
		if !m.planning || m.planName != msg.Replication {
			return m, nil // left the page meanwhile
		}
		if msg.Err != nil {
			m.planning = false
			m.vp.SetContent(sErr.Render(glyphErr + " " + txt.Clean(txt.FirstLine(msg.Err.Error()))))
			return m, nil
		}
		return m.openPlan(msg), nil
	case runDoneMsg:
		delete(m.inflight, msg.Replication)
		delete(m.cancels, msg.Replication)
		if m.quitting && len(m.inflight) == 0 {
			return m, tea.Quit
		}
		if msg.Err != nil {
			m.status, m.statusAt = statusMsg{Text: msg.Replication + ": " + describeRunErr(msg.Err), IsErr: true}, time.Now()
		} else {
			m.status, m.statusAt = statusMsg{Text: msg.Replication + ": done"}, time.Now()
		}
		return m, m.loadSnapshot()

	case connOpenMsg:
		if cw := m.connForm(); cw == nil || cw.probeSeq != msg.seq {
			return m, closeOpen(msg) // the form that tested is gone
		}

	case connTestMsg:
		delete(m.testing, msg.Name)
		if msg.Err == nil && len(msg.Tables) > 0 {
			m.tableLists[msg.Name] = msg.Tables
		}
		msg.Tables = nil
		m.connResults[msg.Name] = msg
		return m, nil

	case wizCloseMsg:
		if m.wiz != nil {
			m.wiz, m.view, m.thenRep = nil, m.wizFrom, false
			m.clampCursor()
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		if msg.Type == tea.KeyRunes { // a web terminal can send NUL and other control bytes as text
			msg.Runes = slices.DeleteFunc(slices.Clone(msg.Runes), func(r rune) bool {
				if msg.Paste && (r == '\n' || r == '\r' || r == '\t') {
					return false // pasted text keeps its lines
				}
				return r < 0x20 || r == 0x7f
			})
			if len(msg.Runes) == 0 {
				return m, nil
			}
		}
		return m.handleKey(msg)
	}
	if m.wiz != nil {
		return m.wizUpdate(msg)
	}
	return m, nil
}

func (m Model) wizUpdate(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case discoverMsg:
		if msg.Err == nil && len(msg.Tables) > 0 {
			m.tableLists[msg.Conn] = msg.Tables
		}
	}
	res, cmd := m.wiz.update(msg)
	switch res {
	case wizDone:
		m.status, m.statusAt = m.wiz.result(), time.Now()
		m.focusOn = m.wiz.saved()
		if cw, ok := m.wiz.(*connWizard); ok {
			delete(m.tableLists, cw.savedName) // the connection may point elsewhere now
			delete(m.tableLists, cw.original)
			if cw.probe != nil && cw.savedName != "" {
				m.connResults[cw.savedName] = connTestMsg{Name: cw.savedName, Err: cw.probe.Err, Took: cw.probe.Took}
			}
		}
		if m.wizFrom == viewDetail && m.focusOn != "" {
			if _, ok := m.wiz.(*repWizard); ok {
				m.repName = m.focusOn // renamed
			}
		}
		_, isConn := m.wiz.(*connWizard)
		m.wiz, m.view = nil, m.wizFrom
		m.clampCursor()
		if isConn && m.thenRep {
			m.thenRep = false
			next, ncmd := m.newRep()
			return next, tea.Batch(cmd, m.loadSnapshot(), ncmd)
		}
		return m, tea.Batch(cmd, m.loadSnapshot())
	case wizCancel:
		_, selfAsks := m.wiz.(*explorer) // the explorer asked already
		if _, isKey := msg.(tea.KeyMsg); isKey && !selfAsks && wizDirty(m.wiz) {
			m.ask = &confirm{text: "discard changes? y/n", onYes: func() tea.Cmd {
				return func() tea.Msg { return wizCloseMsg{} }
			}}
			return m, cmd
		}
		m.wiz, m.view, m.thenRep = nil, m.wizFrom, false
		m.clampCursor()
		return m, cmd
	}
	return m, cmd
}

// connForm is the connection editor on screen: the wizard itself, or one opened from the
// replication wizard's source or target list.
func (m Model) connForm() *connWizard {
	switch w := m.wiz.(type) {
	case *connWizard:
		return w
	case *repWizard:
		return w.child
	}
	return nil
}

func wizDirty(w wizard) bool {
	d, ok := w.(dirtier)
	return ok && d.dirty()
}

// wizTyping reports whether the wizard's keys go to text, so ? types instead of helping.
// A wizard that does not say is taken as typing.
func wizTyping(w wizard) bool {
	if t, ok := w.(typer); ok {
		return t.typing()
	}
	return true
}

func (m Model) openWizard(w wizard, err error) (tea.Model, tea.Cmd) {
	if err != nil {
		return m, status(err.Error(), true)
	}
	m.wizFrom = m.view
	if m.wizFrom == viewWizard {
		m.wizFrom = viewList
	}
	m.wiz, m.view = w, viewWizard
	m.status = statusMsg{}
	return m, w.start()
}

func (m Model) explore(name string) (tea.Model, tea.Cmd) {
	w, err := newExplorer(m.opts, name)
	if err != nil {
		return m, status(err.Error(), true)
	}
	w.tables = m.tableLists[name] // R lists again
	return m.openWizard(w, nil)
}

func (m Model) newConn() (tea.Model, tea.Cmd) {
	w, err := newConnWizard(m.opts, "")
	return m.openWizard(w, err)
}

func (m Model) editConn(name string) (tea.Model, tea.Cmd) {
	w, err := newConnWizard(m.opts, name)
	return m.openWizard(w, err)
}

func (m Model) newRep() (tea.Model, tea.Cmd) {
	conns, err := config.LoadConnections(m.opts.Connections)
	if err != nil {
		return m, status(err.Error(), true)
	}
	if len(conns.Conn) == 0 { // a replication needs connections: make one first
		next, cmd := m.newConn()
		nm := next.(Model)
		nm.status, nm.statusAt = statusMsg{Text: "no connections"}, time.Now()
		nm.thenRep = nm.wiz != nil
		return nm, cmd
	}
	w, err := newRepWizard(m.opts, conns, "")
	return m.openWizard(w, err)
}

func (m Model) addStream(name string) (tea.Model, tea.Cmd) {
	conns, err := config.LoadConnections(m.opts.Connections)
	if err != nil {
		return m, status(err.Error(), true)
	}
	w, err := newAddStreamWizard(m.opts, conns, name)
	return m.openWizard(w, err)
}

func (m Model) dupRep(name string) (tea.Model, tea.Cmd) {
	conns, err := config.LoadConnections(m.opts.Connections)
	if err != nil {
		return m, status(err.Error(), true)
	}
	w, err := newDupRepWizard(m.opts, conns, name)
	return m.openWizard(w, err)
}

func (m Model) editRep(name string) (tea.Model, tea.Cmd) {
	conns, err := config.LoadConnections(m.opts.Connections)
	if err != nil {
		return m, status(err.Error(), true)
	}
	w, err := newRepWizard(m.opts, conns, name)
	return m.openWizard(w, err)
}

func (m Model) editStream(rep, stream string) (tea.Model, tea.Cmd) {
	conns, err := config.LoadConnections(m.opts.Connections)
	if err != nil {
		return m, status(err.Error(), true)
	}
	w, err := newEditStreamWizard(m.opts, conns, rep, stream)
	return m.openWizard(w, err)
}

// deletedMsg reports a finished delete; the model refreshes on it.
type deletedMsg statusMsg

func (m Model) deleteRep(rep replication) tea.Cmd {
	return func() tea.Msg {
		if err := config.DeleteReplication(m.opts.Home, rep.Name); err != nil {
			return deletedMsg{Text: err.Error(), IsErr: true}
		}
		return deletedMsg{Text: "deleted " + rep.Name + ".toml"}
	}
}

func (m Model) deleteConn(name string) tea.Cmd {
	return func() tea.Msg {
		conns, err := config.LoadConnectionsRaw(m.opts.Connections)
		if err != nil {
			return deletedMsg{Text: err.Error(), IsErr: true}
		}
		delete(conns.Conn, name)
		if err := config.WriteConnections(m.opts.Connections, conns); err != nil {
			return deletedMsg{Text: err.Error(), IsErr: true}
		}
		return deletedMsg{Text: "deleted connection " + name}
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+z" {
		return m, tea.Suspend
	}
	before := m.view
	next, cmd := m.handleKeyInner(msg)
	if nm, ok := next.(Model); ok && nm.view != before && before != viewWizard && nm.view != viewWizard {
		nm.status = statusMsg{}
		return nm, cmd
	}
	return next, cmd
}

// quitMsg leaves after stopping the runs of this process; forceQuitMsg leaves regardless.
type (
	quitMsg      struct{}
	forceQuitMsg struct{}
)

// quitGrace is how long quitting waits for the stopped runs to record their end.
const quitGrace = 5 * time.Second

// quit leaves, asking first while a run of this process is in flight: quitting stops it.
func (m Model) quit(text string) (tea.Model, tea.Cmd) {
	var running []string
	for name := range m.cancels { // detached runs carry on without this process
		running = append(running, name)
	}
	if len(running) > 0 {
		sort.Strings(running)
		text = strings.Join(running, ", ") + " running. stop and quit? y/n"
	}
	if text != "" {
		m.ask = &confirm{text: text, onYes: func() tea.Cmd { return func() tea.Msg { return quitMsg{} } }}
		return m, nil
	}
	return m, tea.Quit
}

func (m Model) handleKeyInner(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.ask != nil {
		ask := m.ask
		m.ask = nil
		if msg.String() == "y" || msg.String() == "Y" {
			return m, ask.onYes()
		}
		return m, nil
	}
	if m.help != nil {
		return m.keyHelp(msg)
	}
	if m.wiz != nil {
		switch msg.String() {
		case "ctrl+c":
			if wizDirty(m.wiz) {
				return m.quit("quit and discard changes? y/n")
			}
			return m.quit("")
		case "f1":
			m.help = &helpState{}
			return m, nil
		case "?":
			if !wizTyping(m.wiz) {
				m.help = &helpState{}
				return m, nil
			}
		}
		return m.wizUpdate(msg)
	}
	if m.filtering {
		switch msg.String() {
		case "enter", "esc":
			m.filtering = false
			if msg.String() == "esc" {
				m.filter.SetValue("")
			}
			m.filter.Blur()
			m.clampCursor()
			return m, nil
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.cursor[m.view] = 0
		m.clampCursor()
		return m, cmd
	}

	switch {
	case key.Matches(msg, keys.Quit):
		if m.view != viewList && msg.String() == "q" {
			return m.back(), nil
		}
		return m.quit("")
	case key.Matches(msg, keys.Help):
		m.help = &helpState{}
		return m, nil
	case key.Matches(msg, keys.Refresh):
		return m, tea.Batch(m.loadSnapshot(), m.loadRuns(m.repName, m.streamName))
	case key.Matches(msg, keys.Up):
		if m.view == viewRun {
			m.vp.ScrollUp(1)
			return m, nil
		}
		m.move(-1)
		return m, nil
	case key.Matches(msg, keys.Down):
		if m.view == viewRun {
			m.vp.ScrollDown(1)
			return m, nil
		}
		m.move(1)
		return m, nil
	case key.Matches(msg, keys.PageUp) && m.view != viewRun:
		m.move(-max(m.bodyHeight()-2, 1))
		return m, nil
	case key.Matches(msg, keys.PageDown) && m.view != viewRun:
		m.move(max(m.bodyHeight()-2, 1))
		return m, nil
	case key.Matches(msg, keys.Top):
		m.cursor[m.view] = 0
		m.clampCursor()
		return m, nil
	case key.Matches(msg, keys.Bottom):
		m.cursor[m.view] = 1 << 30
		m.clampCursor()
		return m, nil
	case key.Matches(msg, keys.Filter) && (m.view == viewList || m.view == viewHistory || m.view == viewConns):
		m.filtering = true
		m.status = statusMsg{}
		m.filter.Focus()
		return m, textinput.Blink
	case key.Matches(msg, keys.Conns) && m.view != viewConns:
		m.view = viewConns
		m.filter.SetValue("")
		m.clampCursor()
		return m, nil
	case key.Matches(msg, keys.History) && m.view == viewList:
		m.repName, m.streamName = "", ""
		m.filter.SetValue("")
		m.view = viewHistory
		m.runs = nil
		m.cursor[viewHistory] = 0
		return m, m.loadRuns(m.repName, m.streamName)
	}

	switch m.view {
	case viewList:
		return m.keyList(msg)
	case viewDetail:
		return m.keyDetail(msg)
	case viewHistory:
		return m.keyHistory(msg)
	case viewRun:
		switch {
		case key.Matches(msg, keys.Back):
			return m.back(), nil
		case key.Matches(msg, keys.Run):
			rep, ok := m.rep(m.run.Replication)
			if !ok {
				return m, status(m.run.Replication+".toml not found", true)
			}
			var streams []string
			if m.run.Stream != "*" {
				streams = []string{m.run.Stream}
			}
			if m.planName != "" { // from a dry run: watch it on the detail view
				m.planName, m.planning = "", false
				m.repName, m.streamName, m.view = rep.Name, "", viewDetail
				m.cursor[viewDetail], m.offset[viewDetail] = 0, 0
			}
			return m.startRun(rep, streams, false)
		case key.Matches(msg, keys.Test) && m.planName != "" && !m.planning:
			rep, ok := m.rep(m.planName)
			if !ok {
				return m, status(m.planName+".toml not found", true)
			}
			m.planning, m.planSample, m.planStart = true, true, time.Now()
			return m, m.planNow(rep, true)
		case key.Matches(msg, keys.Edit):
			return m.editFromRun(m.run)
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case viewConns:
		return m.keyConns(msg)
	}
	return m, nil
}

// keyHelp pages and scrolls the help window.
func (m Model) keyHelp(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	h := *m.help
	pages := m.helpPages()
	step := max(m.bodyHeight()-3, 1)
	switch msg.String() {
	case "ctrl+c":
		m.help = nil
		return m.quit("")
	case "esc", "q", "?", "f1", "enter", "backspace":
		m.help = nil
		return m, nil
	case "left", "h", "shift+tab":
		h.page, h.offset = (h.page+len(pages)-1)%len(pages), 0
	case "right", "l", "tab":
		h.page, h.offset = (h.page+1)%len(pages), 0
	case "up", "k":
		h.offset--
	case "down", "j":
		h.offset++
	case "pgup":
		h.offset -= step
	case "pgdown", " ":
		h.offset += step
	case "g", "home":
		h.offset = 0
	case "G", "end":
		h.offset = 1 << 20
	}
	h.offset = min(max(h.offset, 0), max(len(pages[h.page].lines)-step, 0))
	m.help = &h
	return m, nil
}

func (m Model) keyList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	reps := m.visibleReps()
	cur, ok := at(reps, m.cursor[viewList])
	switch {
	case key.Matches(msg, keys.Enter) && ok:
		m.repName, m.streamName = cur.Name, ""
		m.view = viewDetail
		m.cursor[viewDetail] = 0
		m.offset[viewDetail] = 0
		return m, nil
	case key.Matches(msg, keys.Run) && ok:
		return m.startRun(cur, nil, false)
	case key.Matches(msg, keys.RunDetached) && ok:
		return m.startRun(cur, nil, true)
	case key.Matches(msg, keys.Test) && ok:
		return m.startPlan(cur)
	case key.Matches(msg, keys.Toggle) && ok:
		return m, tea.Sequence(m.toggleEnabled(cur), m.loadSnapshot())
	case key.Matches(msg, keys.Cancel) && ok:
		return m.askCancel(cur), nil
	case key.Matches(msg, keys.New):
		return m.newRep()
	case key.Matches(msg, keys.Edit) && ok:
		return m.editRep(cur.Name)
	case key.Matches(msg, keys.Duplicate) && ok && cur.Err == "":
		return m.dupRep(cur.Name)
	case key.Matches(msg, keys.Delete) && ok:
		m.ask = &confirm{text: "delete " + cur.Name + ".toml? y/n", onYes: func() tea.Cmd { return m.deleteRep(cur) }}
		return m, nil
	case key.Matches(msg, keys.Back):
		m.filter.SetValue("")
		m.clampCursor()
		return m, nil
	}
	return m, nil
}

func (m Model) keyDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rep, ok := m.rep(m.repName)
	if !ok {
		return m.back(), nil
	}
	names := rep.streamNames()
	stream, hasStream := at(names, m.cursor[viewDetail])
	_, inFile := rep.stream(stream)
	run, ran := rep.Last[stream]
	switch {
	case key.Matches(msg, keys.Back):
		return m.back(), nil
	case key.Matches(msg, keys.Enter) && hasStream:
		if ran && run.Status == "error" {
			return m.openRun(run), nil
		}
		if !ran || run.Object == "" {
			next, cmd := m.previewStream(rep, stream)
			if nm, ok := next.(Model); ok {
				if e, ok := nm.wiz.(*explorer); ok {
					e.markSample()
				}
			}
			return next, cmd
		}
		w, err := newTargetExplorer(m.opts, rep.Target, run.Object)
		return m.openWizard(w, err)
	case key.Matches(msg, keys.History) && hasStream:
		m.streamName = stream
		m.filter.SetValue("")
		m.view = viewHistory
		m.runs = nil
		m.cursor[viewHistory] = 0
		return m, m.loadRuns(m.repName, m.streamName)
	case key.Matches(msg, keys.Run):
		return m.startRun(rep, nil, false)
	case key.Matches(msg, keys.RunDetached):
		return m.startRun(rep, nil, true)
	case key.Matches(msg, keys.Test):
		return m.startPlan(rep)
	case key.Matches(msg, keys.RunStream) && hasStream && stream != "*":
		return m.startRun(rep, []string{stream}, false)
	case key.Matches(msg, keys.Toggle):
		switch {
		case !hasStream || (stream == "*" && !inFile):
			return m, tea.Sequence(m.toggleEnabled(rep), m.loadSnapshot())
		case inFile && rep.Err == "":
			return m, m.toggleStream(rep, stream)
		}
		return m, status(stream+": from a table pattern, not in "+rep.Name+".toml", true)
	case key.Matches(msg, keys.Delete) && hasStream && stream != "*":
		if !inFile || rep.Err != "" {
			return m, status(stream+": from a table pattern, not in "+rep.Name+".toml", true)
		}
		m.ask = &confirm{text: "delete stream " + stream + "? y/n", onYes: func() tea.Cmd { return m.deleteStream(rep, stream) }}
		return m, nil
	case key.Matches(msg, keys.Cancel):
		return m.askCancel(rep), nil
	case key.Matches(msg, keys.Edit) && rep.Err == "":
		if hasStream && inFile {
			return m.editStream(rep.Name, stream)
		}
		return m.editRep(rep.Name)
	case key.Matches(msg, keys.Add) && rep.Err == "":
		return m.addStream(rep.Name)
	case key.Matches(msg, keys.Preview) && hasStream:
		return m.previewStream(rep, stream)
	}
	return m, nil
}

// askCancel asks before stopping the running streams of a replication.
func (m Model) askCancel(rep replication) Model {
	var ids []int64
	for _, r := range rep.Last {
		if r.Status == "running" {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) > 0 {
		m.ask = &confirm{text: "cancel the run of " + rep.Name + "? y/n", onYes: func() tea.Cmd { return m.cancelRuns(rep.Name, ids) }}
	}
	return m
}

// openRun shows one run; esc goes back to the current view.
// startPlan opens the run page at once, resolving shown until the plan arrives.
func (m Model) startPlan(rep replication) (tea.Model, tea.Cmd) {
	m.run, m.runFrom, m.planName = state.Run{Replication: rep.Name, Stream: "*"}, m.view, rep.Name
	m.planning, m.planSample, m.planStart = true, false, time.Now()
	m.view = viewRun
	m.vp.Width = max(m.width-4, 10)
	m.vp.Height = max(m.bodyHeight()-2, 3)
	m.vp.SetContent("")
	return m, m.planNow(rep, false)
}

// openPlan fills the run page with a dry run; r there runs it for real.
func (m Model) openPlan(p planMsg) Model {
	m.planning = false
	m.vp.SetContent(planText(p.Rows, m.vp.Width))
	m.vp.GotoTop()
	return m
}

func (m Model) openRun(r state.Run) Model {
	m.run, m.runFrom, m.planName = r, m.view, ""
	m.view = viewRun
	m.vp.Width = max(m.width-4, 10)
	m.vp.Height = max(m.bodyHeight()-2, 3)
	m.vp.SetContent(runText(r, m.vp.Width))
	m.vp.GotoTop()
	return m
}

// editFromRun opens what a failed run points at: its stream, else the connection named
// in the error, else the replication.
func (m Model) editFromRun(r state.Run) (tea.Model, tea.Cmd) {
	rep, ok := m.rep(r.Replication)
	if !ok {
		return m, status(r.Replication+".toml not found", true)
	}
	if _, inFile := rep.stream(r.Stream); inFile && rep.Err == "" {
		return m.editStream(rep.Name, r.Stream)
	}
	if name := connInError(r.Error); name != "" {
		for _, c := range m.snap.Conns {
			if c.Name == name {
				return m.editConn(name)
			}
		}
	}
	return m.editRep(rep.Name)
}

// connInError is NAME of an error starting "conn NAME:".
func connInError(s string) string {
	rest, ok := strings.CutPrefix(s, "conn ")
	if !ok {
		return ""
	}
	name, _, ok := strings.Cut(rest, ":")
	if !ok || !config.ValidName(name) {
		return ""
	}
	return name
}

// previewStream opens the source preview for a stream; a wildcard opens the filtered table list.
func (m Model) previewStream(rep replication, stream string) (tea.Model, tea.Cmd) {
	spec, inFile := rep.stream(stream)
	if !inFile {
		spec = config.ResolvedStream{Name: stream, Stream: config.Stream{Mode: rep.Mode}}
	}
	if spec.IsWildcard() {
		w, err := newWildcardExplorer(m.opts, rep.Name, rep.Source, spec)
		return m.openWizard(w, err)
	}
	w, err := newRepStreamExplorer(m.opts, rep.Name, rep.Source, spec)
	return m.openWizard(w, err)
}

func (m Model) keyHistory(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	runs := m.visibleRuns()
	cur, ok := at(runs, m.cursor[viewHistory])
	switch {
	case key.Matches(msg, keys.Back):
		return m.back(), nil
	case key.Matches(msg, keys.Enter) && ok:
		return m.openRun(cur), nil
	}
	return m, nil
}

func (m Model) keyConns(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	conns := m.visibleConns()
	cur, ok := at(conns, m.cursor[viewConns])
	switch {
	case key.Matches(msg, keys.Back):
		return m.back(), nil
	case key.Matches(msg, keys.Test) && ok:
		if m.testing[cur.Name] {
			return m, nil
		}
		m.testing[cur.Name] = true
		return m, m.testConn(cur.Name)
	case key.Matches(msg, keys.Enter) && ok:
		if path := m.missingFile(cur.Name); path != "" {
			m.connResults[cur.Name] = connTestMsg{Name: cur.Name, Err: errors.New("conn " + cur.Name + ": no such file: " + path)}
			return m, nil
		}
		return m.explore(cur.Name)
	case key.Matches(msg, keys.Create) && ok && m.missingFile(cur.Name) != "":
		m.testing[cur.Name] = true
		return m, m.createFile(cur.Name, m.missingFile(cur.Name))
	case key.Matches(msg, keys.New):
		return m.newConn()
	case key.Matches(msg, keys.Edit) && ok:
		return m.editConn(cur.Name)
	case key.Matches(msg, keys.Delete) && ok:
		name := cur.Name
		text := "delete connection " + name + "? y/n"
		if users := usersOf(m.opts.Home, name); len(users) > 0 {
			text = "delete connection " + name + "? used by " + strings.Join(users, ", ") + "  y/n"
		}
		m.ask = &confirm{text: text, onYes: func() tea.Cmd { return m.deleteConn(name) }}
		return m, nil
	}
	return m, nil
}

// startRun runs rep in this process, or detached as its own process that outlives
// the TUI (ctrl+r) when Options.RunCommand is set.
func (m Model) startRun(rep replication, streams []string, detached bool) (tea.Model, tea.Cmd) {
	if m.inflight[rep.Name] {
		return m, status(rep.Name+": running", true)
	}
	if rep.Err != "" || rep.ConnErr != "" { // refused before anything is marked
		return m, m.runNow(rep, streams, detached)
	}
	if detached && len(m.opts.RunCommand) == 0 {
		return m, status("detached runs need the tummy command", true)
	}
	if !m.daemonServes() {
		m.inflight[rep.Name] = true
	}
	return m, m.runNow(rep, streams, detached)
}

func (m Model) back() Model {
	switch m.view {
	case viewDetail, viewConns:
		m.view = viewList
	case viewHistory:
		m.view = viewList
		if m.repName != "" {
			m.view = viewDetail
		}
	case viewRun:
		m.view = cmp.Or(m.runFrom, viewHistory)
		if m.planName != "" {
			m.view = m.runFrom // the list is the zero view
		}
		m.planName, m.planning = "", false
	}
	m.filter.SetValue("")
	m.clampCursor()
	return m
}

func (m *Model) move(delta int) {
	m.cursor[m.view] += delta
	m.clampCursor()
}

func (m *Model) clampCursor() {
	n := m.rowCount()
	c := min(m.cursor[m.view], n-1)
	c = max(c, 0)
	m.cursor[m.view] = c
	rows := max(m.listRows(), 1)
	off := m.offset[m.view]
	if c < off {
		off = c
	}
	if c >= off+rows {
		off = c - rows + 1
	}
	m.offset[m.view] = max(min(off, n-rows), 0)
}

// listRows is how many table rows fit under the table header of the current view.
func (m Model) listRows() int {
	rows := m.bodyHeight() - 1
	if m.view == viewDetail {
		rows -= 2 // replication line and blank
		if rep, ok := m.rep(m.repName); ok && rep.SchedErr != "" {
			rows--
		}
		if m.height >= 18 {
			rows -= detailPanel
		}
	}
	return rows
}

func (m Model) rowCount() int {
	switch m.view {
	case viewList:
		return len(m.visibleReps())
	case viewDetail:
		if rep, ok := m.rep(m.repName); ok {
			return len(rep.streamNames())
		}
	case viewHistory:
		return len(m.visibleRuns())
	case viewConns:
		return len(m.visibleConns())
	}
	return 0
}

func (m Model) rep(name string) (replication, bool) {
	for _, r := range m.snap.Reps {
		if r.Name == name {
			return r, true
		}
	}
	return replication{}, false
}

func (m Model) visibleReps() []replication {
	f := strings.ToLower(m.filter.Value())
	if f == "" {
		return m.snap.Reps
	}
	var out []replication
	for _, r := range m.snap.Reps {
		if strings.Contains(strings.ToLower(r.Name+" "+r.Source+" "+r.Target), f) {
			out = append(out, r)
		}
	}
	return out
}

func (m Model) visibleRuns() []state.Run {
	f := strings.ToLower(m.filter.Value())
	if f == "" {
		return m.runs
	}
	var out []state.Run
	for _, r := range m.runs {
		if strings.Contains(strings.ToLower(r.Replication+" "+r.Stream+" "+r.Object+" "+r.Status+" "+r.Error), f) {
			out = append(out, r)
		}
	}
	return out
}

func (m Model) visibleConns() []connRow {
	f := strings.ToLower(m.filter.Value())
	if f == "" {
		return m.snap.Conns
	}
	var out []connRow
	for _, c := range m.snap.Conns {
		if strings.Contains(strings.ToLower(c.Name+" "+c.Type+" "+c.Target), f) {
			out = append(out, c)
		}
	}
	return out
}

func at[T any](xs []T, i int) (T, bool) {
	var zero T
	if i < 0 || i >= len(xs) {
		return zero, false
	}
	return xs[i], true
}
