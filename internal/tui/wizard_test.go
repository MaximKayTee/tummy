package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"tummy/internal/txt"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	_ "modernc.org/sqlite"

	"tummy/internal/config"
)

func TestMain(m *testing.M) {
	cursorMode = cursor.CursorStatic // no blink ticks for feed to wait on
	os.Exit(m.Run())
}

func keyRunes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
	keyEnter = tea.KeyMsg{Type: tea.KeyEnter}
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
	keyTab   = tea.KeyMsg{Type: tea.KeyTab}
	keyRight = tea.KeyMsg{Type: tea.KeyRight}
	keySave  = tea.KeyMsg{Type: tea.KeyCtrlS}
	keySpace = tea.KeyMsg{Type: tea.KeySpace}
	keyDown  = tea.KeyMsg{Type: tea.KeyDown}
	keyUp    = tea.KeyMsg{Type: tea.KeyUp}
	keyBack  = tea.KeyMsg{Type: tea.KeyBackspace}
	keyTest  = tea.KeyMsg{Type: tea.KeyCtrlG}
	keyAll   = tea.KeyMsg{Type: tea.KeyCtrlA}
	keyNone  = tea.KeyMsg{Type: tea.KeyCtrlN}
)

func typeKeys(f *form, s string) {
	for _, r := range s {
		f.update(keyRunes(string(r)))
	}
}

// feed runs a command and hands its messages to the wizard, one level deep. Commands
// still running after a moment, such as cursor blinks, are dropped.
func feed(t *testing.T, w wizard, cmd tea.Cmd) {
	t.Helper()
	for _, msg := range quick(cmd) {
		if _, ok := msg.(tea.KeyMsg); ok {
			continue
		}
		w.update(msg)
	}
}

func quick(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if b, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range b {
				out = append(out, quick(c)...)
			}
			return out
		}
		if msg == nil {
			return nil
		}
		return []tea.Msg{msg}
	case <-time.After(300 * time.Millisecond):
		return nil
	}
}

// send sends keys to a wizard and runs what they start.
func send(t *testing.T, w wizard, msgs ...tea.KeyMsg) wizResult {
	t.Helper()
	var res wizResult
	for _, m := range msgs {
		var cmd tea.Cmd
		res, cmd = w.update(m)
		feed(t, w, cmd)
	}
	return res
}

// walk presses enter until the replication wizard leaves the step.
func walk(t *testing.T, w *repWizard) {
	t.Helper()
	step := w.step
	for i := 0; i < 60 && w.step == step; i++ {
		send(t, w, keyEnter)
	}
	if w.step == step {
		t.Fatalf("stuck on step %d", step)
	}
}

