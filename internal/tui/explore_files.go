package tui

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	txt "github.com/MaximKayTee/gotuitools/text"

	"tummy/internal/config"
	"tummy/internal/engine"
)

// filesFetch reads the first n rows of the files a stream path matches.
func filesFetch(c *config.Conn, spec config.Stream, n int) previewMsg {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	p, err := engine.PreviewFiles(ctx, c, spec, n)
	msg := previewMsg{Cols: p.Cols, Rows: p.Rows, Total: -1, Complete: p.Complete, Err: err, Took: time.Since(start)}
	if p.Complete {
		msg.Total = len(p.Rows)
	}
	if err == nil {
		msg.Parents = filesNote(p)
		if p.Files == 0 {
			msg.Err = fmt.Errorf("no .csv or .xlsx file matches %s", spec.Path)
			if len(p.Skipped) > 0 {
				msg.Err = fmt.Errorf("%w; not read: %s", msg.Err, strings.Join(p.Skipped, ", "))
			}
		}
	}
	return msg
}

// filesNote is "2 of 5 files" or "1 file", and the files left out.
func filesNote(p engine.FilePreview) string {
	note := txt.Plural(int64(p.Files), "file")
	if p.Read < p.Files {
		note = fmt.Sprintf("%d of %s", p.Read, note)
	}
	if n := len(p.Skipped); n > 0 {
		note += fmt.Sprintf("  %d not .csv or .xlsx", n)
	}
	return note
}

// fileStreamName is a stream name from a file pattern: its last part without the
// extension, letters, digits and _ only.
func fileStreamName(pattern string) string {
	base := path.Base(pattern)
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == '-' || r == ' ':
			b.WriteByte('_')
		}
	}
	return cmpOr(strings.Trim(b.String(), "_"), "files")
}
