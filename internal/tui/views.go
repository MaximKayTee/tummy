package tui

import (
	"cmp"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"tummy/internal/engine"
	"tummy/internal/txt"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/state"
)

const (
	headerLines = 2
	detailPanel = 8 // lines under the stream table for the selected stream's error and definition
)

// bodyHeight is what is left between the header and the footer, which takes one or two
// lines of keys under its rule.
func (m Model) bodyHeight() int { return max(m.height-headerLines-len(m.footer()), 3) }

func (m Model) View() string {
	var body []string
	switch {
	case m.help != nil:
		body = m.helpBody()
	case m.view == viewDetail:
		body = m.detailView()
	default:
		switch m.view {
		case viewList:
			body = m.listView()
		case viewHistory:
			body = m.historyView()
		case viewRun:
			body = m.runView()
		case viewConns:
			body = m.connsView()
		case viewWizard:
			if m.wiz != nil {
				body = m.wiz.view(m.width, m.bodyHeight())
			}
		}
		body = window(body, m.offset[m.view], m.bodyHeight(), m.view != viewRun && m.view != viewWizard)
	}
	if len(body) > m.bodyHeight() {
		body = body[:m.bodyHeight()]
	}
	for len(body) < m.bodyHeight() {
		body = append(body, "")
	}
	for i, l := range body {
		body[i] = ansi.Truncate(l, m.width, "…")
	}
	return strings.Join(append(append(m.header(), body...), m.footer()...), "\n")
}

// window keeps the first line (table header) and scrolls the rest.
func window(lines []string, offset, height int, keepFirst bool) []string {
	if len(lines) <= height {
		return lines
	}
	if !keepFirst {
		offset = min(max(offset, 0), len(lines)-height)
		return lines[offset : offset+height]
	}
	head, rest := lines[:1], lines[1:]
	offset = min(max(offset, 0), max(len(rest)-(height-1), 0))
	end := min(offset+height-1, len(rest))
	return append(head, rest[offset:end]...)
}

// ---------- header and footer ----------

func (m Model) header() []string {
	var crumbs string
	crumb := func(s string) { crumbs += sDim.Render("  ›  ") + sText.Render(txt.Clean(s)) }
	switch m.view {
	case viewList:
		crumb("replications")
	case viewDetail:
		crumb(m.repName)
	case viewHistory:
		crumb("history")
		if m.repName != "" {
			crumbs += sDim.Render("  ·  ") + sText.Render(txt.Clean(m.repName))
		}
		if m.streamName != "" {
			crumbs += sDim.Render("  ·  ") + sText.Render(txt.Clean(m.streamName))
		}
	case viewRun:
		if m.planName != "" {
			crumb("dry run " + txt.Clean(m.planName))
		} else {
			crumb(fmt.Sprintf("run #%d", m.run.ID))
		}
	case viewConns:
		crumb("connections")
	case viewWizard:
		if m.wiz != nil {
			crumb(m.wiz.title())
		}
	}
	if m.help != nil {
		crumb("help")
	}
	right := m.daemonBadge()
	room := m.width - lipgloss.Width(right) - 2
	title := sTitle.Render("tummy")
	// the view name wins over the config path: the path is cut first, from the left
	home := txt.Clean(shortHome(m.opts.Home))
	if pathRoom := room - lipgloss.Width(title) - 2 - lipgloss.Width(crumbs); lipgloss.Width(home) > pathRoom {
		if pathRoom < 8 {
			home = ""
		} else {
			home = "…" + ansi.TruncateLeft(home, lipgloss.Width(home)-pathRoom+1, "")
		}
	}
	left := title
	if home != "" {
		left += "  " + sDim.Render(home)
	}
	left += crumbs
	if lipgloss.Width(left) > room {
		left = ansi.Truncate(left, max(room, 10), "…")
	}
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return []string{ansi.Truncate(left+strings.Repeat(" ", gap)+right, m.width, ""), rule(m.width)}
}

func (m Model) daemonBadge() string {
	d := m.snap.Daemon
	switch {
	case !m.loaded:
		return sDim.Render("loading")
	case d.Alive() && d.Serves(m.opts.Home):
		return sOK.Render("● daemon") + sDim.Render(fmt.Sprintf(" pid %d", d.PID))
	case d.Alive():
		return sWarn.Render("● daemon") + sDim.Render(" serves "+txt.Clean(shortHome(d.Home)))
	case d != nil:
		return sErr.Render("○ daemon") + sDim.Render(" last seen "+ago(d.Heartbeat))
	}
	return sDim.Render("○ no daemon")
}

// statusFor is how long a status line replaces the key hints.
const statusFor = 4 * time.Second

