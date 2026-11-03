package tui

import "github.com/charmbracelet/bubbles/key"

// One grammar in every view: E edit, e on/off, d delete (y/n), ctrl+s save, esc back,
// enter open or next. Backspace only goes back.
type keyMap struct {
	Up, Down, Top, Bottom key.Binding
	PageUp, PageDown      key.Binding
	Enter, Back           key.Binding
	Run, RunStream        key.Binding
	Toggle                key.Binding
	History, Conns        key.Binding
	Test                  key.Binding
	Filter, Refresh       key.Binding
	New, Edit, Delete     key.Binding
	Add, Preview          key.Binding
	Cancel, Create        key.Binding
	Help, Quit            key.Binding
}

var keys = keyMap{
	Up:        key.NewBinding(key.WithKeys("up", "k")),
	Down:      key.NewBinding(key.WithKeys("down", "j")),
	Top:       key.NewBinding(key.WithKeys("g", "home")),
	Bottom:    key.NewBinding(key.WithKeys("G", "end")),
	PageUp:    key.NewBinding(key.WithKeys("pgup")),
	PageDown:  key.NewBinding(key.WithKeys("pgdown")),
	Enter:     key.NewBinding(key.WithKeys("enter", "l", "right")),
	Back:      key.NewBinding(key.WithKeys("esc", "backspace", "h", "left")),
	Run:       key.NewBinding(key.WithKeys("r")),
	RunStream: key.NewBinding(key.WithKeys("s")),
	Toggle:    key.NewBinding(key.WithKeys("e")),
	History:   key.NewBinding(key.WithKeys("H")),
	Conns:     key.NewBinding(key.WithKeys("c")),
	Test:      key.NewBinding(key.WithKeys("t")),
	Filter:    key.NewBinding(key.WithKeys("/")),
	Refresh:   key.NewBinding(key.WithKeys("R")),
	New:       key.NewBinding(key.WithKeys("n")),
	Edit:      key.NewBinding(key.WithKeys("E")),
	Delete:    key.NewBinding(key.WithKeys("d")),
	Add:       key.NewBinding(key.WithKeys("a")),
	Preview:   key.NewBinding(key.WithKeys("p")),
	Cancel:    key.NewBinding(key.WithKeys("x")),
	Create:    key.NewBinding(key.WithKeys("c")),
	Help:      key.NewBinding(key.WithKeys("?", "f1")),
	Quit:      key.NewBinding(key.WithKeys("q", "ctrl+c")),
}
