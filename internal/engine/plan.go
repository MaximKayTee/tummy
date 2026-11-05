package engine

import (
	"context"
	"fmt"

	"tummy/internal/config"
	"tummy/internal/db"
)

// PlanRow is what one stream of a run would do, worked out without writing.
type PlanRow struct {
	Stream    string
	Object    string
	Mode      string
	Action    string // create, replace, truncate, upsert, append, skip
	Exists    bool
	Watermark any    // incremental: the target's max update key
	SQL       string // source query; API streams: path
	Disabled  bool
	Err       string
}

// Plan resolves every stream of r against its connections: target table, action for
// the mode, and the source query, with the watermark the run would start from. The
// target is only read; a missing sqlite target file is created empty, as a run would.
func (e *Engine) Plan(ctx context.Context, r *config.Replication) ([]PlanRow, error) {
	srcConn, err := e.Conns.Get(r.Source)
	if err != nil {
		return nil, err
	}
	tgtConn, err := e.Conns.Get(r.Target)
	if err != nil {
		return nil, err
	}
	if tgtConn.ReadOnly {
		return nil, fmt.Errorf("conn %s: read_only, not a target", r.Target)
	}
	tp := tgtConn.DB()
	tp.Create = true // a sqlite target the run would create: an empty file, as the run makes
	tgt, err := db.Open(ctx, tp)
	if err != nil {
		return nil, err
	}
	defer tgt.Close()
	var streams []stream
	var src *db.DB
	if srcConn.Type == "api" {
		ordered, err := orderByForEach(r.Resolved)
		if err != nil {
			return nil, err
		}
		for _, s := range ordered {
			streams = append(streams, plain(s))
		}
	} else {
		if src, err = db.Open(ctx, srcConn.DB()); err != nil {
			return nil, err
		}
		defer src.Close()
		if streams, err = e.expand(ctx, src, tgt, r); err != nil {
			return nil, err
		}
	}
	var out []PlanRow
	for _, s := range streams {
		target := targetTable(tgt, s)
		row := PlanRow{Stream: s.Name, Object: target.String(), Mode: s.Mode, Disabled: s.Disabled}
		if s.Disabled {
			row.Action = "skip"
			out = append(out, row)
			continue
		}
		exists, err := tgt.TableExists(ctx, target)
		if err != nil {
			row.Err = err.Error()
			out = append(out, row)
			continue
		}
		row.Exists = exists
		row.Action = planAction(s.Mode, exists)
		if src == nil {
			row.SQL = s.Path
			out = append(out, row)
			continue
		}
		wm, err := watermark(ctx, tgt, s, target, exists)
		if err != nil {
			row.Err = err.Error()
			out = append(out, row)
			continue
		}
		row.Watermark = wm
		if sql, _, err := RunQuery(ctx, src, s.ResolvedStream, wm); err != nil {
			row.Err = err.Error()
		} else {
			row.SQL = sql
		}
		out = append(out, row)
	}
	return out, nil
}

// planAction words what a mode does to a target that does or does not exist.
func planAction(mode string, exists bool) string {
	if !exists {
		return "create"
	}
	switch mode {
	case "full-refresh":
		return "replace"
	case "truncate":
		return "truncate"
	case "incremental":
		return "upsert"
	case "backfill", "snapshot":
		return "append"
	}
	return mode
}