func (m Model) footer() []string {
	var left, right string
	recent := time.Since(m.statusAt) < statusFor && m.status.Text != ""
	statusLine := func() string {
		if m.status.IsErr {
			return sErr.Render(txt.Clean(m.status.Text))
		}
		return sStatus.Render(txt.Clean(m.status.Text))
	}
	text := false // left is a sentence, not keys
	switch {
	case m.ask != nil:
		left, text = sWarn.Render(txt.Clean(m.ask.text)), true
	case m.help != nil:
		left, right = helpLine("←/→", "page", "↑/↓ pgup/pgdn", "scroll"), helpLine("esc", "close")
	case m.view == viewWizard && m.wiz != nil:
		if f, ok := m.wiz.(footerer); ok {
			left, right = f.footer()
		} else {
			left = m.wiz.help()
		}
		left, right = escRight(left, right)
		right = strings.TrimSpace(helpLine("f1", "help") + "  " + right)
		if recent {
			left, text = statusLine(), true
		}
	case m.filtering:
		left, right = m.filter.View(), helpLine("enter", "apply", "esc", "clear")
	default:
		left = m.shortHelp()
		if recent {
			left, text = statusLine(), true
		}
		if m.view == viewList {
			right = helpLine("?", "help", "q", "quit")
		} else {
			right = helpLine("?", "help", "esc", "back")
		}
	}
	if text {
		return append([]string{rule(m.width)}, wrapFooter(left, right, m.width)...)
	}
	return append([]string{rule(m.width)}, footerKeys(left, right, m.width)...)
}

// keySep joins the pairs of helpLine.
const keySep = "  "

// escRight moves the esc pair of left to the right part, which is never dropped.
func escRight(left, right string) (string, string) {
	if strings.Contains(ansi.Strip(right), "esc ") {
		return left, right
	}
	var keep []string
	for _, it := range strings.Split(left, keySep) {
		if strings.HasPrefix(ansi.Strip(it), "esc ") && right == "" {
			right = it
			continue
		}
		keep = append(keep, it)
	}
	return strings.Join(keep, keySep), right
}

// footerKeys lays out key pairs: one line when they fit, else two, the right part on the
// first. Pairs that fit nowhere are left out whole, never cut.
func footerKeys(left, right string, width int) []string {
	rw := lipgloss.Width(right)
	if left == "" || 1+lipgloss.Width(left)+2+rw <= width {
		return []string{joinFooter(left, right, width)}
	}
	items := strings.Split(left, keySep)
	take := func(room int) string {
		var line string
		for len(items) > 0 {
			next := items[0]
			if line != "" {
				next = line + keySep + items[0]
			}
			if lipgloss.Width(next) > room {
				break
			}
			line, items = next, items[1:]
		}
		return line
	}
	first := take(width - 1 - rw - 2)
	second := take(width - 1)
	if first == "" && second == "" {
		return []string{joinFooter(left, right, width)}
	}
	gap := max(width-1-lipgloss.Width(first)-rw, 2)
	line1 := ansi.Truncate(" "+first+strings.Repeat(" ", gap)+right, width, "")
	if second == "" {
		return []string{line1}
	}
	return []string{line1, ansi.Truncate(" "+second, width, "")}
}

// wrapFooter fits a sentence on up to two lines beside the right part, at word breaks.
func wrapFooter(left, right string, width int) []string {
	rw := lipgloss.Width(right)
	room := width - 1
	if right != "" {
		room = width - rw - 4
	}
	if room < 10 || lipgloss.Width(left) <= room {
		return []string{joinFooter(left, right, width)}
	}
	lines := strings.Split(ansi.Wrap(left, room, " "), "\n")
	first := lines[0]
	gap := max(width-1-lipgloss.Width(first)-rw, 2)
	out := []string{ansi.Truncate(" "+first+strings.Repeat(" ", gap)+right, width, "")}
	if len(lines) > 1 {
		out = append(out, ansi.Truncate(" "+strings.Join(lines[1:], " "), width, "…"))
	}
	return out
}

// joinFooter fits left and right on one line; only left is ever cut.
func joinFooter(left, right string, width int) string {
	if right == "" {
		return ansi.Truncate(" "+left, width, "…")
	}
	rw := lipgloss.Width(right)
	room := width - rw - 4
	if room < 4 {
		return ansi.Truncate(" "+right, width, "")
	}
	left = ansi.Truncate(left, room, "…")
	gap := max(width-1-lipgloss.Width(left)-rw, 2)
	return ansi.Truncate(" "+left+strings.Repeat(" ", gap)+right, width, "")
}

func (m Model) shortHelp() string {
	switch m.view {
	case viewList:
		return helpLine("enter", "open", "n", "new", "E", "edit", "r", "run", "t", "dry run", "e", "on/off", "d", "delete", "c", "connections", "H", "history", "/", "filter")
	case viewDetail:
		pairs := []string{"E", "edit", "a", "add", "r", "run", "t", "dry run", "s", "run stream", "e", "on/off", "d", "delete", "enter", m.detailEnterWord(), "p", "preview", "H", "history"}
		if rep, ok := m.rep(m.repName); ok && rep.Running > 0 {
			pairs = append(pairs, "x", "cancel run")
		}
		return helpLine(pairs...)
	case viewHistory:
		return helpLine("enter", "details", "/", "filter")
	case viewRun:
		if m.planning {
			return helpLine("esc", "back")
		}
		if m.planName != "" {
			return helpLine("r", "run", "t", fmt.Sprintf("test load %d rows", engine.SampleRows), "↑/↓ pgup/pgdn", "scroll")
		}
		return helpLine("r", "run again", "E", "edit", "↑/↓ pgup/pgdn", "scroll")
	case viewConns:
		pairs := []string{"enter", "explore", "n", "new", "E", "edit", "t", "test", "d", "delete", "/", "filter"}
		if c, ok := at(m.visibleConns(), m.cursor[viewConns]); ok && m.missingFile(c.Name) != "" {
			pairs = append([]string{"c", "create file"}, pairs...)
		}
		return helpLine(pairs...)
	}
	return ""
}

