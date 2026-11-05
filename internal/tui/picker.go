package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"tummy/internal/txt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// picker is a list with a filter. Standalone it is a multi-select with / to filter and
// a/n for all/none. live makes it a dropdown: typing filters, ↑/↓ and pgup/pgdn move,
// space ticks, ctrl+a/ctrl+n tick all/none, enter takes the pick, esc cancels.
type picker struct {
	title    string
	items    []string
	note     map[string]string // dim text after an item
	label    map[string]string // shown in place of an item, as "" shown as none
	selected map[string]bool
	locked   map[string]bool // ticked and fixed
	order    []string        // items in the order they were ticked
	opened   map[string]bool // ticks when the list opened
	added    map[string]bool // typed values turned into items
	cur      int
	offset   int
	filter   textinput.Model
	typing   bool
	single   bool // one pick: typing filters, ↑/↓ move while typing, enter picks
	live     bool // dropdown keys, see above
	free     bool // live: text that matches no item is offered as a row of its own
	bare     bool // no title: drawn inside a form
}

func newPicker(title string, items []string, preselected []string) *picker {
	ti := newInput()
	ti.Prompt = "/"
	ti.Placeholder = "filter"
	ti.Width = 40
	p := &picker{title: title, items: items, selected: map[string]bool{}, note: map[string]string{},
		label: map[string]string{}, locked: map[string]bool{}, added: map[string]bool{}, filter: ti}
	for _, s := range preselected {
		if !p.selected[s] {
			p.selected[s] = true
			p.order = append(p.order, s)
		}
	}
	p.opened = maps.Clone(p.selected)
	return p
}

// toggle flips one item and keeps the pick order. A typed value becomes an item.
func (p *picker) toggle(it string) {
	if p.locked[it] {
		return
	}
	if !slices.Contains(p.items, it) {
		p.items = append(p.items, it)
		p.added[it] = true
	}
	if p.selected[it] {
		p.selected[it] = false
		if i := slices.Index(p.order, it); i >= 0 {
			p.order = slices.Delete(p.order, i, i+1)
		}
		return
	}
	p.selected[it] = true
	p.order = append(p.order, it)
}

// chosenInOrder returns ticked items in the order they were ticked.
func (p *picker) chosenInOrder() []string {
	var out []string
	for _, o := range p.order {
		if p.selected[o] {
			out = append(out, o)
		}
	}
	return out
}

// changed reports whether the ticks differ from when the list opened.
func (p *picker) changed() bool {
	for k, v := range p.selected {
		if v != p.opened[k] {
			return true
		}
	}
	for k, v := range p.opened {
		if v && !p.selected[k] {
			return true
		}
	}
	return false
}

func (p *picker) display(it string) string {
	if l, ok := p.label[it]; ok {
		return l
	}
	return it
}

// typed is the filter text when it is offered as a value of its own.
func (p *picker) typed() string {
	raw := strings.TrimSpace(p.filter.Value())
	if !p.live || !p.free || raw == "" {
		return ""
	}
	for _, it := range p.items {
		if it == raw || p.display(it) == raw {
			return ""
		}
	}
	return raw
}

// visible are the items matching the filter: names starting with it first, then names
// with a word starting with it, then the rest that contain it.
func (p *picker) visible() []string {
	f := strings.ToLower(strings.TrimSpace(p.filter.Value()))
	if f == "" {
		return p.items
	}
	var first, word, rest []string
	for _, it := range p.items {
		d := strings.ToLower(p.display(it))
		i := strings.Index(d, f)
		switch {
		case i < 0:
		case i == 0:
			first = append(first, it)
		case wordStart(d, f):
			word = append(word, it)
		default:
			rest = append(rest, it)
		}
	}
	out := append(append(first, word...), rest...)
	if t := p.typed(); t != "" {
		out = append(out, t)
	}
	return out
}