func repFile(t *testing.T, opts Options, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(config.ReplicationsDir(opts.Home), name+".toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFormBasics(t *testing.T) {
	f := newForm("t",
		text("name", "Name", "").required(),
		choice("kind", "Kind", "", "a", "b", "c"),
		text("only_b", "Only B", "").when(func(f *form) bool { return f.get("kind") == "b" }),
		toggle("on", "On", "", false),
	)
	if len(f.visible()) != 3 {
		t.Fatalf("conditional field shown: %d", len(f.visible()))
	}
	if res, _ := f.update(keySave); res != formContinue || f.field("name").err != "required" {
		t.Fatalf("required check: %v %q", res, f.field("name").err)
	}
	typeKeys(f, "x1")
	f.update(keyTab)
	f.update(keyRight)
	if f.get("kind") != "b" || len(f.visible()) != 4 || f.focused.Key != "kind" {
		t.Fatalf("select right: %q, visible %d, focus %s", f.get("kind"), len(f.visible()), f.focused.Key)
	}
	f.update(keyTab)
	typeKeys(f, "bee")
	f.update(keyTab)
	f.update(keySpace)
	if !f.on("on") {
		t.Fatal("toggle")
	}
	res, _ := f.update(keyEnter) // last field: submit
	if res != formSubmit || f.get("name") != "x1" || f.get("only_b") != "bee" {
		t.Fatalf("submit: %v %q %q", res, f.get("name"), f.get("only_b"))
	}
	if res, _ := f.update(keyEsc); res != formCancel {
		t.Fatal("esc cancels")
	}
	v := strings.Join(f.view(80), "\n")
	if !strings.Contains(v, "[b]") || !strings.Contains(v, "● on") {
		t.Fatalf("view:\n%s", v)
	}
	if !f.changed() {
		t.Fatal("changed")
	}
}

func TestFocusStaysWhenFieldsAppear(t *testing.T) {
	f := newForm("t",
		text("a", "A", ""),
		text("since", "Since", "").when(func(f *form) bool { return f.get("mode") == "incremental" }),
		text("sorted", "Sorted", "").when(func(f *form) bool { return f.get("mode") == "incremental" }),
		choice("mode", "Mode", "", "full-refresh", "truncate", "incremental", "backfill"),
		text("z", "Z", ""),
	)
	f.update(keyDown) // mode
	for range 3 {
		f.update(keyRight)
	}
	if f.get("mode") != "backfill" || f.focused.Key != "mode" {
		t.Fatalf("focus moved: %s on %q", f.focused.Key, f.get("mode"))
	}
	f.update(tea.KeyMsg{Type: tea.KeyLeft}) // incremental: two fields appear above
	if f.focused.Key != "mode" || f.cur != 3 {
		t.Fatalf("focus moved: %s at %d", f.focused.Key, f.cur)
	}
	f.update(keyDown)
	if f.focused.Key != "z" {
		t.Fatalf("next after mode: %s", f.focused.Key)
	}
}

func TestFieldErrorKeepsExample(t *testing.T) {
	f := newForm("t", text("n", "N", "sales   letters").required(), text("m", "M", ""))
	f.update(keySave)
	f.update(keyDown)
	v := strings.Join(f.view(80), "\n")
	if !strings.Contains(v, "↳ required") || !strings.Contains(v, "sales   letters") {
		t.Fatalf("error hides the example:\n%s", v)
	}
}

func TestFormScrolls(t *testing.T) {
	var fs []*field
	for i := range 30 {
		fs = append(fs, text(fmt.Sprintf("f%02d", i), fmt.Sprintf("Field %02d", i), "help"))
	}
	opts := make([]string, 20)
	for i := range opts {
		opts[i] = fmt.Sprintf("opt%02d", i)
	}
	fs = append(fs, choice("pick", "Pick", "", opts...))
	f := newForm("Tall", fs...)
	for range 25 {
		f.update(keyDown)
	}
	v := f.view(80, 12)
	if len(v) > 12 || !strings.Contains(strings.Join(v, "\n"), "Field 25") || !strings.Contains(strings.Join(v, "\n"), "more") {
		t.Fatalf("focused field off screen (%d lines):\n%s", len(v), strings.Join(v, "\n"))
	}
	for range 10 {
		f.update(keyDown)
	}
	f.update(keyEnter) // open the list on the last field
	f.update(keyDown)
	v = f.view(80, 16)
	joined := strings.Join(v, "\n")
	if len(v) > 16 || !strings.Contains(joined, "Pick") || !strings.Contains(joined, "› opt01") {
		t.Fatalf("open list off screen (%d lines):\n%s", len(v), joined)
	}
}

func TestPicker(t *testing.T) {
	p := newPicker("t", []string{"a.x", "a.y", "b.z"}, []string{"a.y"})
	if got := p.chosen(); len(got) != 1 || got[0] != "a.y" {
		t.Fatal("preselected")
	}
	p.update(keySpace) // a.x
	p.update(keyRunes("j"))
	p.update(keySpace) // untick a.y
	if got := p.chosen(); len(got) != 1 || got[0] != "a.x" {
		t.Fatalf("toggle: %v", got)
	}
	p.update(keyRunes("/"))
	p.update(keyRunes("b"))
	p.update(keyEnter)
	if vis := p.visible(); len(vis) != 1 || vis[0] != "b.z" {
		t.Fatalf("filter: %v", vis)
	}
	p.update(keyRunes("a"))
	if got := p.chosen(); len(got) != 2 {
		t.Fatalf("all within filter: %v", got)
	}
	if res, _ := p.update(keyEnter); res != formSubmit {
		t.Fatal("enter submits")
	}
	v := strings.Join(p.view(60, 10), "\n")
	if !strings.Contains(v, "2 selected") || !strings.Contains(v, "● b.z") {
		t.Fatalf("view:\n%s", v)
	}
}

func TestPickerEnterPicksCurrentWhenNothingTicked(t *testing.T) {
	p := newPicker("t", []string{"a", "b", "c"}, nil)
	p.update(keyRunes("j"))
	if res, _ := p.update(keyEnter); res != formSubmit || strings.Join(p.chosen(), ",") != "b" {
		t.Fatalf("enter should pick b: %v", p.chosen())
	}
	p2 := newPicker("t", []string{"a", "b"}, nil)
	p2.update(keyRight)
	if strings.Join(p2.chosen(), ",") != "a" {
		t.Fatal("right ticks")
	}
}

func TestPickerShowsControlCharactersEscaped(t *testing.T) {
	p := newPicker("t", []string{"evil\x1b]52;c;eA==\x07", "ok"}, nil)
	v := strings.Join(p.view(80, 10), "\n")
	if strings.Contains(v, "\x07") || strings.Contains(v, "]52;") && !strings.Contains(v, `\x1b]52;`) {
		t.Fatalf("raw control characters:\n%q", v)
	}
	if txt.Clean("a\x1b[8mb\u009bc\td") != `a\x1b[8mb\x9bc d` {
		t.Fatalf("clean: %q", txt.Clean("a\x1b[8mb\u009bc\td"))
	}
}

func TestMultiField(t *testing.T) {
	f := newForm("t", multi("k", "K", "", []string{"a", "b", "c"}, []string{"b"}).required().fixed())
	if got := f.picked("k"); len(got) != 1 || got[0] != "b" {
		t.Fatalf("preselected: %v", got)
	}
	if v := strings.Join(f.view(80), "\n"); !strings.Contains(v, "[b] ▾") || strings.Contains(v, "○ a") {
		t.Fatalf("closed shows the picks only:\n%s", v)
	}
	f.update(keySpace) // open
	if f.drop == nil {
		t.Fatal("dropdown")
	}
	if v := strings.Join(f.view(80), "\n"); !strings.Contains(v, "○ a") || !strings.Contains(v, "● b") {
		t.Fatalf("open list:\n%s", v)
	}
	f.update(keyDown)
	f.update(keyDown)
	f.update(keySpace) // c
	f.update(keyUp)
	f.update(keySpace) // untick b
	f.update(keyEnter)
	if got := strings.Join(f.picked("k"), ","); got != "c" || f.drop != nil {
		t.Fatalf("toggle: %s", got)
	}
	f.update(keySpace)
	f.update(keySpace) // tick a, then cancel
	f.update(keyEsc)
	if got := strings.Join(f.picked("k"), ","); got != "c" || f.drop != nil {
		t.Fatalf("esc keeps the old ticks: %s", got)
	}
	// unticking everything and enter clears the field
	f.update(keySpace)
	f.update(keyNone)
	f.update(keyEnter)
	if got := f.picked("k"); len(got) != 0 {
		t.Fatalf("enter after unticking all: %v", got)
	}
	if res, _ := f.update(keySave); res != formContinue || f.field("k").err != "required" {
		t.Fatal("required multi")
	}
	f.update(keyAll)
	if res, _ := f.update(keySave); res != formSubmit || len(f.picked("k")) != 3 {
		t.Fatal("all")
	}
	// a fixed list takes no typed values
	f.update(keyNone)
	f.update(keySpace)
	typeKeys(f, "zz")
	if v := strings.Join(f.view(80), "\n"); strings.Contains(v, "zz  new") {
		t.Fatalf("typed value offered on a fixed list:\n%s", v)
	}
	f.update(keyEnter)
	if got := f.picked("k"); len(got) != 0 {
		t.Fatalf("typed text accepted: %v", got)
	}
}

func TestMultiTypedValueIsARow(t *testing.T) {
	f := newForm("t", multi("k", "K", "", []string{"apple", "banana"}, nil))
	f.update(keySpace)
	typeKeys(f, "zz")
	v := strings.Join(f.view(80), "\n")
	if !strings.Contains(v, "○ zz") || !strings.Contains(v, "new") {
		t.Fatalf("typed row:\n%s", v)
	}
	f.update(keySpace) // tick it
	f.update(keyEnter)
	if got := strings.Join(f.picked("k"), ","); got != "zz" {
		t.Fatalf("typed value: %s", got)
	}
}

func TestMultiDropdownFilters(t *testing.T) {
	var opts []string
	for i := 1; i <= 139; i++ {
		opts = append(opts, fmt.Sprintf("%d", i))
	}
	f := newForm("t", multi("k", "K", "", opts, nil).fixed())
	f.update(keySpace) // open, typing straight away
	v := strings.Join(f.view(60), "\n")
	if !strings.Contains(v, "1 of 139") {
		t.Fatalf("footer:\n%s", v)
	}
	typeKeys(f, "12")
	if v := strings.Join(f.view(60), "\n"); !strings.Contains(v, "1 of 12") || strings.Contains(v, "○ 2 ") { // 12 112 120…129
		t.Fatalf("typed filter:\n%s", v)
	}
	f.update(keySpace) // tick 12
	f.update(keyDown)
	f.update(keySpace) // tick 120: names starting with the filter come first
	f.update(keyEnter)
	if got := strings.Join(f.picked("k"), ","); got != "12,120" || f.drop != nil {
		t.Fatalf("typed filter then ticks: %s", got)
	}
}

func TestSelectDropdown(t *testing.T) {
	var opts []string
	for i := 0; i < 40; i++ {
		opts = append(opts, fmt.Sprintf("col_%02d", i))
	}
	f := newForm("t", choice("c", "C", "", opts...), text("x", "X", ""))
	v := strings.Join(f.view(60), "\n")
	if !strings.Contains(v, "[col_00] ▾") || strings.Contains(v, "col_01") {
		t.Fatalf("closed shows the value only:\n%s", v)
	}
	f.update(keyEnter)
	if f.drop == nil {
		t.Fatal("enter opens the list")
	}
	lines := f.view(60)
	if len(lines) > dropRows+7 || !strings.Contains(strings.Join(lines, "\n"), "col_09") || strings.Contains(strings.Join(lines, "\n"), "col_12") {
		t.Fatalf("list scrolls at %d rows:\n%s", dropRows, strings.Join(lines, "\n"))
	}
	f.update(tea.KeyMsg{Type: tea.KeyPgDown})
	if it, _ := f.drop.current(); it != "col_10" {
		t.Fatalf("pgdown: %s", it)
	}
	typeKeys(f, "_3")
	f.update(keyDown)
	f.update(keyEnter)
	if f.get("c") != "col_31" || f.drop != nil || f.cur != 1 {
		t.Fatalf("typed filter then pick: %q cur %d", f.get("c"), f.cur)
	}
	f.update(keyUp)
	f.update(keySpace)
	f.update(keyDown)
	f.update(keyEsc)
	if f.get("c") != "col_31" || f.drop != nil {
		t.Fatal("esc keeps the value")
	}
	f.update(keyRight)
	if f.get("c") != "col_32" {
		t.Fatal("right cycles")
	}
	// a typed value is a row only on a combo
	f.update(keyEnter)
	typeKeys(f, "nope")
	f.update(keyEnter)
	if f.get("c") != "col_32" {
		t.Fatalf("typed value taken by a fixed choice: %q", f.get("c"))
	}
	g := newForm("t", combo("o", "O", "", "", "orders").empty("stream name"))
	if v := strings.Join(g.view(60), "\n"); !strings.Contains(v, "[stream name]") {
		t.Fatalf("empty label:\n%s", v)
	}
	g.update(keyEnter)
	typeKeys(g, "orders_copy")
	g.update(keyEnter)
	if g.get("o") != "orders_copy" {
		t.Fatalf("combo takes a typed value: %q", g.get("o"))
	}
}

func TestStreamFieldsRoundTrip(t *testing.T) {
	api := config.Stream{
		Mode: "incremental", Path: "/posts", Paginate: "page", PageSize: 10, PageParam: "_page", SizeParam: "_limit",
		UpdateKey: "nonempty_at", PrimaryKey: config.Names{"id"}, SinceParam: "since", SinceFormat: "unix",
		Query: map[string]string{"a": "1", "b": "2"}, SkipOn: []int{404}, Limit: 30, ForEach: "users",
		AddColumns: map[string]string{"uid": "{users.id}"}, Records: "data[]", Sorted: "desc",
	}
	f := newForm("t", streamFields(api, nil, true)...)
	if got := streamFromFields(f, api); !equalStream(got, api) {
		t.Fatalf("api round trip:\n%+v\n%+v", got, api)
	}
	f.set("paginate", "") // page fields go, since they were shown
	got := streamFromFields(f, api)
	if got.PageParam != "" || got.SizeParam != "" || got.PageSize != 0 || got.UpdateKey != "nonempty_at" {
		t.Fatalf("hidden by a change: %+v", got)
	}
	tbl := config.Stream{Mode: "full-refresh", Select: config.Names{"-notes"}, Where: "v <> 'none'", Limit: 5, QueryTimeout: "30s", Steps: "keep id", SQL: "select 1"}
	f = newForm("t", streamFields(tbl, []string{"id", "v", "notes"}, false)...)
	if f.field("select") != nil || f.field("sql") != nil || f.field("steps") != nil {
		t.Fatal("sql, select and steps are shaped in explore, not on the form")
	}
	if got := streamFromFields(f, tbl); !equalStream(got, tbl) {
		t.Fatalf("table round trip keeps them:\n%+v\n%+v", got, tbl)
	}
	// the default stays unwritten
	plain := config.Stream{}
	f = newForm("t", streamFields(plain, nil, true)...)
	if got := streamFromFields(f, plain); got.Method != "" || got.Mode != "" || got.SinceFormat != "" {
		t.Fatalf("defaults written: %+v", got)
	}
}

func equalStream(a, b config.Stream) bool {
	return fmt.Sprintf("%+v", a.Values()) == fmt.Sprintf("%+v", b.Values())
}

func TestConnWizardCreatesAndEdits(t *testing.T) {
	opts := testHome(t)
	w, err := newConnWizard(opts, "")
	if err != nil {
		t.Fatal(err)
	}
	f := w.form
	typeKeys(f, "NEW")
	f.set("type", "sqlite")
	f.set("path", "src.db") // relative to the connections file
	res, cmd := w.update(keyTest)
	if res != wizContinue || w.step != connTest || cmd == nil {
		t.Fatalf("ctrl+g should probe: step=%d", w.step)
	}
	msg := cmd()
	probe, ok := msg.(connProbeMsg)
	if !ok || probe.Err != nil || len(probe.Tables) != 1 || probe.Tables[0] != "things" {
		t.Fatalf("probe: %+v", msg)
	}
	w.update(msg)
	if v := strings.Join(w.view(80, 20), "\n"); !strings.Contains(v, "connected") || !strings.Contains(v, "things") {
		t.Fatalf("probe view:\n%s", v)
	}
	if res, _ := w.update(keySave); res != wizDone {
		t.Fatal("save")
	}
	c, _ := config.LoadConnectionsRaw(opts.Connections)
	if c.Conn["NEW"] == nil || c.Conn["NEW"].Type != "sqlite" || c.Conn["NEW"].Path != "src.db" || c.Conn["PG"] == nil {
		t.Fatalf("written: %+v", c.Conn)
	}

	// duplicate name is refused
	w2, _ := newConnWizard(opts, "")
	typeKeys(w2.form, "NEW")
	w2.update(keySave)
	if w2.form.field("name").err == "" {
		t.Fatal("duplicate name accepted")
	}

	// edit with rename, api with bearer
	w3, _ := newConnWizard(opts, "NEW")
	if w3.form.get("name") != "NEW" || w3.form.get("type") != "sqlite" {
		t.Fatal("not prefilled")
	}
	w3.form.set("name", "API2")
	w3.form.set("type", "api")
	w3.form.set("base_url", "http://127.0.0.1:1")
	w3.form.set("auth", "bearer")
	w3.form.set("token", "${API2_TOKEN}")
	w3.form.set("rate_limit", "bogus")
	if res, _ := w3.update(keySave); res != wizContinue || w3.form.field("rate_limit").err == "" {
		t.Fatal("rate limit validation")
	}
	w3.form.set("rate_limit", "10/s")
	if res, _ := w3.update(keySave); res != wizDone {
		t.Fatalf("save: %s", w3.saveErr)
	}
	c, _ = config.LoadConnectionsRaw(opts.Connections)
	if c.Conn["NEW"] != nil || c.Conn["API2"] == nil || c.Conn["API2"].Token != "${API2_TOKEN}" || c.Conn["API2"].RateLimit != "10/s" {
		t.Fatalf("renamed: %+v", c.Conn)
	}
}

func TestConnRenameUpdatesReplications(t *testing.T) {
	opts := testHome(t)
	w, _ := newConnWizard(opts, "SRC")
	w.form.set("name", "SRC2")
	if res, _ := w.update(keySave); res != wizContinue || w.ask == nil || !strings.Contains(w.ask.text, "alpha") {
		t.Fatalf("rename should ask: %+v", w.ask)
	}
	if l, _ := w.footer(); !strings.Contains(l, "y/n") {
		t.Fatalf("footer: %s", l)
	}
	if res, _ := w.update(keyRunes("y")); res != wizDone {
		t.Fatal("y saves")
	}
	if !strings.Contains(repFile(t, opts, "alpha"), `source = "SRC2"`) {
		t.Fatalf("alpha not updated:\n%s", repFile(t, opts, "alpha"))
	}
	c, _ := config.LoadConnectionsRaw(opts.Connections)
	if c.Conn["SRC2"] == nil || c.Conn["SRC"] != nil {
		t.Fatalf("connections: %+v", c.Conn)
	}
	// n saves the connection only and says so
	w, _ = newConnWizard(opts, "SRC2")
	w.form.set("name", "SRC3")
	w.update(keySave)
	if res, _ := w.update(keyRunes("n")); res != wizDone || !w.done.IsErr || !strings.Contains(w.done.Text, "still uses SRC2") {
		t.Fatalf("n: %+v", w.done)
	}
	// keys the form does not show stay
	c.Conn["API"] = &config.Conn{Type: "api", BaseURL: "http://127.0.0.1:1", Timeout: "30s", Headers: map[string]string{"X-A": "1"}}
	config.WriteConnections(opts.Connections, c)
	w, _ = newConnWizard(opts, "API")
	w.form.set("rate_limit", "5/s")
	if res, _ := w.update(keySave); res != wizDone {
		t.Fatal(w.saveErr)
	}
	c, _ = config.LoadConnectionsRaw(opts.Connections)
	if a := c.Conn["API"]; a.Timeout != "30s" || a.Headers["X-A"] != "1" || a.RateLimit != "5/s" {
		t.Fatalf("api: %+v", a)
	}
}

func TestRepWizardFlow(t *testing.T) {
	opts := testHome(t)
	conns, _ := config.LoadConnections(opts.Connections)
	w, err := newRepWizard(opts, conns, "")
	if err != nil {
		t.Fatal(err)
	}
	if w.basics.get("target") == w.basics.get("source") {
		t.Fatalf("target defaults to another connection: %q %q", w.basics.get("source"), w.basics.get("target"))
	}
	w.basics.set("name", "alpha") // exists
	if res := send(t, w, keySave); res != wizContinue || w.basics.field("name").err == "" {
		t.Fatal("existing name accepted")
	}
	w.basics.set("name", "fresh")
	w.basics.set("source", "SRC")
	w.basics.set("target", "TGT")
	w.basics.set("schedule", "custom")
	w.basics.set("cron", "nonsense")
	if res := send(t, w, keySave); res != wizContinue || w.basics.field("cron").err == "" {
		t.Fatal("bad cron accepted")
	}
	w.basics.set("cron", "@every 10m")
	walk(t, w)
	if w.step != stepDefaults || w.draft.Schedule != "@every 10m" {
		t.Fatalf("basics applied: step=%d %+v", w.step, w.draft)
	}
	w.defaults.set("mode", "incremental")
	w.defaults.set("primary_key", "id")
	w.defaults.set("exclude", "tmp_*, x_*")
	walk(t, w)
	if w.step != stepStreams || len(w.draft.Exclude) != 2 || w.srcTables == nil {
		t.Fatalf("defaults applied: %+v tables %v", w.draft, w.srcTables)
	}
	// the table list: schema patterns first, filter as you type
	send(t, w, keyRunes("a"))
	if w.pick == nil || !slices.Contains(w.pick.items, "things") || !slices.Contains(w.pick.items, "*") {
		t.Fatalf("picker: %+v", w.pick)
	}
	for _, r := range "thi" {
		send(t, w, keyRunes(string(r)))
	}
	send(t, w, keySpace, keyEnter)
	if len(w.draft.Streams) != 1 || w.draft.Streams[0].Name != "things" || w.pick != nil {
		t.Fatalf("streams: %+v", w.draft.Streams)
	}
	// a name that is no table is a row of its own
	send(t, w, keyRunes("a"))
	for _, r := range "sales.*" {
		send(t, w, keyRunes(string(r)))
	}
	if v := strings.Join(w.view(100, 20), "\n"); !strings.Contains(v, "sales.*") || !strings.Contains(v, "new") {
		t.Fatalf("typed row:\n%s", v)
	}
	send(t, w, keyEnter)
	if len(w.draft.Streams) != 2 || w.draft.Streams[1].Name != "sales.*" {
		t.Fatalf("typed stream: %+v", w.draft.Streams)
	}
	send(t, w, keyRunes("E"))
	sf := w.stream
	if sf == nil || sf.get("name") != "sales.*" || sf.get("mode") != "incremental" {
		t.Fatalf("edit form: %+v", sf)
	}
	sf.set("update_key", "updated_at")
	sf.set("mode", "snapshot")
	send(t, w, keySave)
	// a real table: columns are looked up and the keys become lists
	send(t, w, keyRunes("g"), keyRunes("E"))
	tf := w.stream
	if tf == nil || tf.field("primary_key").Kind != kindMulti || tf.field("update_key").Kind != kindSelect {
		t.Fatalf("table stream editor should use lists: %+v", tf)
	}
	if got := strings.Join(tf.field("primary_key").Options, ","); got != "id,v" {
		t.Fatalf("columns: %s", got)
	}
	if tf.field("where") == nil || tf.field("limit") == nil || tf.field("sql") != nil || tf.field("select") != nil {
		t.Fatal("where and limit belong on the form; sql and columns in explore")
	}
	tf.field("primary_key").Picked["id"] = true
	send(t, w, keySave)
	if st := w.draft.Streams[0].Stream; strings.Join(st.PrimaryKey, ",") != "id" {
		t.Fatalf("picked keys: %+v", st)
	}
	if s := w.draft.Streams[1].Stream; s.UpdateKey != "updated_at" || s.Mode != "snapshot" {
		t.Fatalf("override: %+v", s)
	}
	if v := strings.Join(w.view(100, 20), "\n"); !strings.Contains(v, "update_key=updated_at") {
		t.Fatalf("overrides column:\n%s", v)
	}
	// e turns a stream off; d asks; backspace only goes back
	send(t, w, keyRunes("e"))
	if !w.draft.Streams[0].Stream.Disabled {
		t.Fatal("e: stream off")
	}
	send(t, w, keyRunes("e"), keyRunes("d"))
	if w.ask == nil {
		t.Fatal("d asks")
	}
	send(t, w, keyRunes("n"))
	if len(w.draft.Streams) != 2 {
		t.Fatal("n keeps the stream")
	}
	send(t, w, keyBack)
	if w.step != stepDefaults || len(w.draft.Streams) != 2 {
		t.Fatalf("backspace: step %d, %d streams", w.step, len(w.draft.Streams))
	}
	walk(t, w)
	// review and save
	send(t, w, keySave)
	if w.step != stepReview || w.reviewErr != "" {
		t.Fatalf("review: step=%d err=%q", w.step, w.reviewErr)
	}
	if res := send(t, w, keySave); res != wizDone {
		t.Fatalf("save: %q", w.reviewErr)
	}
	r, err := config.LoadReplication(filepath.Join(config.ReplicationsDir(opts.Home), "fresh.toml"))
	if err != nil || r.Schedule != "@every 10m" || len(r.Resolved) != 2 || r.Resolved[1].Mode != "snapshot" || r.Resolved[0].PrimaryKey[0] != "id" {
		t.Fatalf("written: %v %+v", err, r)
	}

	// edit existing: prefilled; a rename keeps the file's comments and removes the old file
	os.WriteFile(filepath.Join(config.ReplicationsDir(opts.Home), "fresh.toml"), []byte("# note\n"+repFile(t, opts, "fresh")), 0o644)
	w2, err := newRepWizard(opts, conns, "fresh")
	if err != nil || w2.basics.get("schedule") != "custom" || w2.basics.get("cron") != "@every 10m" {
		t.Fatalf("prefill: %v %q", err, w2.basics.get("schedule"))
	}
	w2.basics.set("name", "renamed")
	send(t, w2, keySave)
	if w2.step != stepReview || !strings.Contains(strings.Join(w2.view(100, 30), "\n"), "was fresh.toml") {
		t.Fatalf("review of a rename:\n%s", strings.Join(w2.view(100, 30), "\n"))
	}
	if res := send(t, w2, keySave); res != wizDone {
		t.Fatalf("resave: %q", w2.reviewErr)
	}
	if _, err := os.Stat(filepath.Join(config.ReplicationsDir(opts.Home), "fresh.toml")); err == nil {
		t.Fatal("old file kept after rename")
	}
	if !strings.HasPrefix(repFile(t, opts, "renamed"), "# note\n") {
		t.Fatalf("comment lost:\n%s", repFile(t, opts, "renamed"))
	}

	// invalid draft is blocked at review
	w3, _ := newRepWizard(opts, conns, "")
	w3.basics.set("name", "bad")
	w3.basics.set("source", "SRC")
	walk(t, w3)
	w3.defaults.set("mode", "incremental")
	walk(t, w3)
	send(t, w3, keyRunes("a"))
	for _, r := range "things" {
		send(t, w3, keyRunes(string(r)))
	}
	send(t, w3, keyEnter, keySave)
	if w3.reviewErr == "" || !strings.Contains(w3.reviewErr, "incremental requires") {
		t.Fatalf("review error: %q", w3.reviewErr)
	}
	if res := send(t, w3, keySave); res != wizContinue {
		t.Fatal("saved an invalid draft")
	}
}

func TestEditKeepsStreamSettings(t *testing.T) {
	opts := testHome(t)
	path := filepath.Join(config.ReplicationsDir(opts.Home), "hr.toml")
	os.WriteFile(path, []byte(`# hr
source = "SRC"
target = "TGT"
mode = "full-refresh"
object = "x_{stream_table}"
schedule = "*/10 * * * *"

[streams.things]
select = ["id", "v"]
where = "v <> 'z'"
limit = 5
query_timeout = "30s"
`), 0o644)
	conns, _ := config.LoadConnections(opts.Connections)
	w, _ := newRepWizard(opts, conns, "hr")
	send(t, w, keySave)
	if w.step != stepReview || w.changes != 0 || !strings.Contains(strings.Join(w.view(100, 30), "\n"), "no changes") {
		t.Fatalf("untouched edit shows changes: %d\n%s", w.changes, strings.Join(w.view(100, 30), "\n"))
	}
	// E on the stream in the detail view: its own values, where and limit included
	w2, err := newEditStreamWizard(opts, conns, "hr", "things")
	if err != nil {
		t.Fatal(err)
	}
	feed(t, w2, w2.start())
	f := w2.stream
	if f == nil || f.get("where") != "v <> 'z'" || f.get("limit") != "5" || f.get("query_timeout") != "30s" {
		t.Fatalf("form from the stream: %+v", f)
	}
	f.set("limit", "7")
	if res := send(t, w2, keySave); res != wizDone {
		t.Fatalf("save: %q", f.err)
	}
	text := repFile(t, opts, "hr")
	for _, want := range []string{"# hr", `where = "v <> 'z'"`, "limit = 7", `query_timeout = "30s"`, `select = ["id", "v"]`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "object") != 1 || strings.Count(text, "mode") != 1 {
		t.Errorf("defaults copied into the stream:\n%s", text)
	}
}

func TestEditKeepsAPIParams(t *testing.T) {
	opts := testHome(t)
	raw, _ := config.LoadConnectionsRaw(opts.Connections)
	raw.Conn["API"] = &config.Conn{Type: "api", BaseURL: "http://127.0.0.1:1"}
	config.WriteConnections(opts.Connections, raw)
	os.WriteFile(filepath.Join(config.ReplicationsDir(opts.Home), "jp.toml"), []byte(`source = "API"
target = "TGT"

[streams.posts_paged]
path = "/posts"
paginate = "page"
page_param = "_page"
size_param = "_limit"
page_size = 10
update_key = "nonempty_at"
mode = "incremental"
`), 0o644)
	conns, _ := config.LoadConnections(opts.Connections)
	w, err := newEditStreamWizard(opts, conns, "jp", "posts_paged")
	if err != nil {
		t.Fatal(err)
	}
	feed(t, w, w.start())
	w.stream.set("limit", "50")
	if res := send(t, w, keySave); res != wizDone {
		t.Fatalf("save: %q", w.stream.err)
	}
	text := repFile(t, opts, "jp")
	for _, want := range []string{`page_param = "_page"`, `size_param = "_limit"`, "page_size = 10", `update_key = "nonempty_at"`, "limit = 50"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "method") || strings.Contains(text, "since_format") {
		t.Errorf("defaults written:\n%s", text)
	}
}

func TestAddFromDetailNeverRemoves(t *testing.T) {
	opts := testHome(t)
	conns, _ := config.LoadConnections(opts.Connections)
	before := repFile(t, opts, "alpha")
	w, _ := newAddStreamWizard(opts, conns, "alpha")
	feed(t, w, w.start())
	if w.pick == nil || !w.pick.locked["things"] {
		t.Fatalf("picker with the streams locked: %+v", w.pick)
	}
	for _, r := range "things" {
		send(t, w, keyRunes(string(r)))
	}
	send(t, w, keySpace) // locked: stays
	if !w.pick.selected["things"] {
		t.Fatal("existing stream unticked")
	}
	if res := send(t, w, keyEnter); res != wizCancel || repFile(t, opts, "alpha") != before {
		t.Fatalf("nothing new: leave without writing: %v %v\n%s", res, w.pick != nil, repFile(t, opts, "alpha"))
	}
	// a new table goes through the review first
	w, _ = newAddStreamWizard(opts, conns, "alpha")
	feed(t, w, w.start())
	send(t, w, keyRunes("*"), keySpace, keyEnter)
	if w.step != stepReview || repFile(t, opts, "alpha") != before {
		t.Fatalf("review before writing: step %d", w.step)
	}
	if v := strings.Join(w.view(100, 30), "\n"); !strings.Contains(v, "+ [streams.") {
		t.Fatalf("diff:\n%s", v)
	}
	send(t, w, keyEsc) // back to the list, ticks kept
	if w.pick == nil || !w.pick.selected["*"] || len(w.draft.Streams) != 2 {
		t.Fatalf("back from review: %+v", w.draft.Streams)
	}
	send(t, w, keyEnter)
	if res := send(t, w, keySave); res != wizDone {
		t.Fatalf("save: %s", w.reviewErr)
	}
	r, _ := config.LoadReplication(filepath.Join(config.ReplicationsDir(opts.Home), "alpha.toml"))
	if len(r.Resolved) != 3 || r.Resolved[0].Name != "things" || r.Resolved[1].Name != "copy" {
		t.Fatalf("streams: %+v", r.Resolved)
	}
}

func TestRepWizardAPIStream(t *testing.T) {
	opts := testHome(t)
	raw, _ := config.LoadConnectionsRaw(opts.Connections)
	raw.Conn["API"] = &config.Conn{Type: "api", BaseURL: "http://127.0.0.1:1"}
	config.WriteConnections(opts.Connections, raw)
	conns, _ := config.LoadConnections(opts.Connections)
	w, _ := newRepWizard(opts, conns, "")
	w.basics.set("name", "apirep")
	w.basics.set("source", "API")
	w.basics.set("target", "TGT")
	walk(t, w)
	if w.defaults.field("records") == nil || w.defaults.field("exclude") != nil {
		t.Fatal("api defaults: records, no exclude")
	}
	w.defaults.set("records", "data[]")
	walk(t, w)
	send(t, w, keyRunes("a"))
	f := w.stream
	f.set("name", "devices")
	f.set("path", "/devices")
	f.set("paginate", "page")
	f.set("page_size", "50")
	send(t, w, keySave)
	send(t, w, keyRunes("a"))
	f = w.stream
	if !slices.Contains(f.field("for_each").Options, "devices") {
		t.Fatalf("for_each options: %v", f.field("for_each").Options)
	}
	f.set("name", "locations")
	f.set("path", "/devices/{devices.id}/locations")
	f.set("for_each", "devices")
	f.set("add_columns", "device_id = {devices.id}")
	f.set("query", "x = 1")
	send(t, w, keySave, keySave)
	if w.reviewErr != "" || w.step != stepReview {
		t.Fatal(w.reviewErr)
	}
	if res := send(t, w, keySave); res != wizDone {
		t.Fatal("save")
	}
	r, err := config.LoadReplication(filepath.Join(config.ReplicationsDir(opts.Home), "apirep.toml"))
	if err != nil {
		t.Fatal(err)
	}
	loc := r.Resolved[1]
	if loc.ForEach != "devices" || loc.AddColumns["device_id"] != "{devices.id}" || loc.Query["x"] != "1" || loc.Records != "data[]" || r.Resolved[0].PageSize != 50 {
		t.Fatalf("api stream: %+v", loc.Stream)
	}
}

func TestAddStreamFromDetail(t *testing.T) {
	opts := testHome(t)
	raw, _ := config.LoadConnectionsRaw(opts.Connections)
	raw.Conn["API"] = &config.Conn{Type: "api", BaseURL: "http://127.0.0.1:1"}
	config.WriteConnections(opts.Connections, raw)
	conns, _ := config.LoadConnections(opts.Connections)
	draft := config.ReplicationDraft{Name: "quick", Source: "API", Target: "TGT", Enabled: true, Defaults: config.Stream{Mode: "full-refresh"},
		Streams: []config.StreamDraft{{Name: "users", Stream: config.Stream{Path: "/users"}}}}
	if _, err := config.WriteReplication(opts.Home, draft); err != nil {
		t.Fatal(err)
	}
	w, err := newAddStreamWizard(opts, conns, "quick")
	if err != nil {
		t.Fatal(err)
	}
	feed(t, w, w.start())
	if w.step != stepStreams || w.stream == nil || w.title() != "add to quick" {
		t.Fatalf("opens on the stream form: step=%d", w.step)
	}
	f := w.stream
	f.set("name", "users")
	if res := send(t, w, keySave); res != wizContinue || f.field("name").err != "exists" {
		t.Fatal("duplicate name")
	}
	f.set("name", "posts")
	f.set("path", "/posts")
	if res := send(t, w, keySave); res != wizDone || w.saved() != "quick" {
		t.Fatalf("submit saves: %v", res)
	}
	r, err := config.LoadReplication(filepath.Join(opts.Home, "replications", "quick.toml"))
	if err != nil {
		t.Fatal(err)
	}
	d := config.DraftOf(r)
	if len(d.Streams) != 2 || d.Streams[1].Name != "posts" || d.Streams[1].Stream.Path != "/posts" || d.Streams[0].Stream.Path != "/users" {
		t.Fatalf("file: %+v", d.Streams)
	}
	w, _ = newAddStreamWizard(opts, conns, "quick")
	feed(t, w, w.start())
	if res := send(t, w, keyEsc); res != wizCancel {
		t.Fatal("esc leaves")
	}
}

func TestNewConnectionFromSourceList(t *testing.T) {
	opts := testHome(t)
	conns, _ := config.LoadConnections(opts.Connections)
	w, _ := newRepWizard(opts, conns, "")
	send(t, w, keyDown, keyEnter) // source list
	for _, r := range "new conn" {
		send(t, w, keyRunes(string(r)))
	}
	send(t, w, keyEnter)
	if w.child == nil || w.childField != "source" {
		t.Fatal("new connection… opens the connection form")
	}
	typeKeys(w.child.form, "FRESH")
	w.child.form.set("type", "sqlite")
	w.child.form.set("path", "src.db")
	send(t, w, keySave)
	if w.child != nil || w.basics.get("source") != "FRESH" {
		t.Fatalf("source after the new connection: %q", w.basics.get("source"))
	}
}

func TestModelOpensWizards(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "n")
	if m.view != viewWizard || m.wiz == nil || !strings.Contains(m.View(), "New replication") {
		t.Fatalf("n:\n%s", m.View())
	}
	m = press(m, "esc")
	if m.view != viewList || m.wiz != nil {
		t.Fatal("esc leaves an untouched wizard")
	}
	m = press(m, "E")
	if !strings.Contains(m.View(), "Edit replication alpha") {
		t.Fatalf("E:\n%s", m.View())
	}
	m = press(m, "x", "esc") // typed into the name: asks
	if m.ask == nil || !strings.Contains(m.View(), "discard changes? y/n") {
		t.Fatalf("discard prompt:\n%s", m.View())
	}
	m = press(m, "n")
	if m.wiz == nil || m.ask != nil {
		t.Fatal("n stays in the wizard")
	}
	m = press(m, "esc")
	mm, cmd := m.Update(keyRunes("y"))
	mm, _ = mm.(Model).Update(cmd())
	m = mm.(Model)
	if m.wiz != nil || m.view != viewList {
		t.Fatal("y discards")
	}
	m = press(m, "c", "n")
	if !strings.Contains(m.View(), "New connection") {
		t.Fatalf("conn n:\n%s", m.View())
	}
	m = press(m, "esc", "enter")
	if !strings.Contains(m.View(), "explore PG") {
		t.Fatalf("explore:\n%s", m.View())
	}
	m = press(m, "esc", "d")
	if m.ask == nil || !strings.Contains(m.View(), "delete connection PG? used by beta") {
		t.Fatalf("delete prompt:\n%s", m.View())
	}
	m = press(m, "n")
	if m.ask != nil {
		t.Fatal("n should decline")
	}
}