// detailEnterWord is what enter does on the stream under the cursor.
func (m Model) detailEnterWord() string {
	rep, ok := m.rep(m.repName)
	if !ok {
		return "data"
	}
	name, ok := at(rep.streamNames(), m.cursor[viewDetail])
	if !ok {
		return "data"
	}
	if run, ran := rep.Last[name]; ran && run.Status == "error" {
		return "error"
	}
	return "data"
}

// ---------- help ----------

type helpPage struct {
	title string
	lines []string
}

func helpRows(rows [][2]string) []string {
	w := 0
	for _, r := range rows {
		w = max(w, lipgloss.Width(r[0]))
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = "  " + sKey.Render(pad(r[0], w, false)) + "  " + r[1]
	}
	return out
}

func (m Model) helpPages() []helpPage {
	var keys []string
	section := func(title string, rows [][2]string) {
		if len(rows) == 0 {
			return
		}
		if len(keys) > 0 {
			keys = append(keys, "")
		}
		keys = append(keys, " "+sHeader.Render(title))
		keys = append(keys, helpRows(rows)...)
	}
	switch m.view {
	case viewWizard:
		if kh, ok := m.wiz.(keyHelper); ok {
			section(kh.keyHelp())
		}
		section("anywhere", [][2]string{{"f1", "help"}, {"?", "help, when not typing"}, {"ctrl+c", "quit; asks when there are changes or runs"}, {"ctrl+z", "suspend; fg resumes"}})
	case viewList:
		section("replications", [][2]string{
			{"enter", "streams of the replication"}, {"n", "new replication"}, {"E", "edit"}, {"e", "on/off for the daemon"},
			{"d", "delete the file (y/n)"}, {"r", "run now"}, {"t", "dry run: target table, action, source query and columns per stream, nothing written"}, {"x", "cancel a running run (y/n)"}, {"H", "run history"}, {"c", "connections"},
		})
	case viewDetail:
		section("streams", [][2]string{
			{"enter", "loaded data in the target; a failed stream: its run"}, {"p", "source preview"}, {"E", "edit the stream"}, {"a", "add tables or an endpoint"},
			{"e", "stream on/off; (replication): the replication"}, {"d", "delete the stream (y/n)"}, {"r", "run every stream"}, {"s", "run the stream"},
			{"x", "cancel the run (y/n)"}, {"H", "history of the stream"},
		})
	case viewHistory:
		section("history", [][2]string{{"enter", "run details"}, {"/", "filter"}})
	case viewRun:
		if m.planName != "" {
			section("dry run", [][2]string{{"r", "run it"}, {"t", fmt.Sprintf("test load: the first %d rows per stream into a copy of the run's temp table, rolled back", engine.SampleRows)}, {"↑/↓ pgup/pgdn", "scroll"}})
			break
		}
		section("run", [][2]string{{"r", "run again"}, {"E", "edit the stream, else the connection in the error"}, {"↑/↓ pgup/pgdn", "scroll"}})
	case viewConns:
		section("connections", [][2]string{
			{"enter", "explore tables"}, {"n", "new connection"}, {"E", "edit"}, {"t", "test"},
			{"d", "delete (y/n)"}, {"c", "create a missing sqlite file"},
		})
	}
	if m.view != viewWizard {
		section("move", [][2]string{
			{"↑/↓  k/j", "move"}, {"pgup/pgdn", "page"}, {"g/G", "top / bottom"}, {"enter  →  l", "open"},
			{"esc  ←  h  backspace", "back"}, {"/", "filter"}, {"R", "refresh"}, {"q", "back; quit on the list"},
		})
	}
	keys = append(keys, "",
		" "+sHeader.Render("files"),
		"  "+sDim.Render("config       ")+txt.Clean(shortHome(m.opts.Home)),
		"  "+sDim.Render("connections  ")+txt.Clean(shortHome(m.opts.Connections)),
		"  "+sDim.Render("state        ")+txt.Clean(shortHome(m.opts.State.Path)))

	forms := []string{" " + sHeader.Render("fields")}
	forms = append(forms, helpRows([][2]string{
		{"tab  ↓", "next field"}, {"shift+tab  ↑", "previous field"}, {"enter", "next field; open a list; on the last field: apply"},
		{"pgup/pgdn", "five fields"}, {"←/→", "cycle a list's value"}, {"space", "on/off; open a list"},
		{"type", "on a list: open it filtered"}, {"ctrl+s", "save"}, {"esc", "back"},
	})...)
	forms = append(forms, "", " "+sHeader.Render("open list"))
	forms = append(forms, helpRows([][2]string{
		{"type", "filter"}, {"↑/↓  pgup/pgdn", "move"}, {"home/end", "first / last"}, {"space", "tick"},
		{"ctrl+a / ctrl+n", "tick all / none shown"}, {"enter", "pick; done"}, {"esc", "close, no change"},
		{"new", "typed value"},
	})...)

	sched := []string{" " + sHeader.Render("schedule") + "  " + sDim.Render("minute  hour  day  month  weekday, UTC")}
	sched = append(sched, helpRows([][2]string{
		{"*/15 * * * *", "every 15 min"}, {"0 * * * *", "hourly"}, {"30 3 * * *", "03:30 UTC daily"},
		{"0 6 * * 1-5", "06:00 UTC Mon-Fri"}, {"0 0 1 * *", "00:00 UTC on the 1st"}, {"@every 10m", "every 10 min"},
		{"@hourly  @daily", "0 * * * *   0 0 * * *"}, {"CRON_TZ=Europe/Berlin 0 6 * * *", "06:00 Berlin time"},
	})...)
	sched = append(sched, "", "  "+sDim.Render("*  any    */n  every n    a-b  range    a,b  list    weekday 0-6, 0 Sunday"))
	return []helpPage{{"keys", keys}, {"forms", forms}, {"schedule", sched}}
}

