package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tummy/internal/api"
	"tummy/internal/config"
	"tummy/internal/db"
	"tummy/internal/probe"
)

type wizResult int

const (
	wizContinue wizResult = iota
	wizDone
	wizCancel
)

// wizard is a multi-step editor that takes over the body until it finishes.
type wizard interface {
	start() tea.Cmd
	update(msg tea.Msg) (wizResult, tea.Cmd)
	view(width, height int) []string
	title() string
	help() string
	result() statusMsg
	saved() string // name of what was saved, to select it
}

// wizAsk is a y/n question inside a wizard; esc goes back to where it was asked.
type wizAsk struct {
	text    string
	yes, no func() (wizResult, tea.Cmd)
}

func (a *wizAsk) key(msg tea.KeyMsg) (wizResult, tea.Cmd, bool) {
	switch msg.String() {
	case "y", "Y":
		return wrapRes(a.yes)
	case "n", "N":
		if a.no != nil {
			return wrapRes(a.no)
		}
		return wizContinue, nil, true
	case "esc":
		return wizContinue, nil, true
	}
	return wizContinue, nil, false
}

func wrapRes(fn func() (wizResult, tea.Cmd)) (wizResult, tea.Cmd, bool) {
	r, c := fn()
	return r, c, true
}

// nameRe is the name rule of config.ValidName, for callers that match a pattern.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// connTypes are the database dialects and api.
func connTypes() []string { return append(db.Dialects(), "api") }

type connProbeMsg struct {
	Name      string
	Databases []string // listed instead of tables when the connection names no database
	Tables    []string
	Schemas   []string // databases with schemas
	Err       error
	Took      time.Duration
}

// connOpenMsg is the first half of a database test: the connection is open, the listing
// follows as a second command so the screen can say which step it is on.
type connOpenMsg struct {
	Name      string
	DB        *db.DB
	Took      time.Duration
	Err       error
	Databases bool // list databases, not tables
}

type connStep int

const (
	connEdit connStep = iota
	connTest
)

type connWizard struct {
	opts      Options
	conns     *config.Connections // raw, for editing
	original  string
	base      config.Conn // as written: keys the form does not show are kept
	form      *form
	step      connStep
	probing   bool
	connected time.Duration // set while listing, after the open step
	scroll    int           // test screen: first table shown
	pageSize  int           // test screen: tables per page, set by view
	probe     *connProbeMsg
	done      statusMsg
	savedName string
	saveErr   string
	ask       *wizAsk
}

// entraModes are the fedauth values the form offers. "" is a SQL login.
var entraModes = []string{"", "ActiveDirectoryPassword", "ActiveDirectoryServicePrincipal", "ActiveDirectoryDefault", "ActiveDirectoryManagedIdentity", "ActiveDirectoryAzCli"}

var entraNotes = map[string]string{
	"ActiveDirectoryPassword":         "Entra user and password",
	"ActiveDirectoryServicePrincipal": "user: client id@tenant id, password: client secret",
	"ActiveDirectoryDefault":          "env, managed identity, az login",
	"ActiveDirectoryManagedIdentity":  "user: client id, blank: system assigned",
	"ActiveDirectoryAzCli":            "az login on this machine",
}

// entraMode is the Auth choice when it applies: SQL Server only; "" otherwise.
func entraMode(f *form) string {
	if f.get("type") != "mssql" || !f.shown("fedauth") {
		return ""
	}
	return f.get("fedauth")
}

// noPassword lists the Entra modes that take no password.
func noPassword(mode string) bool {
	return mode == "ActiveDirectoryDefault" || mode == "ActiveDirectoryManagedIdentity" || mode == "ActiveDirectoryAzCli"
}

// scheme is the url scheme for a type.
func scheme(typ string) string {
	if typ == "mssql" {
		return "sqlserver"
	}
	return typ
}