func TestHelpInWizards(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "n")
	m = press(m, "?") // the name field types it
	if m.help != nil {
		t.Fatal("? on a text field types")
	}
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyF1})
	m = mm.(Model)
	if m.help == nil || !strings.Contains(m.View(), "ctrl+s") {
		t.Fatalf("f1 help:\n%s", m.View())
	}
	m = press(m, "esc")
	mm, _ = m.Update(keyDown)
	m = press(mm.(Model), "?") // source: a list
	if m.help == nil {
		t.Fatal("? on a list opens help")
	}
	mm, _ = m.Update(keyRight)
	m = mm.(Model)
	if v := m.View(); !strings.Contains(v, "open list") {
		t.Fatalf("forms page:\n%s", v)
	}
}

func TestNoConnectionsOpensConnectionForm(t *testing.T) {
	opts := testHome(t)
	os.Remove(opts.Connections)
	m := loaded(t, opts)
	m = press(m, "n")
	if _, ok := m.wiz.(*connWizard); !ok || !strings.Contains(m.View(), "no connections") {
		t.Fatalf("n without connections:\n%s", m.View())
	}
}

func TestDeleteFlows(t *testing.T) {
	opts := testHome(t)
	m := loaded(t, opts)
	m = press(m, "c", "G", "d")
	if m.ask == nil {
		t.Fatal("no prompt")
	}
	mm, cmd := m.Update(keyRunes("y"))
	m = mm.(Model)
	if d, ok := cmd().(deletedMsg); !ok || d.IsErr {
		t.Fatalf("delete: %+v", d)
	}
	c, _ := config.LoadConnectionsRaw(opts.Connections)
	if c.Conn["TGT"] != nil {
		t.Fatalf("TGT not deleted: %v", c.Names())
	}
	m = loaded(t, opts)
	m = press(m, "d")
	_, cmd = m.Update(keyRunes("y"))
	if d, ok := cmd().(deletedMsg); !ok || d.IsErr {
		t.Fatalf("delete: %+v", d)
	}
	if _, err := os.Stat(filepath.Join(config.ReplicationsDir(opts.Home), "alpha.toml")); err == nil {
		t.Fatal("alpha.toml not deleted")
	}
}

