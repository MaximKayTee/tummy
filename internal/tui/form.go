package tui

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"tummy/internal/txt"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"tummy/internal/config"
)

type fieldKind int

const (
	kindText fieldKind = iota
	kindSecret
	kindSelect
	kindToggle
	kindMulti // several of Options; Picked holds the ticked ones
)

// field is one line of a form.
type field struct {
	Key      string
	Label    string
	Help     string
	HelpFn   func(f *form) string // help that depends on other fields; wins over Help
	Example  string               // shown before the help while the field is empty or wrong
	Kind     fieldKind
	Options  []string               // select, multi
	OptsFn   func(f *form) []string // select, multi: options worked out when the list opens
	Value    string                 // text, secret, select
	On       bool                   // toggle
	Picked   map[string]bool        // multi
	Notes    map[string]string      // dim text beside an option in the list; optionNotes when nil
	Empty    string                 // select: how the "" option reads, as none
	Free     bool                   // select: text that matches no option is offered as a value
	Fixed    bool                   // multi: only the options, no typed values
	Actions  []string               // select: rows that run something instead of setting the value
	Masked   bool                   // text: shown with passwords masked unless focused
	Invert   bool                   // toggle: On is shown as off, so a "disabled" key reads as Enabled
	Required bool
	ReqFn    func(f *form) bool // required when true; Required wins
	Validate func(string) error
	Show     func(f *form) bool // nil means always

	input  textinput.Model
	err    string
	shown0 bool // visible when the form opened
}

func text(key, label, help string) *field {
	return &field{Key: key, Label: label, Help: help, Kind: kindText}
}

func secret(key, label, help string) *field {
	return &field{Key: key, Label: label, Help: help, Kind: kindSecret}
}

func choice(key, label, help string, options ...string) *field {
	f := &field{Key: key, Label: label, Help: help, Kind: kindSelect, Options: options}
	if len(options) > 0 {
		f.Value = options[0]
	}
	return f
}

// combo is a choice that also takes a value typed in.
func combo(key, label, help string, options ...string) *field {
	f := choice(key, label, help, options...)
	f.Free = true
	return f
}

func multi(key, label, help string, options []string, picked []string) *field {
	f := &field{Key: key, Label: label, Help: help, Kind: kindMulti, Options: options, Picked: map[string]bool{}}
	for _, p := range picked {
		f.Picked[p] = true
	}
	return f
}

func toggle(key, label, help string, on bool) *field {
	return &field{Key: key, Label: label, Help: help, Kind: kindToggle, On: on}
}

func (f *field) required() *field                        { f.Required = true; return f }
func (f *field) requiredWhen(fn func(*form) bool) *field { f.ReqFn = fn; return f }
func (f *field) def(v string) *field                     { f.Value = v; return f }
func (f *field) when(fn func(*form) bool) *field {
	f.Show = fn
	return f
}
func (f *field) check(fn func(string) error) *field  { f.Validate = fn; return f }
func (f *field) notes(n map[string]string) *field    { f.Notes = n; return f }
func (f *field) empty(label string) *field           { f.Empty = label; return f }
func (f *field) free() *field                        { f.Free = true; return f }
func (f *field) fixed() *field                       { f.Fixed = true; return f }
func (f *field) masked() *field                      { f.Masked = true; return f }
func (f *field) helpFn(fn func(*form) string) *field { f.HelpFn = fn; return f }
func (f *field) actions(rows ...string) *field       { f.Actions = rows; return f }
func (f *field) eg(example string) *field            { f.Example = example; return f }
func (f *field) invert() *field                      { f.Invert = true; return f }
func (f *field) optsFn(fn func(*form) []string) *field {
	f.OptsFn = fn
	return f
}

// options are the field's choices now: OptsFn when set, else Options.
func (f *form) options(fl *field) []string {
	if fl.OptsFn != nil {
		return fl.OptsFn(f)
	}
	return fl.Options
}
func (f *field) display(v string) string {
	if v == "" && f.Empty != "" {
		return f.Empty
	}
	return v
}