func (m Model) helpBody() []string {
	pages := m.helpPages()
	h := *m.help
	h.page = min(h.page, len(pages)-1)
	var tabs []string
	for i, p := range pages {
		if i == h.page {
			tabs = append(tabs, sKey.Render(p.title))
		} else {
			tabs = append(tabs, sDim.Render(p.title))
		}
	}
	out := []string{" " + strings.Join(tabs, sDim.Render("  ·  ")), ""}
	lines := pages[h.page].lines
	rows := max(m.bodyHeight()-len(out), 1)
	off := min(h.offset, max(len(lines)-rows, 0))
	end := min(off+rows, len(lines))
	if end < len(lines) {
		end = max(end-1, off)
		return append(append(out, lines[off:end]...), "  "+sDim.Render(fmt.Sprintf("↓ %d more", len(lines)-end)))
	}
	return append(out, lines[off:end]...)
}

// ---------- replication list ----------

func (m Model) listView() []string {
	reps := m.visibleReps()
	if m.snap.Err != nil {
		return []string{"", sErr.Render("  " + txt.Clean(m.snap.Err.Error()))}
	}
	if m.loaded && len(reps) == 0 {
		return []string{"", sDim.Render("  none")}
	}
	wide := m.width >= 100
	srcW, tgtW := 6, 6
	for _, r := range reps {
		srcW = max(srcW, lipgloss.Width(txt.Clean(r.Source)))
		tgtW = max(tgtW, lipgloss.Width(txt.Clean(r.Target)))
	}
	cols := []column{{Title: "REPLICATION"}, {Title: "SOURCE", Width: min(srcW, 18)}, {Title: "TARGET", Width: min(tgtW, 18)}, {Title: "STREAMS", Width: 7, Right: true},
		{Title: "SCHEDULE", Width: 13}, {Title: "LAST RUN"}, {Title: "NEXT", Width: 11}}
	if !wide {
		cols = []column{{Title: "REPLICATION"}, {Title: "STREAMS", Width: 7, Right: true}, {Title: "LAST RUN"}, {Title: "NEXT", Width: 9}}
	}
	var rows [][]string
	for _, r := range reps {
		name := txt.Clean(r.Name)
		switch {
		case r.Err != "" || r.ConnErr != "":
			name = sErr.Render(name)
		case !r.Enabled:
			name = sDim.Render(name + " (off)")
		}
		row := []string{name}
		if wide {
			row = append(row, txt.Clean(r.Source), txt.Clean(r.Target))
		}
		row = append(row, strconv.Itoa(len(r.Streams)))
		if wide {
			row = append(row, txt.Clean(orDash(r.Schedule)))
		}
		row = append(row, m.lastRunCell(r), m.nextCell(r))
		rows = append(rows, row)
	}
	return table(m.width, cols, rows, m.cursor[viewList])
}

func (m Model) lastRunCell(r replication) string {
	switch {
	case r.Err != "":
		return sErr.Render(glyphErr + " " + txt.Clean(txt.FirstLine(r.Err)))
	case r.ConnErr != "":
		return sErr.Render(glyphErr + " " + txt.Clean(r.ConnErr))
	case m.inflight[r.Name] || r.Running > 0:
		return sWarn.Render(m.spin.View()+" "+runningLabel(r)) + sDim.Render("  "+txt.Thousands(r.Rows)+" rows")
	case r.Pending > 0 && m.daemonServes():
		return sWarn.Render(glyphQueued + " queued")
	case len(r.Last) == 0:
		return sDim.Render(glyphNever + " never")
	case r.Failed > 0 && r.Last["*"].Status == "error":
		return sErr.Render(glyphErr+" failed") + sDim.Render("  "+ago(r.LastAt))
	case r.Failed > 0:
		return sErr.Render(fmt.Sprintf("%s %s failed", glyphErr, plural(r.Failed, "stream"))) + sDim.Render("  "+ago(r.LastAt))
	case r.Stopped > 0:
		return sWarn.Render(glyphSkip+" cancelled") + sDim.Render("  "+ago(r.LastAt))
	case r.Skipped > 0 && r.OK == 0:
		return sWarn.Render(glyphSkip+" skipped") + sDim.Render("  "+ago(r.LastAt))
	}
	return sOK.Render(glyphOK+" ok") + sDim.Render("  "+txt.Thousands(r.Rows)+" rows  "+ago(r.LastAt))
}

