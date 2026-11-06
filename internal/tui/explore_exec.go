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

// ---------- statements run off the preview: full run, execute, drop table ----------

// execMsg is the result of a statement: what ran, how many rows, how long.
type execMsg struct {
	seq  int
	kind string // bench, exec, or drop:TABLE
	n    int64
	took time.Duration
	err  error
}

// timed runs f on the connection off the loop and reports its count and duration.
func timed(h *dbHandle, seq int, kind string, f func(context.Context, *db.DB) (int64, error)) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		start := time.Now()
		d, err := h.get(ctx)
		if err != nil {
			return execMsg{seq: seq, kind: kind, err: err}
		}
		n, err := f(ctx, d)
		return execMsg{seq: seq, kind: kind, n: n, took: time.Since(start), err: err}
	}
}

// begin marks a statement as running; the head shows what.
func (e *explorer) begin(what string) int {
	e.busy, e.busySeq = what, nextSeq()
	return e.busySeq
}

// writable: statements may run on this connection.
func (e *explorer) writable() bool { return !e.isAPI && !e.conn.ReadOnly }

// bench runs the query as compiled in full, rows read and dropped, and times it.
func (e *explorer) bench() tea.Cmd {
	if !e.steppable() || e.compiled == "" || e.fetching || e.busy != "" {
		return nil
	}
	e.benchText = ""
	sql, args := e.compiled, e.args
	return timed(e.h, e.begin("full run"), "bench", func(ctx context.Context, d *db.DB) (int64, error) {
		rd, err := d.Query(ctx, sql, args...)
		if err != nil {
			return 0, err
		}
		defer rd.Close()
		var n int64
		for {
			row, err := rd.Next()
			if err != nil || row == nil {
				return n, err
			}
			n++
		}
	})
}

// execSQL runs the editor's text as written, no wrapping, no limit.
func (e *explorer) execSQL() tea.Cmd {
	sql := strings.TrimSpace(e.sqlInput.Value())
	switch {
	case sql == "" || e.busy != "":
		return nil
	case !e.writable():
		e.setNote(e.conn.Name+": read only", true)
		return nil
	}
	e.fetchErr = ""
	return timed(e.h, e.begin("executing"), "exec", execute(sql))
}

func execute(sql string) func(context.Context, *db.DB) (int64, error) {
	return func(ctx context.Context, d *db.DB) (int64, error) {
		res, err := d.ExecContext(ctx, sql)
		if err != nil {
			return 0, fmt.Errorf("%w\n%s", err, sql)
		}
		n, _ := res.RowsAffected()
		return n, nil
	}
}

func (e *explorer) applyExec(msg execMsg) tea.Cmd {
	if msg.seq != e.busySeq {
		return nil
	}
	e.busy, e.statsBusy = "", ""
	drop := strings.TrimPrefix(msg.kind, "drop:")
	switch {
	case msg.err != nil && msg.kind == "exec":
		e.fetchErr = msg.err.Error()
	case msg.err != nil:
		e.setNote(msg.kind+": "+txt.Clean(txt.FirstLine(msg.err.Error())), true)
		if drop != msg.kind {
			e.statsNote, e.statsErr = txt.FirstLine(msg.err.Error()), true
		}
	case msg.kind == "bench":
		e.benchText = fmt.Sprintf("full %s rows  %s", txt.Thousands(msg.n), txt.Dur(msg.took))
	case msg.kind == "exec":
		e.setNote(fmt.Sprintf("%s rows affected  %s", txt.Thousands(msg.n), txt.Dur(msg.took)), false)
	default: // dropped
		delete(e.stats, drop)
		e.statsNote, e.statsErr = "dropped "+txt.Clean(drop), false
		if e.page == pagePreview && e.table == drop {
			e.resetPreview()
			e.table = ""
			if e.fromStream {
				e.fetchErr = "dropped " + drop
				return nil
			}
			e.page = pageTables
		}
		return e.discover()
	}
	return nil
}

// askDrop opens the confirmation for dropping table: its name must be typed.
func (e *explorer) askDrop(table string) tea.Cmd {
	switch {
	case table == "" || e.busy != "":
		return nil
	case !e.writable():
		e.setNote(e.conn.Name+": read only", true)
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
	if e.page == pageTables {
		e.statsBusy = "dropping " + e.dropTable
	}
	return timed(e.h, e.begin("dropping "+e.dropTable), "drop:"+e.dropTable, execute(db.DropTableSQL(e.dl, db.ParseTable(e.dropTable))))
}