// optionNotes describe the fixed vocabularies; a field's own Notes win.
var optionNotes = map[string]string{
	"full-refresh": "drop and reload",
	"truncate":     "empty, keep the table, reload",
	"incremental":  "new and changed rows by update key, upsert on primary key",
	"backfill":     "rows in the given range",
	"snapshot":     "append every run, _tummy_loaded_at",
	"postgres":     "PostgreSQL",
	"mssql":        "SQL Server",
	"mysql":        "MySQL, MariaDB",
	"sqlite":       "file",
	"api":          "HTTP JSON",
	"bearer":       "Authorization: Bearer <token>",
	"basic":        "user and password",
	"header":       "custom header",
	"query":        "key in the query string",
	"page":         "page number parameter",
	"offset":       "row offset parameter",
	"cursor":       "value from the response sent back",
	"link":         "Link header rel=next",
	"rfc3339":      "2026-01-02T15:04:05Z",
	"date":         "2026-01-02",
	"unix":         "seconds",
	"unix_ms":      "milliseconds",
	"raw":          "as stored",
	"desc":         "newest first, stop at the watermark",
	"asc":          "oldest first",
	"guided":       "column, operator, value",
	"contains":     "LIKE %x%",
	"starts with":  "LIKE x%",
	"ends with":    "LIKE %x",
	"in":           "one of several values",
	"int":          "bigint",
	"float":        "double",
	"decimal":      "decimal(18,4)",
	"text":         "varchar",
	"bool":         "boolean",
	"timestamp":    "date and time",
}

type form struct {
	title    string
	fields   []*field
	cur      int    // index of the focused field in visible()
	focused  *field // stays focused when fields before it appear or go
	drop     *picker
	offset   int    // first body line shown when the form is taller than the screen
	err      string // under the title, for the form as a whole
	note     string // dim line under the title
	action   string // "key:row" after an Actions row was picked; see takeAction
	init     map[string]string
	done     string // what enter on the last field does, for the footer; "" is apply
	tabSteps bool   // tab and shift+tab leave the form as formNext / formPrev; ↓/↑ move fields
}

const dropRows = 10

// cursorMode is the text cursor of forms and lists; tests use a static one.
var cursorMode = cursor.CursorBlink

func newInput() textinput.Model {
	ti := textinput.New()
	ti.Cursor.SetMode(cursorMode)
	return ti
}

func newForm(title string, fields ...*field) *form {
	f := &form{title: title, fields: fields}
	for _, fl := range fields {
		switch fl.Kind {
		case kindText, kindSecret:
			ti := newInput()
			ti.Prompt = ""
			ti.CharLimit = 4096
			ti.Width = 60
			ti.SetValue(fl.Value)
			if fl.Kind == kindSecret {
				ti.EchoMode = textinput.EchoPassword
			}
			fl.input = ti
		case kindMulti:
			if fl.Picked == nil {
				fl.Picked = map[string]bool{}
			}
		}
	}
	for _, fl := range f.visible() {
		fl.shown0 = true
	}
	f.init = f.values()
	f.focus()
	return f
}

// values is every field as text, to tell whether the form changed.
func (f *form) values() map[string]string {
	out := map[string]string{}
	for _, fl := range f.fields {
		switch fl.Kind {
		case kindToggle:
			out[fl.Key] = fmt.Sprint(fl.On)
		case kindMulti:
			out[fl.Key] = strings.Join(f.picked(fl.Key), "\x00")
		default:
			out[fl.Key] = f.get(fl.Key)
		}
	}
	return out
}

func (f *form) changed() bool { return !maps.Equal(f.values(), f.init) }

// fieldChanged reports whether one field differs from when the form opened.
func (f *form) fieldChanged(key string) bool { return f.values()[key] != f.init[key] }

func (f *form) get(key string) string {
	for _, fl := range f.fields {
		if fl.Key == key {
			switch fl.Kind {
			case kindSecret:
				return fl.input.Value() // spaces can be part of a password
			case kindText:
				return strings.TrimSpace(fl.input.Value())
			}
			return fl.Value
		}
	}
	return ""
}

func (f *form) on(key string) bool {
	for _, fl := range f.fields {
		if fl.Key == key {
			return fl.On
		}
	}
	return false
}

// set gives a field a value; a multi field takes a comma list.
func (f *form) set(key, v string) {
	for _, fl := range f.fields {
		if fl.Key == key {
			fl.Value = v
			switch fl.Kind {
			case kindText, kindSecret:
				fl.input.SetValue(v)
			case kindMulti:
				fl.Picked = map[string]bool{}
				for _, p := range splitList(v) {
					fl.Picked[p] = true
				}
			}
		}
	}
}