func TestDetailCursorFollowsSelection(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "enter") // alpha: streams things, copy
	rows := func() (int, int) {
		lines := strings.Split(m.View(), "\n")
		thingsRow, copyRow := -1, -1
		for i, l := range lines {
			switch {
			case strings.HasPrefix(l, "› things "):
				thingsRow = i
			case strings.HasPrefix(l, "› copy "):
				copyRow = i
			}
		}
		return thingsRow, copyRow
	}
	if a, b := rows(); a < 0 || b >= 0 {
		t.Fatalf("cursor should start on things:\n%s", m.View())
	}
	m = press(m, "j")
	if a, b := rows(); a >= 0 || b < 0 {
		t.Fatalf("cursor should move to copy:\n%s", m.View())
	}
	m = press(m, "j") // clamps at the last row
	if _, b := rows(); b < 0 {
		t.Fatalf("cursor should stay on copy:\n%s", m.View())
	}
	m = press(m, "k", "k")
	if a, _ := rows(); a < 0 {
		t.Fatalf("cursor should be back on things:\n%s", m.View())
	}
}

func TestDetailEditOpensTheStream(t *testing.T) {
	m := loaded(t, testHome(t))
	m = press(m, "enter", "j", "E") // alpha › copy
	w, ok := m.wiz.(*repWizard)
	if !ok || w.entry != entryEdit || w.stream == nil || w.stream.field("sql") != nil || w.stream.get("mode") == "" {
		t.Fatalf("E on a stream: %+v", m.wiz)
	}
}