func newConnWizard(opts Options, editing string) (*connWizard, error) {
	conns, err := config.LoadConnectionsRaw(opts.Connections)
	if err != nil {
		return nil, err
	}
	w := &connWizard{opts: opts, conns: conns, original: editing}
	if editing != "" {
		cur, ok := conns.Conn[editing]
		if !ok {
			return nil, fmt.Errorf("connection %s not found", editing)
		}
		w.base = *cur
	}
	c := w.base
	isDB := func(f *form) bool { t := f.get("type"); return t != "api" && t != "sqlite" }
	// An existing url that is not scheme://… (a key=value DSN, or one env reference) is edited as written.
	parts, parsed := config.ParseURL(c.URL)
	if c.Type == "api" {
		parts.User, parts.Password = c.User, c.Password
	}
	if parts.Query == nil {
		parts.Query = url.Values{}
	}
	if parts.Instance != "" {
		parts.Host += `\` + parts.Instance
	}
	raw := c.URL != "" && !parsed
	rawURL := func(f *form) bool { return isDB(f) && raw }
	fields := func(f *form) bool { return isDB(f) && !raw }
	isAPI := func(f *form) bool { return f.get("type") == "api" }
	authIs := func(vals ...string) func(*form) bool {
		return func(f *form) bool { return isAPI(f) && slices.Contains(vals, f.get("auth")) }
	}
	title := "New connection"
	if editing != "" {
		title = "Edit connection " + editing
	}
	dir := shortHome(filepath.Dir(opts.Connections))
	w.form = newForm(title,
		text("name", "Name", "letters, digits, _ -").eg("DEVPG").def(c.Name).required().check(func(v string) error {
			if !config.ValidName(v) {
				return errors.New("letters, digits, _ -")
			}
			if _, exists := conns.Conn[v]; exists && v != editing {
				return errors.New("exists")
			}
			return nil
		}),
		choice("type", "Type", "", connTypes()...).def(cmp.Or(c.Type, "postgres")),
		text("url", "URL", "${VAR}: environment").def(c.URL).when(rawURL).required().masked().check(func(v string) error {
			return config.CheckURL(w.form.get("type"), v)
		}),
		text("host", "Host", "").eg("db.example.com   srv\\SQLEXPRESS").def(parts.Host).when(fields).required(),
		text("port", "Port", "").def(parts.Port).when(fields).helpFn(func(f *form) string {
			return "blank: " + config.DefaultPort[f.get("type")]
		}).check(func(v string) error {
			if _, err := strconv.Atoi(v); err != nil {
				return errors.New("a number")
			}
			return nil
		}),
		text("database", "Database", "blank: test lists the databases").def(parts.Database).when(fields),
		choice("fedauth", "Auth", "", entraModes...).empty("SQL login").notes(entraNotes).def(parts.Query.Get("fedauth")).when(func(f *form) bool { return fields(f) && f.get("type") == "mssql" }),
		text("user", "User", "").def(parts.User).when(func(f *form) bool { return fields(f) || authIs("basic")(f) }).requiredWhen(func(f *form) bool {
			return authIs("basic")(f) || !noPassword(entraMode(f))
		}),
		secret("password", "Password", "${VAR}: environment").eg("${DB_PASS}").def(parts.Password).when(func(f *form) bool {
			return (fields(f) && !noPassword(entraMode(f))) || authIs("basic")(f)
		}),
		text("options", "Options", "driver parameters").def(pairs(queryPairs(parts.Query))).when(fields).check(checkPairs).helpFn(func(f *form) string {
			if f.get("options") != "" {
				return "driver parameters"
			}
			return optionExamples[f.get("type")] + "   driver parameters"
		}),
		combo("schema", "Schema", "for unqualified table names; ctrl+g lists them", "").empty("default").eg("public   dbo").def(c.Schema).when(isDB),
		text("path", "Path", "relative to "+dir).eg("shop.db").def(c.Path).when(func(f *form) bool { return f.get("type") == "sqlite" }).required(),
		toggle("read_only", "Read only", "never a target", c.ReadOnly).when(func(f *form) bool { return !isAPI(f) }),
		text("base_url", "Base URL", "stream paths are appended").eg("https://api.example.com/v1").def(c.BaseURL).when(isAPI).required(),
		choice("auth", "Auth", "", "", "bearer", "basic", "header", "query").empty("none").def(c.Auth).when(isAPI),
		secret("token", "Token", "${VAR}: environment").eg("${API_TOKEN}").def(c.Token).when(authIs("bearer")).required(),
		text("header", "Header", "").eg("X-Api-Key").def(c.Header).when(authIs("header")),
		secret("token_h", "Key", "${VAR}: environment").eg("${API_KEY}").def(c.Token).when(authIs("header")).required(),
		text("param", "Parameter", "").eg("api_key").def(c.Param).when(authIs("query")).required(),
		secret("token_q", "Key", "${VAR}: environment").eg("${API_KEY}").def(c.Token).when(authIs("query")).required(),
		text("rate_limit", "Rate limit", "shared by every stream").eg("100/m   10/s   5000/h").def(c.RateLimit).when(isAPI).check(func(v string) error {
			_, err := api.ParseRate(v)
			return err
		}),
		text("retry_on", "Retry on", "HTTP statuses retried with backoff").eg("429, 502, 503, 504").def(ints(c.RetryOn)).when(isAPI).check(func(v string) error {
			_, err := parseInts(v)
			return err
		}),
		text("headers", "Headers", "sent with every request to the base URL host").eg("X-Tenant = acme, Accept = application/json").def(pairs(c.Headers)).when(isAPI).check(checkPairs),
		text("timeout", "Timeout", "per request; blank: 2m").eg("30s").def(c.Timeout).when(isAPI).check(func(v string) error {
			if d, err := time.ParseDuration(v); err != nil || d <= 0 {
				return errors.New("a duration: 30s, 5m")
			}
			return nil
		}),
	)
	w.form.done = "save"
	if c.Schema != "" {
		w.form.field("schema").Options = []string{"", c.Schema}
	}
	return w, nil
}

func (w *connWizard) title() string {
	if w.original != "" {
		return "edit " + w.original
	}
	return "new connection"
}

func (w *connWizard) help() string {
	l, r := w.footer()
	return l + "  " + r
}

func (w *connWizard) footer() (string, string) {
	switch {
	case w.ask != nil:
		return sWarn.Render(txt.Clean(w.ask.text)), helpLine("esc", "back")
	case w.step == connTest && w.canCreate() != "":
		return helpLine("c", "create file", "ctrl+s", "save", "t", "test again"), helpLine("esc", "edit")
	case w.step == connTest:
		return helpLine("ctrl+s", "save", "t", "test again", "↑/↓ pgup/pgdn", "scroll"), helpLine("esc", "edit")
	}
	return helpLine(append(w.form.hints(), "ctrl+s", "save", "ctrl+g", "test")...), helpLine("esc", escWord(w.form, "cancel"))
}

// escWord is what esc does: cancel an open list, else the given word.
func escWord(f *form, otherwise string) string {
	if f != nil && f.drop != nil {
		return "cancel list"
	}
	return otherwise
}

func (w *connWizard) typing() bool { return w.step == connEdit && w.ask == nil && w.form.typing() }
func (w *connWizard) dirty() bool  { return w.form.changed() }

func (w *connWizard) keyHelp() (string, [][2]string) {
	return "connection", [][2]string{
		{"ctrl+s", "save"},
		{"ctrl+g", "test: connect, list tables and schemas"},
		{"c", "after a test: create a missing sqlite file"},
		{"esc", "cancel"},
	}
}

// canCreate is the sqlite file the last test did not find, else "".
func (w *connWizard) canCreate() string {
	if w.probe == nil || w.probe.Err == nil || w.probing {
		return ""
	}
	return missingFile(w.conn())
}

func (w *connWizard) result() statusMsg { return w.done }
func (w *connWizard) saved() string     { return w.savedName }
func (w *connWizard) start() tea.Cmd    { return w.form.focus() }

// conn builds the connection from the form, on top of the one being edited.
func (w *connWizard) conn() *config.Conn {
	f := w.form
	c := w.base
	c.Name, c.Type = f.get("name"), f.get("type")
	c.Dir = filepath.Dir(w.opts.Connections)
	c.URL, c.Schema, c.Path, c.BaseURL, c.Auth = "", "", "", "", ""
	c.Token, c.Header, c.Param, c.User, c.Password, c.RateLimit, c.RetryOn = "", "", "", "", "", "", nil
	c.ReadOnly = c.Type != "api" && f.on("read_only")
	switch c.Type {
	case "sqlite":
		c.Path = f.get("path")
	case "api":
		c.BaseURL, c.Auth = f.get("base_url"), f.get("auth")
		switch c.Auth {
		case "bearer":
			c.Token = f.get("token")
		case "header":
			c.Header, c.Token = f.get("header"), f.get("token_h")
		case "query":
			c.Param, c.Token = f.get("param"), f.get("token_q")
		case "basic":
			c.User, c.Password = f.get("user"), f.get("password")
		}
		c.RateLimit = f.get("rate_limit")
		c.RetryOn, _ = parseInts(f.get("retry_on"))
		if f.fieldChanged("headers") {
			c.Headers, _ = parsePairs(f.get("headers"))
		}
		c.Timeout = f.get("timeout")
	default:
		c.Schema = f.get("schema")
		if fl := f.field("host"); fl == nil || (fl.Show != nil && !fl.Show(f)) {
			c.URL = f.get("url")
			break
		}
		p := config.URLParts{Scheme: scheme(c.Type), Host: f.get("host"), Port: f.get("port"), Database: f.get("database"), User: f.get("user"), Query: url.Values{}}
		if h, inst, has := strings.Cut(p.Host, "\\"); has && c.Type == "mssql" {
			p.Host, p.Instance = h, inst
		}
		mode := entraMode(f)
		if !noPassword(mode) {
			p.Password = f.get("password")
		}
		if opts, _ := parsePairs(f.get("options")); opts != nil {
			for k, v := range opts {
				p.Query.Set(k, v)
			}
		}
		if mode != "" {
			p.Query.Set("fedauth", mode)
		}
		c.URL = p.String()
	}
	if c.Type != "api" {
		c.Headers, c.Timeout = nil, ""
	}
	return &c
}

func (w *connWizard) update(msg tea.Msg) (wizResult, tea.Cmd) {
	switch msg := msg.(type) {
	case connOpenMsg:
		if msg.Err != nil {
			w.probing = false
			w.probe = &connProbeMsg{Name: msg.Name, Err: msg.Err, Took: msg.Took}
			return wizContinue, nil
		}
		w.connected = msg.Took
		return wizContinue, listTablesCmd(msg)
	case connProbeMsg:
		w.probing = false
		w.probe = &msg
		if fl := w.form.field("schema"); fl != nil && len(msg.Schemas) > 0 {
			fl.Options = append([]string{""}, msg.Schemas...)
		}
		return wizContinue, nil
	case tea.KeyMsg:
		if w.ask != nil {
			ask := w.ask
			res, cmd, ok := ask.key(msg)
			if ok {
				w.ask = nil
			}
			return res, cmd
		}
		if w.step == connTest {
			switch msg.String() {
			case "esc", "backspace":
				w.step = connEdit
				return wizContinue, w.form.focus()
			case "t", "ctrl+g":
				return wizContinue, w.startProbe()
			case "down", "j":
				w.scroll++
			case "up", "k":
				w.scroll = max(w.scroll-1, 0)
			case "pgdown", " ":
				w.scroll += w.pageSize
			case "pgup":
				w.scroll = max(w.scroll-w.pageSize, 0)
			case "home", "g":
				w.scroll = 0
			case "end", "G":
				w.scroll = math.MaxInt32
			case "c":
				if path := w.canCreate(); path != "" {
					if err := probe.CreateSQLite(path); err != nil {
						w.saveErr = err.Error()
						return wizContinue, nil
					}
					return wizContinue, w.startProbe()
				}
			case "ctrl+s":
				return w.trySave()
			}
			return wizContinue, nil
		}
		switch msg.String() {
		case "ctrl+g":
			w.form.commit()
			if !w.form.validate() {
				return wizContinue, w.form.focus()
			}
			w.step = connTest
			w.saveErr = ""
			return wizContinue, w.startProbe()
		case "ctrl+s":
			w.form.commit()
			return w.trySave()
		}
		res, cmd := w.form.update(msg)
		switch res {
		case formCancel:
			return wizCancel, nil
		case formSubmit:
			return w.trySave()
		}
		return wizContinue, cmd
	}
	return wizContinue, nil
}

func (w *connWizard) startProbe() tea.Cmd {
	w.probing = true
	w.probe = nil
	w.connected = 0
	w.scroll = 0
	c := w.conn()
	if c.Type == "api" || c.Type == "sqlite" {
		return func() tea.Msg { return probeConn(c) }
	}
	return func() tea.Msg { return openConn(c) }
}

// probeConn reaches an API or opens a sqlite file and lists its tables, in one step.
func probeConn(c *config.Conn) connProbeMsg {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := probe.Conn(ctx, c, probe.AllSchemas)
	return connProbeMsg{Name: c.Name, Tables: res.Tables, Err: err, Took: res.Took}
}

// noDatabase is true when the url names no database. Such a test lists the databases.
func noDatabase(c *config.Conn) bool {
	p, ok := config.ParseURL(c.URL)
	return ok && p.Database == "" && (c.Type == "postgres" || c.Type == "mssql")
}

// openConn is the connect step of a database test. Postgres needs a database to log
// into: without one the maintenance database stands in.
func openConn(c *config.Conn) tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	x := *c
	if c.Type == "postgres" && noDatabase(c) {
		p, _ := config.ParseURL(c.URL)
		p.Database = "postgres"
		x.URL = p.String()
	}
	d, err := probe.Open(ctx, &x)
	return connOpenMsg{Name: c.Name, DB: d, Took: time.Since(start), Err: err, Databases: noDatabase(c)}
}

// listTablesCmd is the listing step: every schema's tables, then the schema names.
func listTablesCmd(open connOpenMsg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		defer open.DB.Close()
		start := time.Now()
		msg := connProbeMsg{Name: open.Name}
		if open.Databases {
			dbs, err := open.DB.ListDatabases(ctx)
			if err != nil {
				msg.Err = fmt.Errorf("conn %s: list databases: %w", open.Name, err)
				return msg
			}
			msg.Databases, msg.Took = dbs, open.Took+time.Since(start)
			return msg
		}
		tables, err := open.DB.ListAllTables(ctx)
		if err != nil {
			msg.Err = fmt.Errorf("conn %s: list tables after %s, connected in %s: %w", open.Name, txt.Dur(time.Since(start)), txt.Dur(open.Took), err)
			return msg
		}
		for _, t := range tables {
			msg.Tables = append(msg.Tables, t.String())
		}
		msg.Schemas, _ = open.DB.ListSchemas(ctx)
		msg.Took = open.Took + time.Since(start)
		return msg
	}
}

// trySave validates and saves; a rename asks about the replications using the old name.
func (w *connWizard) trySave() (wizResult, tea.Cmd) {
	if !w.form.validate() {
		w.step = connEdit
		return wizContinue, w.form.focus()
	}
	c := w.conn()
	if w.original == "" || c.Name == w.original {
		return w.finish(nil)
	}
	users := usersOf(w.opts.Home, w.original)
	if len(users) == 0 {
		return w.finish(nil)
	}
	w.ask = &wizAsk{
		text: fmt.Sprintf("rename %s to %s in %s too? y/n", w.original, c.Name, strings.Join(users, ", ")),
		yes:  func() (wizResult, tea.Cmd) { return w.finish(users) },
		no: func() (wizResult, tea.Cmd) {
			res, cmd := w.finish(nil)
			if res == wizDone {
				verb := "uses"
				if len(users) > 1 {
					verb = "use"
				}
				w.done = statusMsg{Text: fmt.Sprintf("saved: %s; %s still %s %s", c.Name, strings.Join(users, ", "), verb, w.original), IsErr: true}
			}
			return res, cmd
		},
	}
	return wizContinue, nil
}

// finish writes connections.toml, then points the given replications at the new name.
func (w *connWizard) finish(rename []string) (wizResult, tea.Cmd) {
	c := w.conn()
	if w.original != "" && w.original != c.Name {
		if err := w.conns.Rename(w.original, c.Name); err != nil { // keeps the section in place
			w.saveErr = err.Error()
			return wizContinue, nil
		}
	}
	c.Dir = ""
	w.conns.Conn[c.Name] = c
	if err := config.WriteConnections(w.opts.Connections, w.conns); err != nil {
		w.saveErr = err.Error()
		return wizContinue, nil
	}
	w.done = statusMsg{Text: "saved: " + c.Name}
	w.savedName = c.Name
	if err := renameConn(w.opts.Home, rename, w.original, c.Name); err != nil {
		w.done = statusMsg{Text: "saved: " + c.Name + "; " + err.Error(), IsErr: true}
	}
	return wizDone, nil
}

// usersOf lists the replications whose source or target is conn.
func usersOf(home, conn string) []string {
	files, _ := config.ListReplicationFiles(home)
	var out []string
	for _, f := range files {
		if r, err := config.LoadReplicationRaw(f); err == nil && (r.Source == conn || r.Target == conn) {
			out = append(out, config.NameOf(f))
		}
	}
	return out
}

// renameConn points the named replications from one connection name to another.
func renameConn(home string, reps []string, from, to string) error {
	var errs []error
	for _, name := range reps {
		path, err := config.ReplicationPath(home, name)
		if err == nil {
			var r *config.Replication
			if r, err = config.LoadReplicationRaw(path); err == nil {
				d := config.DraftOf(r)
				if d.Source == from {
					d.Source = to
				}
				if d.Target == from {
					d.Target = to
				}
				_, err = config.WriteReplication(home, d)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (w *connWizard) view(width, height int) []string {
	if w.step == connEdit {
		out := w.form.view(width, height-2)
		if w.saveErr != "" {
			out = append(out, "", "  "+sErr.Render(glyphErr+" "+txt.Clean(txt.FirstLine(w.saveErr))))
		}
		return out
	}
	c := w.conn()
	out := []string{" " + sTitle.Render(txt.Clean(c.Name)) + "  " + sDim.Render(txt.Clean(c.Type+"  "+c.Target())), ""}
	switch {
	case w.probing && w.connected > 0:
		out = append(out, "  "+sOK.Render(glyphOK+" connected")+sDim.Render("  "+txt.Dur(w.connected))+"   "+sWarn.Render("◐ listing tables"))
	case w.probing:
		out = append(out, "  "+sWarn.Render("◐ connecting"))
	case w.probe != nil && w.probe.Err != nil:
		out = append(out, strings.Split(sErrBox.Width(max(width-4, 20)).Render(txt.CleanText(w.probe.Err.Error())), "\n")...)
	case w.probe != nil:
		out = append(out, "  "+sOK.Render(glyphOK+" connected")+sDim.Render("  "+txt.Dur(w.probe.Took)))
		if len(w.probe.Databases) > 0 {
			out = append(out, "", "  "+sHeader.Render(plural(len(w.probe.Databases), "database")))
			for _, d := range w.probe.Databases {
				out = append(out, "    "+txt.Clean(d))
			}
		} else if c.Type != "api" {
			out = append(out, "", "  "+sHeader.Render(plural(len(w.probe.Tables), "table")))
			if counts := schemaCounts(w.probe.Tables); len(counts) > 1 {
				for _, line := range wrapWords(counts, "   ", max(width-4, 20)) {
					out = append(out, "  "+sDim.Render(txt.Clean(line)))
				}
			}
			all := w.probe.Tables
			room := max(height-len(out)-3, 1)
			w.pageSize = room
			w.scroll = max(min(w.scroll, len(all)-room), 0)
			shown := all[w.scroll:min(w.scroll+room, len(all))]
			for _, t := range shown {
				out = append(out, "    "+txt.Clean(t))
			}
			if end := w.scroll + len(shown); end < len(all) || w.scroll > 0 {
				out = append(out, sDim.Render(fmt.Sprintf("    %d-%d of %d", w.scroll+1, end, len(all))))
			}
		}
	}
	if w.saveErr != "" {
		out = append(out, "")
		out = append(out, strings.Split(sErrBox.Width(max(width-4, 20)).Render(txt.CleanText(w.saveErr)), "\n")...)
	}
	return out
}

// ---------- helpers ----------

func helpLine(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, sKey.Render(pairs[i])+" "+sDim.Render(pairs[i+1]))
	}
	return strings.Join(parts, "  ")
}

func ints(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ", ")
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, p := range splitList(s) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parsePairs reads "k = v, k2 = v2".
func parsePairs(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, p := range splitList(s) {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("%q: want key = value", p)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// pairs writes a map as "k = v, k2 = v2", keys sorted.
func pairs(m map[string]string) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, k+" = "+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

var optionExamples = map[string]string{
	"postgres": "sslmode = require",
	"mysql":    "tls = true",
	"mssql":    "encrypt = true",
}

// queryPairs flattens a query to its first values, fedauth left out: the Auth field holds it.
func queryPairs(q url.Values) map[string]string {
	out := map[string]string{}
	for k, v := range q {
		if k != "fedauth" && len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// schemaCounts is "schema N" per schema in the order the tables come, "" left out.
func schemaCounts(tables []string) []string {
	var order []string
	n := map[string]int{}
	for _, t := range tables {
		schema, _, ok := strings.Cut(t, ".")
		if !ok {
			continue
		}
		if n[schema] == 0 {
			order = append(order, schema)
		}
		n[schema]++
	}
	var out []string
	for _, s := range order {
		out = append(out, fmt.Sprintf("%s %d", s, n[s]))
	}
	return out
}

// wrapWords joins words with sep into lines no wider than width; a word wider than width
// gets a line of its own.
func wrapWords(words []string, sep string, width int) []string {
	var out []string
	line := ""
	for _, w := range words {
		switch {
		case line == "":
			line = w
		case lipgloss.Width(line)+lipgloss.Width(sep)+lipgloss.Width(w) <= width:
			line += sep + w
		default:
			out = append(out, line)
			line = w
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}
