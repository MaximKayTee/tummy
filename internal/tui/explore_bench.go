package tui

import (
	"context"
	"fmt"
	"time"
	"tummy/internal/txt"

	tea "github.com/charmbracelet/bubbletea"
)

// benchMsg is the result of running the preview's query in full.
type benchMsg struct {
	seq  int
	rows int
	took time.Duration
	err  error
}

// bench runs the query as shown in full, rows read and dropped, and times it.
func (e *explorer) bench() tea.Cmd {
	switch {
	case !e.steppable() || e.compiled == "":
		return nil
	case e.fetching || e.benching:
		return nil
	}
	e.benching, e.benchText = true, ""
	e.benchSeq = nextSeq()
	seq, h, sql, args := e.benchSeq, e.h, e.compiled, e.args
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		start := time.Now()
		d, err := h.get(ctx)
		if err != nil {
			return benchMsg{seq: seq, err: err}
		}
		rd, err := d.Query(ctx, sql, args...)
		if err != nil {
			return benchMsg{seq: seq, err: err}
		}
		defer rd.Close()
		n := 0
		for {
			row, err := rd.Next()
			if err != nil {
				return benchMsg{seq: seq, err: err}
			}
			if row == nil {
				break
			}
			n++
		}
		return benchMsg{seq: seq, rows: n, took: time.Since(start)}
	}
}

func (e *explorer) applyBench(msg benchMsg) {
	if msg.seq != e.benchSeq {
		return
	}
	e.benching = false
	if msg.err != nil {
		e.setNote("full run: "+txt.Clean(txt.FirstLine(msg.err.Error())), true)
		return
	}
	e.benchText = fmt.Sprintf("full %s rows  %s", txt.Thousands(int64(msg.rows)), txt.Dur(msg.took))
}