func TestWizardEnterExploresDraftStream(t *testing.T) {
	opts := testHome(t)
	conns, _ := config.LoadConnections(opts.Connections)
	w, err := newRepWizard(opts, conns, "")
	if err != nil {
		t.Fatal(err)
	}
	w.basics.set("name", "draft")
	w.basics.set("source", "SRC")
	w.basics.set("target", "TGT")
	walk(t, w)
	walk(t, w)
	send(t, w, keyRunes("a"))
	for _, r := range "things" {
		send(t, w, keyRunes(string(r)))
	}
	send(t, w, keyEnter)
	if w.step != stepStreams || len(w.draft.Streams) != 1 || w.draft.Streams[0].Name != "things" {
		t.Fatalf("setup: step %d streams %+v", w.step, w.draft.Streams)
	}
	// ctrl+e on the picker previews the highlighted table and comes back to the picker
	send(t, w, keyRunes("a"))
	send(t, w, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyCtrlE}) // past the * row
	if w.explore == nil || w.explore.streamName != "things" || w.pick == nil {
		t.Fatalf("ctrl+e: explore %v pick %v", w.explore, w.pick)
	}
	send(t, w, keyEsc)
	if w.explore != nil || w.pick == nil {
		t.Fatal("esc should return to the picker")
	}
	send(t, w, keyEsc)
	// a source change lists tables again
	w.step = stepBasics
	before := w.srcTables
	w.basics.set("source", "TGT")
	w.applyBasics()
	if w.srcTables != nil || before == nil {
		t.Fatal("source change should drop the table list")
	}
	w.basics.set("source", "SRC")
	w.applyBasics()
	walk(t, w)
	walk(t, w)
	send(t, w, keyRunes("g"), keyEnter) // first stream: things
	e := w.explore
	if e == nil || e.bound != &w.draft || e.page != pagePreview || e.streamName != "things" {
		t.Fatalf("enter should explore the draft stream: %+v", e)
	}
	run(e, e.start())
	e.update(keyRight)
	e.update(keySpace) // untick v
	send(t, w, keySave)
	if !w.touched || strings.Join(w.draft.Streams[0].Stream.Select, ",") != "id" {
		t.Fatalf("ctrl+s should save into the draft: %+v", w.draft.Streams[0].Stream)
	}
	send(t, w, keyEsc)
	if w.explore != nil || w.step != stepStreams {
		t.Fatalf("esc returns to the streams step: %v %d", w.explore, w.step)
	}
	// s: a query on the source, + adds it to the draft
	send(t, w, keyRunes("s"))
	e = w.explore
	if e == nil || !e.editSQL || e.bound != &w.draft {
		t.Fatalf("s should open the sql editor on the draft: %+v", e)
	}
	e.sqlInput.SetValue("select id from things where id > 1")
	press1(e, keySave)
	if len(e.cols) != 1 || e.fetchErr != "" {
		t.Fatalf("sql preview: %+v %s", e.cols, e.fetchErr)
	}
	e.update(keyRunes("+"))
	if e.page != pageStream || e.stream == nil {
		t.Fatalf("+ should open the stream form on the draft: page %d", e.page)
	}
	e.stream.set("name", "big_things")
	send(t, w, keySave)
	send(t, w, keyEsc)
	if w.explore != nil || w.step != stepStreams {
		t.Fatalf("esc from the query returns to the streams step, not the table list: %v %d", w.explore, w.step)
	}
	if len(w.draft.Streams) != 2 || w.draft.Streams[1].Name != "big_things" || w.draft.Streams[1].Stream.SQL != "select id from things where id > 1" {
		t.Fatalf("query stream in the draft: %+v", w.draft.Streams)
	}
	if _, err := os.Stat(filepath.Join(config.ReplicationsDir(opts.Home), "draft.toml")); err == nil {
		t.Fatal("nothing written to disk")
	}
}

