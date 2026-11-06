package tui

import (
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"tummy/internal/config"
)

// ---------- bookmarks: saved queries, nothing written to the connection ----------

// loadMarks reads the bookmarks on this connection.
func (e *explorer) loadMarks() error {
	b, err := config.LoadBookmarks(config.BookmarksPath(e.opts.Home))
	if err != nil {
		return err
	}
	e.marks = b.For(e.conn.Name)
	return nil
}

// canBookmark: B is offered.
func (e *explorer) canBookmark() bool { return e.bookmarkable() == "" }

// bookmarkable: why the preview cannot be bookmarked, or "".
func (e *explorer) bookmarkable() string {
	switch {
	case !e.steppable():
		return "not for api"
	case len(e.cols) == 0:
		return "no preview"
	case e.editSQL:
		return "run the sql first"
	case e.spec.Where != "" || len(e.spec.Select) > 0 || e.spec.Range != "":
		return "stream has where, select or range: bookmark its target data"
	}
	return ""
}

// saveBookmark is B: a known bookmark is rewritten, anything else asks for a name.
func (e *explorer) saveBookmark() tea.Cmd {
	if why := e.bookmarkable(); why != "" {
		e.setNote(why, true)
		return nil
	}
	if e.bookmark != "" {
		if err := e.writeBookmark(e.bookmark); err != nil {
			e.setNote(txt.Clean(txt.FirstLine(err.Error())), true)
		}
		return nil
	}
	return e.toBookmarkForm()
}

func (e *explorer) toBookmarkForm() tea.Cmd {
	if e.bookmarkable() != "" {
		return e.saveBookmark() // the note
	}
	def := e.bookmark
	if def == "" {
		def = e.defaultBookmarkName()
	}
	e.markForm = newForm("Bookmark",
		text("name", "Name", "letters digits _ -").def(def).required().check(func(v string) error {
			if !nameRe.MatchString(v) {
				return errors.New("letters, digits, _ -")
			}
			if v != e.bookmark && slices.ContainsFunc(e.marks, func(b *config.Bookmark) bool { return b.Name == v }) {
				return errors.New("exists")
			}
			return nil
		}),
	)
	e.markForm.done = "save"
	e.page = pageBookmark
	return e.markForm.focus()
}

// defaultBookmarkName is the table without its schema, or query.
func (e *explorer) defaultBookmarkName() string {
	if e.spec.SQL != "" || e.table == "" {
		return "query"
	}
	_, name := splitLast(e.table)
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name)
	return cmpOr(strings.Trim(name, "_"), "query")
}

// writeBookmark saves the preview as bookmark name: the table or sql, the steps with the
// ticks as a trailing keep, and the stream it was opened from. A bookmark opened and
// saved again keeps its stream. The preview then saves to that bookmark.
func (e *explorer) writeBookmark(name string) error {
	p, err := e.finalSteps(e.tickedCols())
	if err != nil {
		return err
	}
	bm := config.Bookmark{Name: name, Conn: e.conn.Name, SQL: e.spec.SQL, Steps: renderSteps(p), Rep: e.repName, Stream: e.streamName}
	if bm.SQL == "" {
		bm.Table = e.table
	}
	all, err := config.EditBookmarks(e.opts.Home, func(all *config.Bookmarks) error {
		if old := all.Bookmark[name]; old != nil && e.bookmark == name {
			bm.Rep, bm.Stream = old.Rep, old.Stream
		}
		return all.Set(bm)
	})
	if err != nil {
		return err
	}
	e.marks = all.For(e.conn.Name)
	e.bookmark = name
	e.baseSteps, e.baseSQL = renderSteps(e.pipeline), e.spec.SQL
	e.tickBase = strings.Join(e.tickedCols(), "\x00")
	e.setNote("saved bookmarks.toml › "+name, false)
	return nil
}

// openBookmark previews a bookmark: its table or sql with its steps as the pipeline.
func (e *explorer) openBookmark(b *config.Bookmark) tea.Cmd {
	e.loadBookmark(b)
	return e.fetch()
}

func (e *explorer) loadBookmark(b *config.Bookmark) {
	e.resetPreview()
	e.table, e.streamName = "", ""
	e.openSpec(b.Table, config.Stream{SQL: b.SQL, Steps: b.Steps})
	e.bookmark = b.Name
	e.page = pagePreview
}

// newBookmarkExplorer opens the explorer on a bookmark; esc leaves.
func newBookmarkExplorer(opts Options, b *config.Bookmark) (*explorer, error) {
	e, err := newExplorer(opts, b.Conn)
	if err != nil {
		return nil, err
	}
	e.loadBookmark(b)
	e.fromStream = true
	return e, nil
}

func (e *explorer) deleteBookmark(name string) error {
	all, err := config.EditBookmarks(e.opts.Home, func(all *config.Bookmarks) error { delete(all.Bookmark, name); return nil })
	if err != nil {
		return err
	}
	e.marks = all.For(e.conn.Name)
	e.cur = min(e.cur, max(len(e.visibleItems())-1, 0))
	return nil
}

// clipboardOut receives the OSC 52 sequence that sets the terminal's clipboard.
var clipboardOut io.Writer = os.Stdout

// copySQL puts the query as shown on the clipboard through the terminal (OSC 52).
func (e *explorer) copySQL() tea.Cmd {
	sql := e.shownSQL()
	if sql == "" {
		e.setNote("no sql", true)
		return nil
	}
	e.setNote("sql copied", false)
	return func() tea.Msg {
		io.WriteString(clipboardOut, ansi.SetSystemClipboard(sql))
		return nil
	}
}