// wordStart reports whether f starts a word of d after . _ - / or a space.
func wordStart(d, f string) bool {
	for i := 1; i < len(d); i++ {
		if strings.ContainsRune("._-/ ", rune(d[i-1])) && strings.HasPrefix(d[i:], f) {
			return true
		}
	}
	return false
}

// chosen returns ticked items in list order, then ticked values typed in.
func (p *picker) chosen() []string {
	var out []string
	for _, it := range p.items {
		if p.selected[it] {
			out = append(out, it)
		}
	}
	for _, o := range p.order {
		if p.selected[o] && !slices.Contains(p.items, o) {
			out = append(out, o)
		}
	}
	return out
}

// current is the item under the cursor.
func (p *picker) current() (string, bool) {
	return at(p.visible(), p.cur)
}

func (p *picker) update(msg tea.KeyMsg) (formResult, tea.Cmd) {
	if p.live {
		return p.updateLive(msg)
	}
	if p.typing {
		switch msg.String() {
		case "esc":
			if p.single {
				return formCancel, nil
			}
			p.typing = false
			p.filter.Blur()
			p.cur = 0
			return formContinue, nil
		case "enter":
			if p.single {
				return formSubmit, nil
			}
			p.typing = false
			p.filter.Blur()
			p.cur = 0
			return formContinue, nil
		case "up", "ctrl+k":
			if p.single {
				p.cur = max(p.cur-1, 0)
				return formContinue, nil
			}
		case "down", "ctrl+j":
			if p.single {
				p.cur = min(p.cur+1, max(len(p.visible())-1, 0))
				return formContinue, nil
			}
		case "pgup":
			p.cur = max(p.cur-dropRows, 0)
			return formContinue, nil
		case "pgdown":
			p.cur = min(p.cur+dropRows, max(len(p.visible())-1, 0))
			return formContinue, nil
		}
		var cmd tea.Cmd
		p.filter, cmd = p.filter.Update(msg)
		p.cur = 0
		return formContinue, cmd
	}
	vis := p.visible()
	switch msg.String() {
	case "esc":
		return formCancel, nil
	case "enter", "ctrl+s":
		if len(p.chosen()) == 0 && p.cur < len(vis) {
			p.toggle(vis[p.cur])
		}
		return formSubmit, nil
	case "/":
		p.typing = true
		return formContinue, p.filter.Focus()
	case "up", "k":
		p.cur = max(p.cur-1, 0)
	case "down", "j":
		p.cur = min(p.cur+1, max(len(vis)-1, 0))
	case "g":
		p.cur = 0
	case "G":
		p.cur = max(len(vis)-1, 0)
	case "pgup":
		p.cur = max(p.cur-dropRows, 0)
	case "pgdown":
		p.cur = min(p.cur+dropRows, max(len(vis)-1, 0))
	case " ", "x", "right", "l":
		if p.cur < len(vis) {
			p.toggle(vis[p.cur])
		}
	case "a", "ctrl+a":
		for _, it := range vis {
			if !p.selected[it] {
				p.toggle(it)
			}
		}
	case "n", "ctrl+n":
		for _, it := range vis {
			if p.selected[it] {
				p.toggle(it)
			}
		}
	}
	return formContinue, nil
}