func (m Model) nextCell(r replication) string {
	switch {
	case r.Err != "":
		return sDim.Render("—")
	case r.SchedErr != "":
		return sErr.Render(glyphErr + " " + txt.Clean(txt.FirstLine(r.SchedErr)))
	case !r.Enabled:
		return sDim.Render("off")
	case r.Schedule == "":
		return sDim.Render("—")
	case !m.daemonServes():
		return sDim.Render("no daemon")
	}
	return in(r.Next)
}

// ---------- replication detail ----------

func (m Model) detailView() []string {
	rep, ok := m.rep(m.repName)
	if !ok {
		return []string{"", sDim.Render("  " + txt.Clean(m.repName) + ".toml not found")}
	}
	if rep.Err != "" {
		return append([]string{"", " " + sDim.Render(txt.Clean(shortHome(rep.Path))), ""},
			strings.Split(sErrBox.Width(max(m.width-4, 20)).Render(txt.CleanText(rep.Err)), "\n")...)
	}
	meta := sDim.Render("source ") + sText.Render(txt.Clean(rep.Source)) + sDim.Render("   target ") + sText.Render(txt.Clean(rep.Target)) +
		sDim.Render("   mode ") + sText.Render(txt.Clean(cmp.Or(rep.Mode, "full-refresh")))
	if rep.Schedule != "" {
		meta += "   " + sText.Render(txt.Clean(rep.Schedule))
	}
	if rep.Enabled {
		meta += "   " + sBadgeOn.Render("enabled")
	} else {
		meta += "   " + sBadgeOff.Render("disabled")
	}
	lines := []string{" " + meta}
	if rep.ConnErr != "" {
		lines = append(lines, " "+sErr.Render(glyphErr+" "+txt.Clean(rep.ConnErr)))
	}
	if rep.SchedErr != "" {
		lines = append(lines, " "+sErr.Render(glyphErr+" daemon: "+txt.Clean(txt.FirstLine(rep.SchedErr))))
	}
	lines = append(lines, "")

	wide := m.width >= 100
	cols := []column{{Title: "STREAM"}, {Title: "OBJECT"}, {Title: "MODE", Width: 12},
		{Title: "LAST RUN", Width: 11}, {Title: "TIME", Width: 7, Right: true}, {Title: "ROWS", Width: 9, Right: true}, {Title: "STATUS"}}
	if !wide {
		cols = []column{{Title: "STREAM"}, {Title: "MODE", Width: 12}, {Title: "LAST RUN", Width: 10}, {Title: "ROWS", Width: 8, Right: true}, {Title: "STATUS"}}
	}
	var rows [][]string
	for _, name := range rep.streamNames() {
		spec, inFile := rep.stream(name)
		run, ran := rep.Last[name]
		label := txt.Clean(name)
		object, mode := "", rep.Mode
		switch {
		case name == "*" && !inFile:
			label = sErr.Render(replicationRow)
			if run.Status != "error" {
				label = sWarn.Render(replicationRow)
			}
		case inFile && spec.IsWildcard():
			row := []string{sDim.Render(label), sDim.Render("table pattern"), sDim.Render(txt.Clean(spec.Mode))}
			if !wide {
				row = []string{sDim.Render(label), sDim.Render(txt.Clean(spec.Mode)), "", "", sDim.Render("table pattern")}
			}
			rows = append(rows, row)
			continue
		case !inFile:
			label = sDim.Render(label)
		case spec.Disabled:
			label = sDim.Render(label + " (off)")
			object, mode = m.objectOf(rep, spec), spec.Mode
		default:
			object, mode = m.objectOf(rep, spec), spec.Mode
		}
		if ran && run.Object != "" {
			object = run.Object
		}
		row := []string{label}
		if wide {
			row = append(row, sDim.Render(txt.Clean(orDash(object))))
		}
		row = append(row, txt.Clean(mode))
		switch {
		case ran && wide:
			row = append(row, ago(run.Started), txt.Dur(run.Duration()), txt.Thousands(run.Rows), m.statusCell(run))
		case ran:
			row = append(row, ago(run.Started), txt.Thousands(run.Rows), m.statusCell(run))
		default:
			row = append(row, sDim.Render("never"))
		}
		rows = append(rows, row)
	}
	var panel []string
	if m.height >= 18 {
		if name, ok := at(rep.streamNames(), m.cursor[viewDetail]); ok {
			var def []string
			if run, ran := rep.Last[name]; ran && (run.Status == "error" || run.Status == "skipped" || run.Status == "cancelled") && run.Error != "" {
				box := sErrBox
				if run.Status != "error" {
					box = box.BorderForeground(colWarn)
				}
				def = strings.Split(box.Width(max(m.width-4, 20)).Render(txt.CleanText(run.Error)), "\n")
				if len(def) > 5 {
					def = append(def[:4], sErr.Render(" …  enter: the whole error"))
				}
			}
			if spec, ok := rep.stream(name); ok {
				def = append(def, m.definition(spec, rep.Source)...)
			}
			if len(def) > detailPanel-2 {
				def = append(def[:detailPanel-3], " "+sDim.Render("…"))
			}
			if len(def) > 0 {
				panel = append([]string{"", rule(m.width)}, def...)
			}
		}
	}
	tbl := table(m.width, cols, rows, m.cursor[viewDetail])
	room := max(m.bodyHeight()-len(lines)-len(panel), 2)
	return append(append(lines, window(tbl, m.offset[viewDetail], room, true)...), panel...)
}