func TestDetailDefinitionPanel(t *testing.T) {
	opts := testHome(t)
	os.WriteFile(filepath.Join(config.ReplicationsDir(opts.Home), "def.toml"), []byte("source = \"SRC\"\ntarget = \"TGT\"\n[streams.things]\nwhere = \"id > 1\"\nsteps = \"keep id | sort -id\"\n"), 0o644)
	m := loaded(t, opts)
	m = press(m, "/")
	for _, r := range "def" {
		m = press(m, string(r))
	}
	m = press(m, "enter", "enter")
	v := m.View()
	for _, want := range []string{"table    things", "where    id > 1", "steps    keep id | sort -id"} {
		if !strings.Contains(v, want) {
			t.Errorf("definition missing %q:\n%s", want, v)
		}
	}
}

func TestConnWizardComposesURL(t *testing.T) {
	opts := testHome(t)
	w, _ := newConnWizard(opts, "")
	f := w.form
	typeKeys(f, "MS")
	f.set("type", "mssql")
	f.set("host", `srv\INST`)
	f.set("database", "db")
	f.set("fedauth", "ActiveDirectoryServicePrincipal")
	f.set("user", "app@tenant")
	f.set("password", "${SP_SECRET}")
	f.set("options", "encrypt = disable")
	if !f.validate() {
		t.Fatalf("validate: %v", formErrs(f))
	}
	want := "sqlserver://app%40tenant:${SP_SECRET}@srv/INST?database=db&encrypt=disable&fedauth=ActiveDirectoryServicePrincipal"
	if got := w.conn().URL; got != want {
		t.Fatalf("url\n got %s\nwant %s", got, want)
	}
	f.set("fedauth", "ActiveDirectoryDefault")
	f.set("user", "")
	if !f.validate() {
		t.Fatalf("default auth needs no user: %v", formErrs(f))
	}
	if got := w.conn().URL; got != "sqlserver://srv/INST?database=db&encrypt=disable&fedauth=ActiveDirectoryDefault" {
		t.Fatalf("url %s", got)
	}
	f.set("fedauth", "")
	if f.validate() {
		t.Fatal("sql login without user accepted")
	}
	f.set("user", "sa")
	f.set("password", "p@ss")
	f.set("port", "14330")
	if res, _ := w.update(keySave); res != wizDone {
		t.Fatalf("save: %v", formErrs(f))
	}

	// edit: fields come back from the url, and the written url is unchanged
	w2, _ := newConnWizard(opts, "MS")
	f2 := w2.form
	for k, want := range map[string]string{"host": `srv\INST`, "port": "14330", "database": "db", "user": "sa", "password": "p@ss", "options": "encrypt = disable", "fedauth": ""} {
		if got := f2.get(k); got != want {
			t.Errorf("%s: got %q want %q", k, got, want)
		}
	}
	if w2.conn().URL != w.conn().URL {
		t.Fatalf("edit changed url: %s", w2.conn().URL)
	}

	// a key=value DSN is edited as written
	c, _ := config.LoadConnectionsRaw(opts.Connections)
	c.Conn["MS"].URL = "server=srv;user id=sa;password=x"
	if err := config.WriteConnections(opts.Connections, c); err != nil {
		t.Fatal(err)
	}
	w3, _ := newConnWizard(opts, "MS")
	if h := w3.form.field("host"); h.Show(w3.form) || !w3.form.field("url").Show(w3.form) {
		t.Fatal("raw url should show the url field")
	}
	if w3.conn().URL != "server=srv;user id=sa;password=x" {
		t.Fatalf("raw url changed: %s", w3.conn().URL)
	}
}

