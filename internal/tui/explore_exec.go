package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/db"
)

// ---------- one-off statements and dropping a table ----------

// execMsg is the result of a statement run as written.
type execMsg struct {
	seq      int
	affected int64
	took     time.Duration
	err      error
	drop     string // the table dropped, "" for a typed statement
}

// writable: statements may run on this connection.
func (e *explorer) writable() bool { return !e.isAPI && !e.conn.ReadOnly }

// execSQL runs the editor's text as written, no wrapping, no limit.
func (e *explorer) execSQL() tea.Cmd {
	sql := strings.TrimSpace(e.sqlInput.Value())
	switch {
	case sql == "":
		return nil
	case !e.writable():
		e.setNote(e.conn.Name+": read only", true)
		return nil
	case e.execBusy:
		return nil
	}
	e.execBusy, e.fetchErr = true, ""
	e.execSeq = nextSeq()
	return e.runStatement(e.execSeq, sql, "")
}

func (e *explorer) runStatement(seq int, sql, drop string) tea.Cmd {
	h := e.h
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		start := time.Now()
		d, err := h.get(ctx)
		if err != nil {
			return execMsg{seq: seq, err: err, drop: drop}
		}
		res, err := d.ExecContext(ctx, sql)
		if err != nil {
			return execMsg{seq: seq, err: fmt.Errorf("%w\n%s", err, sql), drop: drop}
		}
		n, _ := res.RowsAffected()
		return execMsg{seq: seq, affected: n, took: time.Since(start), drop: drop}
	}
}

func (e *explorer) applyExec(msg execMsg) tea.Cmd {
	if msg.seq != e.execSeq {
		return nil
	}
	e.execBusy, e.statsBusy = false, ""
	switch {
	case msg.err != nil && msg.drop != "":
		e.statsNote, e.statsErr = txt.FirstLine(msg.err.Error()), true
		e.setNote("drop "+txt.Clean(msg.drop)+": "+txt.Clean(txt.FirstLine(msg.err.Error())), true)
		return nil
	case msg.err != nil:
		e.fetchErr = msg.err.Error()
		return nil
	case msg.drop != "":
		delete(e.stats, msg.drop)
		e.statsNote, e.statsErr = "dropped "+txt.Clean(msg.drop), false
		if e.page == pagePreview && e.table == msg.drop {
			if e.fromStream {
				e.done = statusMsg{Text: "dropped " + msg.drop}
				e.resetPreview()
				e.table = ""
				e.fetchErr = "dropped " + msg.drop
				return nil
			}
			e.resetPreview()
			e.table = ""
			e.page = pageTables
		}
		return e.discover()
	}
	e.setNote(fmt.Sprintf("%s rows affected  %s", txt.Thousands(msg.affected), txt.Dur(msg.took)), false)
	return nil
}

// askDrop opens the confirmation for dropping table: its name must be typed.
func (e *explorer) askDrop(table string) tea.Cmd {
	switch {
	case table == "" || !e.writable():
		if table != "" {
			e.setNote(e.conn.Name+": read only", true)
		}
		return nil
	case e.execBusy:
		return nil
	}
	e.dropTable = table
	e.dropForm = newForm("Drop table "+table,
		text("name", "Table", "").required().check(func(v string) error {
			if v != table {
				return errors.New("type " + table)
			}
			return nil
		}),
	)
	e.dropForm.done = "drop"
	e.dropFrom = e.page
	e.page = pageDrop
	return e.dropForm.focus()
}

// drop runs DROP TABLE for the confirmed table.
func (e *explorer) drop() tea.Cmd {
	e.page = e.dropFrom
	e.execBusy = true
	e.execSeq = nextSeq()
	if e.page == pageTables {
		e.statsBusy = "dropping " + e.dropTable
	}
	return e.runStatement(e.execSeq, db.DropTableSQL(e.dl, db.ParseTable(e.dropTable)), e.dropTable)
}