// replicationRow names the "*" row: what happened to the replication as a whole.
const replicationRow = "(replication)"

// streamLabel is a stream name as shown, "*" as the replication row.
func streamLabel(s string) string {
	if s == "*" {
		return replicationRow
	}
	return s
}

// objectOf is the target table a stream loads, as the engine will name it.
func (m Model) objectOf(rep replication, s config.ResolvedStream) string {
	obj := s.Object
	var target connRow
	for _, c := range m.snap.Conns {
		if c.Name == rep.Target {
			target = c
		}
	}
	schemas := target.Type != "sqlite"
	t := db.ParseTable(s.Name)
	if obj == "" {
		switch {
		case s.SQL != "" || s.Path != "":
			obj = s.Name
		case !schemas && t.Schema != "":
			obj = "{stream_schema}_{stream_table}"
		case t.Schema == "":
			obj = "{stream_table}"
		default:
			obj = "{stream_schema}.{stream_table}"
		}
	}
	return strings.NewReplacer(
		"{stream_schema}", t.Schema,
		"{stream_table}", t.Name,
		"{stream_name}", strings.ReplaceAll(s.Name, ".", "_"),
		"{target_schema}", target.Schema,
	).Replace(obj)
}

// definition shows what a stream reads: table or sql, steps, filters, or the endpoint.
func (m Model) definition(s config.ResolvedStream, source string) []string {
	var out []string
	row := func(k, v string) {
		if v != "" {
			out = append(out, " "+sDim.Render(pad(k, 8, false))+" "+ansi.Truncate(txt.Clean(v), max(m.width-11, 10), "…"))
		}
	}
	num := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	switch {
	case s.Path != "":
		var conn connRow
		for _, c := range m.snap.Conns {
			if c.Name == source {
				conn = c
			}
		}
		row("url", strings.TrimRight(conn.Target, "/")+s.Path)
		if s.Method != "" && !strings.EqualFold(s.Method, "GET") {
			row("method", s.Method)
		}
		var q []string
		for k, v := range s.Query {
			q = append(q, k+"="+v)
		}
		sort.Strings(q)
		row("query", strings.Join(q, "  "))
		row("records", s.Records)
		switch s.Paginate {
		case "cursor":
			row("paginate", "cursor  "+s.CursorField+" → "+cmp.Or(s.CursorParam, "cursor"))
		case "page", "offset":
			row("paginate", s.Paginate+"  "+cmp.Or(s.PageParam, s.Paginate)+"  "+cmp.Or(s.SizeParam, "limit")+"="+num(s.PageSize))
		case "link":
			row("paginate", "link header")
		}
		if s.PageSize > 0 && s.Paginate == "cursor" {
			row("page", num(s.PageSize))
		}
		row("limit", num(s.Limit))
		row("for_each", s.ForEach)
		if s.SinceParam != "" {
			row("since", s.SinceParam+"  "+cmp.Or(s.SinceFormat, "rfc3339")+"  "+s.Sorted)
		}
		row("rate", conn.RateLimit)
	case s.SQL != "":
		for i, l := range strings.Split(strings.TrimSpace(s.SQL), "\n") {
			k := "sql"
			if i > 0 {
				k = ""
			}
			row(k, l)
		}
	default:
		row("table", s.Name)
		row("columns", strings.Join(s.Select, ", "))
		row("limit", num(s.Limit))
	}
	row("where", s.Where)
	row("steps", s.Steps)
	if s.Range != "" {
		row("range", s.UpdateKey+"  "+s.Range)
	}
	return out
}

func (m Model) statusCell(r state.Run) string {
	switch r.Status {
	case "running":
		return sWarn.Render(m.spin.View() + " " + phaseLabel(r.Phase, r.Rows))
	case "error":
		return sErr.Render(glyphErr + " " + txt.Clean(txt.FirstLine(r.Error)))
	case "skipped":
		return sWarn.Render(glyphSkip + " " + txt.Clean(cmp.Or(txt.FirstLine(r.Error), "skipped")))
	case "cancelled":
		return sWarn.Render(glyphSkip + " cancelled")
	}
	return sOK.Render(glyphOK + " ok")
}

// ---------- history ----------