func formErrs(f *form) map[string]string {
	out := map[string]string{}
	for _, fl := range f.visible() {
		if fl.err != "" {
			out[fl.Key] = fl.err
		}
	}
	return out
}

func TestReadOnlyConnNotATarget(t *testing.T) {
	opts := testHome(t)
	conns, _ := config.LoadConnectionsRaw(opts.Connections)
	conns.Conn["PG"].ReadOnly = true
	if err := config.WriteConnections(opts.Connections, conns); err != nil {
		t.Fatal(err)
	}
	conns, _ = config.LoadConnections(opts.Connections)
	if !conns.Conn["PG"].ReadOnly {
		t.Fatal("read_only not written")
	}
	w, err := newRepWizard(opts, conns, "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(w.basics.field("target").Options, "PG") || !slices.Contains(w.basics.field("source").Options, "PG") {
		t.Fatalf("source %v target %v", w.basics.field("source").Options, w.basics.field("target").Options)
	}
	cw, _ := newConnWizard(opts, "PG")
	if !cw.form.on("read_only") {
		t.Fatal("form toggle off")
	}
}

func TestTabPastTheEndMovesSteps(t *testing.T) {
	opts := testHome(t)
	conns, _ := config.LoadConnections(opts.Connections)
	w, _ := newRepWizard(opts, conns, "")
	w.start() // the app always starts a wizard; start sets the entry's key behaviour
	w.basics.set("name", "tabby")
	w.basics.set("source", "SRC")
	w.basics.set("target", "TGT")
	send(t, w, keyDown)
	if w.step != stepBasics || w.basics.cur != 1 {
		t.Fatal("down moves a field")
	}
	send(t, w, keyTab)
	if w.step != stepDefaults {
		t.Fatalf("tab from any field: step %d", w.step)
	}
	send(t, w, tea.KeyMsg{Type: tea.KeyShiftTab})
	if w.step != stepBasics {
		t.Fatalf("shift+tab: step %d", w.step)
	}
	// a stream edited on its own (E on the detail view) keeps tab as a field key
	w2, _ := newEditStreamWizard(opts, conns, "alpha", "copy") // a sql stream: its form opens at once
	run2(w2, w2.start())
	if w2.stream == nil || w2.stream.tabSteps || w2.basics.tabSteps {
		t.Fatal("single-stream wizard: tab is not a step key")
	}
	send(t, w, keyTab)
	walk(t, w) // defaults -> streams
	send(t, w, keyTab)
	if w.step != stepReview {
		t.Fatalf("tab on the streams step: %d", w.step)
	}
	send(t, w, tea.KeyMsg{Type: tea.KeyShiftTab})
	if w.step != stepStreams {
		t.Fatalf("shift+tab on review: %d", w.step)
	}
}

// run2 runs a command and feeds its message back, once.
func run2(w wizard, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				run2(w, c)
			}
			return
		}
		w.update(msg)
	}
}