// updateLive is the dropdown: the filter always has the keys it does not use.
func (p *picker) updateLive(msg tea.KeyMsg) (formResult, tea.Cmd) {
	vis := p.visible()
	last := max(len(vis)-1, 0)
	switch msg.String() {
	case "esc":
		return formCancel, nil
	case "enter":
		if !p.single && !p.changed() && strings.TrimSpace(p.filter.Value()) != "" {
			// typed a search and ticked nothing: enter takes the row under the cursor
			if it, ok := p.current(); ok && !p.selected[it] {
				p.toggle(it)
			}
		}
		return formSubmit, nil
	case "up", "ctrl+k", "shift+tab":
		p.cur = max(p.cur-1, 0)
		return formContinue, nil
	case "down", "ctrl+j", "tab":
		p.cur = min(p.cur+1, last)
		return formContinue, nil
	case "pgup":
		p.cur = max(p.cur-dropRows, 0)
		return formContinue, nil
	case "pgdown":
		p.cur = min(p.cur+dropRows, last)
		return formContinue, nil
	case "ctrl+home", "home":
		p.cur = 0
		return formContinue, nil
	case "ctrl+end", "end":
		p.cur = last
		return formContinue, nil
	case " ":
		if !p.single {
			if it, ok := p.current(); ok {
				p.toggle(it)
			}
			return formContinue, nil
		}
	case "ctrl+a":
		if !p.single {
			for _, it := range vis {
				if !p.selected[it] && it != p.typed() {
					p.toggle(it)
				}
			}
		}
		return formContinue, nil
	case "ctrl+n", "alt+n":
		if !p.single {
			for _, it := range vis {
				if p.selected[it] {
					p.toggle(it)
				}
			}
		}
		return formContinue, nil
	}
	before := p.filter.Value()
	var cmd tea.Cmd
	p.filter, cmd = p.filter.Update(msg)
	if p.filter.Value() != before {
		p.cur = 0
	}
	return formContinue, cmd
}

// hints are the keys of the list for the footer.
func (p *picker) hints() []string {
	switch {
	case p.live && p.single:
		return []string{"type", "filter", "enter", "pick"}
	case p.live:
		return []string{"type", "filter", "space", "tick", "ctrl+a/n alt+a/n", "all/none", "ctrl+e", "preview", "enter", "done"}
	case p.typing && p.single:
		return []string{"type", "filter", "enter", "pick"}
	case p.typing:
		return []string{"enter", "apply filter"}
	}
	return []string{"/", "filter", "space", "tick", "ctrl+a/n alt+a/n", "all/none", "enter", "done"}
}

func (p *picker) view(width, height int) []string {
	var out []string
	if !p.bare {
		head := " " + sTitle.Render(txt.Clean(p.title))
		if !p.single {
			head += sDim.Render(fmt.Sprintf("  %d selected", len(p.chosen())))
		}
		out = append(out, head, "")
	}
	if p.typing || p.filter.Value() != "" {
		out = append(out, " "+p.filter.View())
		if !p.bare {
			out = append(out, "")
		}
	}
	vis := p.visible()
	if len(vis) == 0 {
		return append(out, sDim.Render("  no match"))
	}
	rows := max(height-len(out), 1)
	footer := len(vis) > rows
	if footer {
		rows = max(rows-1, 1)
	}
	p.cur = min(p.cur, len(vis)-1)
	if p.cur < p.offset {
		p.offset = p.cur
	}
	if p.cur >= p.offset+rows {
		p.offset = p.cur - rows + 1
	}
	p.offset = max(min(p.offset, len(vis)-rows), 0)
	end := min(p.offset+rows, len(vis))
	typed := p.typed()
	notes := map[string]string{}
	nameW := 0
	for i := p.offset; i < end; i++ {
		it := vis[i]
		n := p.note[it]
		if (typed != "" && it == typed) || p.added[it] {
			n = "new"
		}
		notes[it] = txt.Clean(n)
		if n != "" {
			nameW = max(nameW, lipgloss.Width(txt.Clean(p.display(it))))
		}
	}
	for i := p.offset; i < end; i++ {
		it := vis[i]
		name := txt.Clean(p.display(it))
		line := "  " + name
		if !p.single {
			box := sDim.Render("○")
			if p.selected[it] {
				box = sOK.Render("●")
			}
			line = "  " + box + " " + name
		}
		if n := notes[it]; n != "" {
			line += strings.Repeat(" ", max(nameW-lipgloss.Width(name), 0)) + sDim.Render("  "+n)
		}
		line = pad(line, width, false)
		if i == p.cur {
			line = selectedLine(line, width)
		}
		out = append(out, line)
	}
	if footer {
		out = append(out, sDim.Render(fmt.Sprintf("  %d of %d", p.cur+1, len(vis))))
	}
	return out
}