func (m Model) historyView() []string {
	if m.runsErr != nil {
		return []string{"", sErr.Render("  " + txt.Clean(m.runsErr.Error()))}
	}
	runs := m.visibleRuns()
	if len(runs) == 0 {
		return []string{"", sDim.Render("  none")}
	}
	wide := m.width >= 100
	cols := []column{{Title: "ID", Width: 5, Right: true}}
	if m.repName == "" {
		cols = append(cols, column{Title: "REPLICATION"})
	}
	if m.streamName == "" {
		cols = append(cols, column{Title: "STREAM"})
	}
	started := "Jan 02 15:04:05"
	if wide {
		cols = append(cols, column{Title: "STARTED", Width: 15}, column{Title: "TIME", Width: 7, Right: true},
			column{Title: "ROWS", Width: 9, Right: true}, column{Title: "BY", Width: 8}, column{Title: "STATUS"})
	} else {
		started = "01-02 15:04"
		cols = append(cols, column{Title: "STARTED", Width: 11}, column{Title: "ROWS", Width: 7, Right: true},
			column{Title: "BY", Width: 8}, column{Title: "STATUS"})
	}
	var rows [][]string
	for _, r := range runs {
		row := []string{strconv.FormatInt(r.ID, 10)}
		if m.repName == "" {
			row = append(row, txt.Clean(r.Replication))
		}
		if m.streamName == "" {
			row = append(row, txt.Clean(streamLabel(r.Stream)))
		}
		row = append(row, r.Started.Local().Format(started))
		if wide {
			row = append(row, txt.Dur(r.Duration()), txt.Thousands(r.Rows), sDim.Render(txt.Clean(r.Trigger)), m.statusCell(r))
		} else {
			row = append(row, txt.Thousands(r.Rows), sDim.Render(txt.Clean(r.Trigger)), m.statusCell(r))
		}
		rows = append(rows, row)
	}
	return table(m.width, cols, rows, m.cursor[viewHistory])
}

// ---------- run detail ----------

// planText lays out a dry run: one block per stream.
func planText(rows []engine.PlanRow, width int) string {
	var b strings.Builder
	counts := map[string]int{}
	var order []string
	sampled, failed := 0, 0
	for _, p := range rows {
		a := p.Action
		if p.Err != "" {
			a = "error"
		}
		if counts[a] == 0 {
			order = append(order, a)
		}
		counts[a]++
		if p.Sample != nil {
			sampled++
			if p.Sample.Err != "" {
				failed++
			}
		}
	}
	var parts []string
	for _, a := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[a], a))
	}
	if sampled > 0 {
		parts = append(parts, fmt.Sprintf("test load %d/%d ok", sampled-failed, sampled))
	}
	b.WriteString(sHeader.Render(plural(len(rows), "stream")) + sDim.Render("   "+strings.Join(parts, "   ")) + "\n\n")
	for i, p := range rows {
		if i > 0 {
			b.WriteString("\n")
		}
		head := sText.Render(txt.Clean(p.Stream)) + sDim.Render("  →  ") + sText.Render(txt.Clean(p.Object)) + "   "
		switch {
		case p.Err != "":
			head += sErr.Render(glyphErr + " " + txt.Clean(txt.FirstLine(p.Err)))
		case p.Action == "skip":
			head += sDim.Render("disabled")
		default:
			head += sWarn.Render(p.Action) + sDim.Render("  "+p.Mode)
			if p.Watermark != nil {
				head += sDim.Render(fmt.Sprintf("  from %v", p.Watermark))
			}
		}
		b.WriteString(ansi.Truncate(head, width, "…") + "\n")
		for _, l := range strings.Split(strings.TrimSpace(p.SQL), "\n") {
			b.WriteString("    " + sText.Render(ansi.Truncate(txt.Clean(l), max(width-4, 10), "…")) + "\n")
		}
		planColumnsText(&b, p.Columns, width)
		planSampleText(&b, p.Sample, width)
	}
	if len(rows) == 0 {
		b.WriteString(sDim.Render("  no streams"))
	}
	return b.String()
}

// planColumnsText lists a stream's columns: name, source type, target type, note.
func planColumnsText(b *strings.Builder, cols []engine.PlanColumn, width int) {
	if len(cols) == 0 {
		return
	}
	wn, ws, wt := 0, 0, 0
	for _, c := range cols {
		wn = max(wn, ansi.StringWidth(txt.Clean(c.Name)))
		ws = max(ws, ansi.StringWidth(txt.Clean(orDash(c.Source))))
		wt = max(wt, ansi.StringWidth(c.Target))
	}
	wn, ws, wt = min(wn, 32), min(ws, 20), min(wt, 24)
	b.WriteString("\n")
	for _, c := range cols {
		l := "    " + sText.Render(pad(txt.Clean(c.Name), wn, false)) + "  " + sDim.Render(pad(txt.Clean(orDash(c.Source)), ws, false)) +
			sDim.Render("  →  ")
		if c.Note == "" {
			l += sText.Render(c.Target)
		} else {
			l += sText.Render(pad(c.Target, wt, false)) + "   " + sWarn.Render(c.Note)
		}
		b.WriteString(ansi.Truncate(l, width, "…") + "\n")
	}
}

