package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/db"
	"tummy/internal/state"
	"tummy/internal/txt"
)

// Row counts and sizes of a connection's tables: read from the catalog on s, counted
// exactly on c, kept as history in the state database.

type statsSort int

const (
	sortName statsSort = iota
	sortRows
	sortSize
)

func (s statsSort) String() string { return [...]string{"name", "rows", "size"}[s] }

// statsMsg carries the stored readings after a load, refresh or count.
type statsMsg struct {
	conn  string
	seq   int
	stats map[string]state.TableStat
	note  string
	err   error
}

func statsByTable(list []state.TableStat) map[string]state.TableStat {
	out := make(map[string]state.TableStat, len(list))
	for _, s := range list {
		out[db.Table{Schema: s.Schema, Name: s.Table}.String()] = s
	}
	return out
}

func (e *explorer) statsCmd(work func(ctx context.Context) (string, error)) tea.Cmd {
	e.statsSeq = nextSeq()
	st, name, seq := e.opts.State, e.conn.Name, e.statsSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var note string
		if work != nil {
			var err error
			if note, err = work(ctx); err != nil {
				return statsMsg{conn: name, seq: seq, err: err}
			}
		}
		list, err := st.TableStats(ctx, name)
		return statsMsg{conn: name, seq: seq, stats: statsByTable(list), note: note, err: err}
	}
}

// loadStats reads the stored readings; the database is not touched.
func (e *explorer) loadStats() tea.Cmd {
	if e.opts.State == nil {
		return nil
	}
	return e.statsCmd(nil)
}

// refreshStats reads every table's rows and size from the catalog and stores them.
func (e *explorer) refreshStats() tea.Cmd {
	e.statsBusy = "reading row counts and sizes"
	h, st, name := e.h, e.opts.State, e.conn.Name
	return e.statsCmd(func(ctx context.Context) (string, error) {
		d, err := h.get(ctx)
		if err != nil {
			return "", err
		}
		list, err := d.TableStats(ctx)
		if err != nil {
			return "", err
		}
		rows := make([]state.TableStat, len(list))
		for i, s := range list {
			rows[i] = state.TableStat{Schema: s.Table.Schema, Table: s.Table.Name, Rows: s.Rows, Bytes: s.Bytes, Exact: s.Exact}
		}
		if err := st.SaveTableStats(ctx, name, time.Now(), true, rows); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s refreshed", plural(len(list), "table")), nil
	})
}

// countTable runs COUNT(*) on one table and stores it with the last known size.
func (e *explorer) countTable(name string) tea.Cmd {
	e.statsBusy = "counting " + name
	h, st, conn := e.h, e.opts.State, e.conn.Name
	bytes := int64(-1)
	if s, ok := e.stats[name]; ok {
		bytes = s.Bytes
	}
	t := db.ParseTable(name)
	return e.statsCmd(func(ctx context.Context) (string, error) {
		d, err := h.get(ctx)
		if err != nil {
			return "", err
		}
		start := time.Now()
		n, err := d.CountTable(ctx, t)
		if err != nil {
			return "", err
		}
		row := state.TableStat{Schema: t.Schema, Table: t.Name, Rows: n, Bytes: bytes, Exact: true}
		if err := st.SaveTableStats(ctx, conn, time.Now(), false, []state.TableStat{row}); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s: %s rows, counted in %s", name, txt.Thousands(n), txt.Dur(time.Since(start))), nil
	})
}

func (e *explorer) applyStats(msg statsMsg) {
	if msg.seq != e.statsSeq || msg.conn != e.conn.Name {
		return
	}
	e.statsBusy = ""
	e.statsNote, e.statsErr = msg.note, msg.err != nil
	if msg.err != nil {
		e.statsNote = txt.FirstLine(msg.err.Error())
		return
	}
	e.stats = msg.stats
}

// sortTables orders names by rows or size, largest first, unknown last.
func (e *explorer) sortTables(names []string) []string {
	if e.sortBy == sortName || len(e.stats) == 0 {
		return names
	}
	out := slices.Clone(names)
	val := func(n string) int64 {
		s, ok := e.stats[n]
		switch {
		case !ok:
			return -1
		case e.sortBy == sortRows:
			return s.Rows
		}
		return s.Bytes
	}
	slices.SortStableFunc(out, func(a, b string) int {
		va, vb := val(a), val(b)
		switch {
		case va > vb:
			return -1
		case va < vb:
			return 1
		}
		return strings.Compare(a, b)
	})
	return out
}

// statsColumns is the stats part of a table-list line: rows, size, change, age.
const statsWidth = 14 + 2 + 10 + 2 + 12 + 2 + 11

func statsHeader() string {
	return pad("ROWS", 14, true) + "  " + pad("SIZE", 10, true) + "  " + pad("CHANGE", 12, true) + "  " + pad("READ", 11, false)
}

func statsColumns(s state.TableStat, ok bool) string {
	if !ok {
		return strings.Repeat(" ", statsWidth)
	}
	rows := "—"
	if s.Rows >= 0 {
		rows = txt.Thousands(s.Rows)
		if !s.Exact {
			rows = "~" + rows
		}
	}
	change := ""
	if s.Rows >= 0 && s.PrevRows >= 0 && s.Rows != s.PrevRows {
		d := s.Rows - s.PrevRows
		change = "+" + txt.Thousands(d)
		if d < 0 {
			change = "−" + txt.Thousands(-d)
		}
	}
	return pad(rows, 14, true) + "  " + pad(sizeText(s.Bytes), 10, true) + "  " + pad(change, 12, true) + "  " + pad(ago(s.Refreshed), 11, false)
}

// sizeText formats bytes in binary units; "—" when unknown.
func sizeText(n int64) string {
	if n < 0 {
		return "—"
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB"} {
		f /= 1024
		if f < 1024 || u == "TiB" {
			if f < 10 {
				return fmt.Sprintf("%.1f %s", f, u)
			}
			return fmt.Sprintf("%.0f %s", f, u)
		}
	}
	return ""
}