func (f *form) field(key string) *field {
	for _, fl := range f.fields {
		if fl.Key == key {
			return fl
		}
	}
	return nil
}

// shown reports whether a field of the form is visible now.
func (f *form) shown(key string) bool {
	fl := f.field(key)
	return fl != nil && (fl.Show == nil || fl.Show(f))
}

// picked returns the ticked options of a multi field in option order, then ticked values typed in.
func (f *form) picked(key string) []string {
	fl := f.field(key)
	if fl == nil {
		return nil
	}
	var out []string
	for _, o := range fl.Options {
		if fl.Picked[o] {
			out = append(out, o)
		}
	}
	var extra []string
	for v, on := range fl.Picked {
		if on && !slices.Contains(fl.Options, v) {
			extra = append(extra, v)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

func (f *form) visible() []*field {
	var out []*field
	for _, fl := range f.fields {
		if fl.Show == nil || fl.Show(f) {
			out = append(out, fl)
		}
	}
	return out
}

// sync points cur at the focused field after fields appeared or went. A focused field
// that went hides the focus on the next visible one.
func (f *form) sync() []*field {
	vis := f.visible()
	if len(vis) == 0 {
		return vis
	}
	if f.focused != nil {
		if i := slices.Index(vis, f.focused); i >= 0 {
			f.cur = i
		} else if at := slices.Index(f.fields, f.focused); at >= 0 {
			f.cur = len(vis) - 1
			for _, fl := range f.fields[at:] {
				if i := slices.Index(vis, fl); i >= 0 {
					f.cur = i
					break
				}
			}
		}
	}
	f.cur = min(max(f.cur, 0), len(vis)-1)
	f.focused = vis[f.cur]
	return vis
}

// focus syncs the focus and gives the keyboard to the focused text field.
func (f *form) focus() tea.Cmd {
	vis := f.sync()
	var cmd tea.Cmd
	for i, fl := range vis {
		if fl.Kind != kindText && fl.Kind != kindSecret {
			continue
		}
		if i == f.cur && f.drop == nil {
			cmd = fl.input.Focus()
		} else {
			fl.input.Blur()
		}
	}
	return cmd
}

func (f *form) move(delta int) tea.Cmd {
	vis := f.sync()
	if len(vis) == 0 {
		return nil
	}
	f.cur = min(max(f.cur+delta, 0), len(vis)-1)
	f.focused = vis[f.cur]
	return f.focus()
}

// typing reports whether keys go to text: a focused text field or an open list.
func (f *form) typing() bool {
	if f.drop != nil {
		return true
	}
	return f.focused != nil && (f.focused.Kind == kindText || f.focused.Kind == kindSecret)
}

// takeAction returns and clears the Actions row picked last, as "key:row".
func (f *form) takeAction() string {
	a := f.action
	f.action = ""
	return a
}

type formResult int

const (
	formContinue formResult = iota
	formSubmit
	formCancel
	formNext // tab on the last field: the host may move on
	formPrev // shift+tab on the first field: the host may move back
)

// update handles a key. Submit happens on ctrl+s, or enter on the last field. A change
// to any field clears the form's error.
func (f *form) update(msg tea.KeyMsg) (formResult, tea.Cmd) {
	if f.err == "" {
		return f.handle(msg)
	}
	before := f.values()
	res, cmd := f.handle(msg)
	if res == formContinue && !maps.Equal(before, f.values()) {
		f.err = ""
	}
	return res, cmd
}

func (f *form) handle(msg tea.KeyMsg) (formResult, tea.Cmd) {
	vis := f.sync()
	if len(vis) == 0 {
		return formCancel, nil
	}
	fl := vis[f.cur]
	if f.drop != nil && msg.String() == "ctrl+s" {
		f.commit()
		if f.validate() {
			return formSubmit, nil
		}
		return formContinue, f.focus()
	}
	if f.drop != nil {
		res, cmd := f.drop.update(msg)
		switch res {
		case formSubmit:
			if f.closeDrop(fl, true) {
				return formContinue, nil
			}
			return f.next()
		case formCancel:
			f.closeDrop(fl, false)
			return formContinue, f.focus()
		}
		return formContinue, cmd
	}
	switch msg.String() {
	case "esc":
		return formCancel, nil
	case "ctrl+s":
		if f.validate() {
			return formSubmit, nil
		}
		return formContinue, f.focus()
	case "up":
		return formContinue, f.move(-1)
	case "shift+tab":
		if f.tabSteps {
			return formPrev, nil
		}
		return formContinue, f.move(-1)
	case "down":
		return formContinue, f.move(1)
	case "tab":
		if f.tabSteps {
			return formNext, nil
		}
		return formContinue, f.move(1)
	case "pgup":
		return formContinue, f.move(-5)
	case "pgdown":
		return formContinue, f.move(5)
	case "enter":
		if fl.Kind == kindSelect || fl.Kind == kindMulti {
			return formContinue, f.openDrop(fl, "")
		}
		return f.next()
	}
	switch fl.Kind {
	case kindSelect:
		switch msg.String() {
		case "left", "right":
			if opts := f.options(fl); len(opts) > 0 {
				step := 1
				if msg.String() == "left" {
					step = len(opts) - 1
				}
				fl.Value = opts[(index(opts, fl.Value)+step)%len(opts)]
				fl.err = ""
			}
		case " ":
			return formContinue, f.openDrop(fl, "")
		default:
			if r := typedRunes(msg); r != "" {
				return formContinue, f.openDrop(fl, r) // typing opens the list filtered
			}
		}
		return formContinue, f.focus() // fields may have appeared or gone
	case kindToggle:
		switch msg.String() {
		case " ", "left", "right":
			fl.On = !fl.On
		}
		return formContinue, f.focus()
	case kindMulti:
		switch msg.String() {
		case " ", "right":
			return formContinue, f.openDrop(fl, "")
		case "ctrl+a", "alt+a":
			for _, o := range fl.Options {
				fl.Picked[o] = true
			}
			fl.err = ""
		case "ctrl+n", "alt+n":
			fl.Picked = map[string]bool{}
		default:
			if r := typedRunes(msg); r != "" {
				return formContinue, f.openDrop(fl, r)
			}
		}
		return formContinue, f.focus()
	default:
		if fl.Kind == kindSecret && msg.Paste { // a copied secret often ends in a line break
			msg.Runes = []rune(strings.TrimRight(string(msg.Runes), "\r\n"))
		}
		var cmd tea.Cmd
		fl.input, cmd = fl.input.Update(msg)
		fl.err = ""
		return formContinue, cmd
	}
}

// typedRunes is the text of a key that types something other than a space.
func typedRunes(msg tea.KeyMsg) string {
	if msg.Type != tea.KeyRunes || msg.Alt || strings.TrimSpace(string(msg.Runes)) == "" {
		return ""
	}
	return string(msg.Runes)
}

// next moves to the next field; on the last one it submits.
func (f *form) next() (formResult, tea.Cmd) {
	vis := f.sync()
	if f.cur >= len(vis)-1 {
		if f.validate() {
			return formSubmit, nil
		}
		return formContinue, f.focus()
	}
	return formContinue, f.move(1)
}

// openDrop shows the options of the focused field as a list, typed filtering it.
func (f *form) openDrop(fl *field, typed string) tea.Cmd {
	notes := fl.Notes
	if notes == nil {
		notes = optionNotes
	}
	items := slices.Clone(f.options(fl))
	var p *picker
	switch fl.Kind {
	case kindSelect:
		if !slices.Contains(items, fl.Value) {
			items = append(items, fl.Value)
		}
		items = append(items, fl.Actions...)
		p = newPicker("", items, nil)
		p.single = true
		p.free = fl.Free
		p.cur = index(items, fl.Value)
	case kindMulti:
		picked := f.picked(fl.Key)
		for _, v := range picked {
			if !slices.Contains(items, v) {
				items = append(items, v)
			}
		}
		p = newPicker("", items, picked)
		p.free = !fl.Fixed
	default:
		return nil
	}
	known := f.options(fl)
	p.note = map[string]string{}
	for _, it := range items {
		if n := notes[it]; n != "" {
			p.note[it] = n
		} else if fl.Kind == kindMulti && !slices.Contains(known, it) {
			p.note[it] = "not listed"
		}
	}
	if fl.Empty != "" {
		p.label[""] = fl.Empty
	}
	p.live, p.typing, p.bare = true, true, true
	if typed != "" {
		p.filter.SetValue(typed)
		p.filter.CursorEnd()
		p.cur = 0
	}
	f.drop = p
	if fl.Kind == kindText || fl.Kind == kindSecret {
		fl.input.Blur()
	}
	return p.filter.Focus()
}

// commit takes the pick of an open list, as enter would, without moving on.
func (f *form) commit() {
	if f.drop == nil {
		return
	}
	vis := f.sync()
	f.closeDrop(vis[f.cur], true)
	f.focus()
}

// closeDrop copies the list state back into the field. It reports whether an Actions
// row was picked instead of a value.
func (f *form) closeDrop(fl *field, apply bool) bool {
	d := f.drop
	f.drop = nil
	if !apply {
		return false
	}
	switch fl.Kind {
	case kindSelect:
		it, ok := d.current()
		if !ok {
			return false
		}
		if slices.Contains(fl.Actions, it) {
			f.action = fl.Key + ":" + it
			return true
		}
		fl.Value = it
	case kindMulti:
		fl.Picked = map[string]bool{}
		for _, o := range d.chosen() {
			fl.Picked[o] = true
		}
	}
	fl.err = ""
	f.sync()
	return false
}

func index(opts []string, v string) int {
	for i, o := range opts {
		if o == v {
			return i
		}
	}
	return 0
}

// validate sets per-field errors and moves the focus to the first bad field.
func (f *form) validate() bool {
	var first *field
	for _, fl := range f.visible() {
		fl.err = ""
		v := f.get(fl.Key)
		req := fl.Required || (fl.ReqFn != nil && fl.ReqFn(f))
		switch {
		case req && fl.Kind == kindMulti && len(f.picked(fl.Key)) == 0:
			fl.err = "required"
		case req && v == "" && fl.Kind != kindToggle && fl.Kind != kindMulti:
			fl.err = "required"
		case fl.Validate != nil && (v != "" || req):
			if err := fl.Validate(v); err != nil {
				fl.err = err.Error()
			}
		}
		if fl.err != "" && first == nil {
			first = fl
		}
	}
	if first != nil {
		f.focused = first
		f.focus()
	}
	return first == nil
}

// hints are the keys of the focused field for the footer. On the last field enter
// finishes the form, so it says what it does there.
func (f *form) hints() []string {
	last := f.onLast()
	if f.drop != nil {
		h := f.drop.hints()
		if last {
			for i := 0; i+1 < len(h); i += 2 {
				if h[i] == "enter" {
					h[i+1] = f.doneWord()
				}
			}
		}
		return h
	}
	if f.focused == nil {
		return nil
	}
	next := []string{"tab/↓", "next"}
	if f.tabSteps {
		next = []string{"↓", "next"}
	}
	if last {
		next = []string{"enter", f.doneWord()}
	}
	switch f.focused.Kind {
	case kindSelect:
		return append([]string{"space", "open", "←/→", "cycle"}, next...)
	case kindMulti:
		return append([]string{"space", "open", "ctrl+a/n alt+a/n", "all/none"}, next...)
	case kindToggle:
		return append([]string{"space", "on/off"}, next...)
	}
	if last {
		return next
	}
	return []string{"enter/tab", "next"}
}

// onLast reports whether the focused field is the last one shown.
func (f *form) onLast() bool {
	vis := f.visible()
	return len(vis) > 0 && f.focused == vis[len(vis)-1]
}

// doneWord is what enter on the last field does.
func (f *form) doneWord() string {
	if f.done != "" {
		return f.done
	}
	return "apply"
}

// helpText is the dim line under a focused field: the example while the field is empty
// or wrong, then the help.
func (fl *field) helpText(f *form) string {
	h := fl.Help
	if fl.HelpFn != nil {
		h = fl.HelpFn(f)
	}
	if fl.Example != "" && (fl.err != "" || fl.blank(f)) {
		h = strings.TrimSpace(fl.Example + "   " + h)
	}
	return h
}

// blank reports whether the field has no value.
func (fl *field) blank(f *form) bool {
	switch fl.Kind {
	case kindMulti:
		return len(f.picked(fl.Key)) == 0
	case kindToggle:
		return false
	case kindText, kindSecret:
		return strings.TrimSpace(fl.input.Value()) == ""
	}
	return fl.Value == ""
}

// view draws the form. With a height it scrolls so the focused field, its open list and
// its help stay on screen.
func (f *form) view(width int, height ...int) []string {
	vis := f.sync()
	labelW := 0
	for _, fl := range vis {
		labelW = max(labelW, lipgloss.Width(fl.Label))
	}
	var head []string
	if f.title != "" {
		head = append(head, " "+sTitle.Render(txt.Clean(f.title)), "")
	}
	if f.err != "" {
		head = append(head, " "+sErr.Render(ansi.Truncate(glyphErr+" "+txt.Clean(f.err), max(width-2, 10), "…")), "")
	}
	if f.note != "" {
		head = append(head, " "+sDim.Render(ansi.Truncate(txt.Clean(f.note), max(width-2, 10), "…")), "")
	}
	avail := 0
	if len(height) > 0 {
		avail = height[0] - len(head)
	}
	sub := func(s string) string { return strings.Repeat(" ", labelW+4) + s }
	valW := max(width-labelW-6, 10)
	var body []string
	starts := make([]int, len(vis))
	focusStart, focusEnd := 0, 0
	for i, fl := range vis {
		focused := i == f.cur
		starts[i] = len(body)
		if focused {
			focusStart = len(body)
		}
		label := pad(fl.Label, labelW, false)
		if focused {
			label = sKey.Render(label)
		} else {
			label = sDim.Render(label)
		}
		body = append(body, ansi.Truncate("  "+label+"  "+f.render(fl, focused, valW), width, "…"))
		if focused && f.drop != nil {
			dh := dropRows + 2
			if avail > 0 {
				dh = min(dh, max(avail-2, 4))
			}
			for _, l := range f.drop.view(max(width-labelW-4, 10), dh) {
				body = append(body, strings.Repeat(" ", labelW+2)+l)
			}
		} else {
			if fl.err != "" {
				body = append(body, sub(sErr.Render(ansi.Truncate("↳ "+txt.Clean(fl.err), valW, "…"))))
			}
			if help := fl.helpText(f); help != "" && (focused || fl.err != "") {
				body = append(body, sub(sDim.Render(ansi.Truncate(help, valW, "…"))))
			}
		}
		if focused {
			focusEnd = len(body)
		}
	}
	if avail <= 0 || len(body) <= avail {
		f.offset = 0
		return append(head, body...)
	}
	rows := max(avail-1, 1)
	focusEnd = min(focusEnd, focusStart+rows)
	if focusStart < f.offset {
		f.offset = focusStart
	}
	if focusEnd > f.offset+rows {
		f.offset = focusEnd - rows
	}
	f.offset = min(max(f.offset, 0), len(body)-rows)
	above, below := 0, 0
	for _, s := range starts {
		switch {
		case s < f.offset:
			above++
		case s >= f.offset+rows:
			below++
		}
	}
	var more []string
	if above > 0 {
		more = append(more, fmt.Sprintf("↑ %d more", above))
	}
	if below > 0 {
		more = append(more, fmt.Sprintf("↓ %d more", below))
	}
	out := append(head, body[f.offset:f.offset+rows]...)
	return append(out, "  "+sDim.Render(strings.Join(more, "   ")))
}

// render is the value part of a field's line.
func (f *form) render(fl *field, focused bool, width int) string {
	switch fl.Kind {
	case kindText, kindSecret:
		if focused && f.drop == nil {
			fl.input.Width = max(width-1, 10)
			return fl.input.View()
		}
		if fl.Kind == kindSecret {
			return sText.Render(strings.Repeat("•", min(len([]rune(fl.input.Value())), width)))
		}
		v := fl.input.Value()
		if fl.Masked {
			v = config.Redact(v)
		}
		return sText.Render(ansi.Truncate(txt.Clean(v), width, "…"))
	case kindSelect:
		return sSelected.Render("["+txt.Clean(fl.display(fl.Value))+"]") + sDim.Render(" ▾")
	case kindToggle:
		if fl.On != fl.Invert {
			return sOK.Render("● on")
		}
		return sDim.Render("○ off")
	case kindMulti:
		picked := f.picked(fl.Key)
		if len(picked) == 0 {
			return sDim.Render("[none]") + sDim.Render(" ▾")
		}
		return sSelected.Render("["+txt.Clean(strings.Join(picked, ", "))+"]") + sDim.Render(" ▾")
	}
	return ""
}