// planSampleText reports a stream's test load.
func planSampleText(b *strings.Builder, s *engine.PlanSample, width int) {
	if s == nil {
		return
	}
	b.WriteString("\n")
	label := "    " + sDim.Render("test load  ")
	if s.Err != "" {
		msg := txt.Clean(strings.Join(strings.Fields(s.Err), " "))
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, label, sErr.Width(max(width-15, 20)).Render(glyphErr+" "+msg)) + "\n")
		return
	}
	l := label + sOK.Render(plural(int(s.Rows), "row")+" loaded") +
		sDim.Render("   read "+txt.Dur(s.Read)+"   load "+txt.Dur(s.Load)+"   rolled back")
	b.WriteString(ansi.Truncate(l, width, "…") + "\n")
	if s.Stripped > 0 {
		b.WriteString("    " + sWarn.Render(fmt.Sprintf("           NUL bytes removed from %d values", s.Stripped)) + "\n")
	}
}

func runText(r state.Run, width int) string {
	var b strings.Builder
	w := func(k, v string) { fmt.Fprintf(&b, "%s  %s\n", sDim.Render(pad(k, 12, false)), v) }
	w("replication", txt.Clean(r.Replication))
	w("stream", txt.Clean(streamLabel(r.Stream)))
	w("object", txt.Clean(orDash(r.Object)))
	w("trigger", txt.Clean(r.Trigger))
	w("started", r.Started.Local().Format(time.DateTime))
	if r.Ended != nil {
		w("ended", r.Ended.Local().Format(time.DateTime))
	}
	w("duration", txt.Dur(r.Duration()))
	w("rows", txt.Thousands(r.Rows))
	switch r.Status {
	case "error":
		w("status", sErr.Render("error"))
	case "running":
		w("status", sWarn.Render("running"))
	case "skipped", "cancelled":
		w("status", sWarn.Render(r.Status))
	default:
		w("status", sOK.Render("ok"))
	}
	if r.Error != "" {
		box := sErrBox
		if r.Status != "error" {
			box = box.BorderForeground(colWarn)
		}
		b.WriteString("\n" + box.Width(max(width-2, 20)).Render(txt.CleanText(r.Error)) + "\n")
	}
	return b.String()
}

func (m Model) runView() []string {
	if m.planning {
		word := " resolving streams"
		if m.planSample {
			word = fmt.Sprintf(" test loading %d rows per stream, rolled back", engine.SampleRows)
		}
		return []string{"", "  " + sWarn.Render(m.spin.View()+word) + sDim.Render("  "+txt.Dur(time.Since(m.planStart).Round(time.Second)))}
	}
	m.vp.Height = max(m.bodyHeight()-1, 3)
	return append([]string{""}, strings.Split(m.vp.View(), "\n")...)
}

// ---------- connections ----------

func (m Model) connsView() []string {
	conns := m.visibleConns()
	if len(conns) == 0 {
		return []string{"", sDim.Render("  none")}
	}
	cols := []column{{Title: "NAME"}, {Title: "TYPE", Width: 12}, {Title: "URL"}, {Title: "SIZE", Right: true}, {Title: "TEST"}}
	var rows [][]string
	for _, c := range conns {
		size := ""
		if cs, ok := m.snap.ConnStats[c.Name]; ok {
			size = sizeText(cs.Bytes) + sDim.Render("  "+ago(cs.Refreshed))
		}
		test := ""
		switch {
		case m.testing[c.Name]:
			test = sWarn.Render(m.spin.View() + " testing")
		case c.Err != "":
			test = sErr.Render(glyphErr + " " + txt.Clean(txt.FirstLine(c.Err)))
		default:
			if res, ok := m.connResults[c.Name]; ok {
				if res.Err != nil {
					test = sErr.Render(glyphErr + " " + txt.Clean(txt.FirstLine(strings.TrimPrefix(res.Err.Error(), "conn "+c.Name+": "))))
				} else {
					test = sOK.Render(glyphOK+" ok") + sDim.Render("  "+txt.Dur(res.Took))
				}
			}
		}
		typ := c.Type
		if c.ReadOnly {
			typ += " ro"
		}
		rows = append(rows, []string{txt.Clean(c.Name), sDim.Render(txt.Clean(typ)), sDim.Render(txt.Clean(c.Target)), size, test})
	}
	out := table(m.width, cols, rows, m.cursor[viewConns])
	if cur, ok := at(conns, m.cursor[viewConns]); ok && !m.testing[cur.Name] {
		if res, ok := m.connResults[cur.Name]; ok && res.Err != nil {
			box := strings.Split(sErrBox.Width(max(m.width-4, 20)).Render(txt.CleanText(res.Err.Error())), "\n")
			out = append(append(out, ""), box...)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// phaseLabel words a running stream's phase.
func phaseLabel(phase string, rows int64) string {
	switch phase {
	case "connecting":
		return "connecting"
	case "querying":
		return "querying"
	case "fetching":
		return "fetching " + txt.Thousands(rows)
	case "loading":
		if rows > 0 {
			return "loading " + txt.Thousands(rows)
		}
		return "loading"
	case "merging":
		return "merging"
	}
	return "running"
}

// runningLabel summarises a replication with streams in flight.
func runningLabel(r replication) string {
	for _, run := range r.Last {
		if run.Status == "running" {
			return txt.Clean(run.Stream) + ": " + phaseLabel(run.Phase, run.Rows)
		}
	}
	return "starting"
}
