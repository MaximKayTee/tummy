package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"tummy/internal/txt"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"tummy/internal/config"
)

func (w *repWizard) toReview() tea.Cmd {
	w.reviewErr, w.reviewWarn, w.changes = "", "", 0
	text, diff, err := config.PreviewWrite(w.opts.Home, w.draft)
	if err != nil {
		w.reviewErr = err.Error()
		if path, perr := config.ReplicationPath(w.opts.Home, w.draft.Name); perr == nil {
			w.reviewErr = strings.TrimPrefix(w.reviewErr, path+": ")
		}
	}
	if len(w.draft.Streams) == 0 {
		w.reviewWarn = "no streams"
	}
	var lines []string
	if w.original == "" || len(diff) == 0 {
		lines = strings.Split(strings.TrimRight(txt.CleanText(text), "\n"), "\n")
	} else {
		lines, w.changes = styleDiff(diff)
	}
	w.review = viewport.New(80, 10)
	w.review.SetContent(strings.Join(lines, "\n"))
	w.step = stepReview
	return nil
}

// styleDiff colours the lines of config.PreviewWrite and counts the changed ones.
func styleDiff(diff []string) ([]string, int) {
	out := make([]string, len(diff))
	changes := 0
	for i, l := range diff {
		l = txt.Clean(l)
		switch {
		case strings.HasPrefix(l, "+ "):
			out[i] = sOK.Render(l)
			changes++
		case strings.HasPrefix(l, "- "):
			out[i] = sErr.Render(l)
			changes++
		default:
			out[i] = sDim.Render(l)
		}
	}
	return out, changes
}

// save writes d. A rename copies the old file first so its comments stay, then removes it.
func (w *repWizard) save(d config.ReplicationDraft) error {
	renamed := w.original != "" && w.original != d.Name
	if renamed && d.From == "" {
		d.From = w.original // config patches the old file, comments and all
	}
	if _, err := config.WriteReplication(w.opts.Home, d); err != nil {
		return err
	}
	w.draft = d
	w.savedName = d.Name
	w.done = statusMsg{Text: "saved: " + d.Name + ".toml"}
	if renamed {
		if err := config.DeleteReplication(w.opts.Home, w.original); err != nil {
			w.done = statusMsg{Text: fmt.Sprintf("saved: %s.toml; %s.toml not removed: %v", d.Name, w.original, err), IsErr: true}
		}
	}
	return nil
}

func (w *repWizard) reviewView(width, height int) []string {
	title := "replications/" + w.draft.Name + ".toml"
	if w.original != "" && w.original != w.draft.Name {
		title += sDim.Render("  was " + w.original + ".toml")
	}
	out := []string{" " + sTitle.Render(txt.Clean(title)), ""}
	switch {
	case w.reviewErr != "":
		out = append(out, " "+sErr.Render(glyphErr+" "+txt.Clean(txt.FirstLine(w.reviewErr))), "")
	case w.reviewWarn != "":
		out = append(out, " "+sWarn.Render(w.reviewWarn), "")
	case w.original != "" && w.changes == 0:
		out = append(out, " "+sDim.Render("no changes"), "")
	}
	w.review.Width = max(width-2, 10)
	w.review.Height = max(height-len(out), 3)
	for _, l := range strings.Split(w.review.View(), "\n") {
		out = append(out, " "+l)
	}
	return out
}

// overrides summarises which keys differ from the defaults.
func overrides(s, d config.Stream) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if !slices.Equal(s.PrimaryKey, d.PrimaryKey) {
		add("pk", strings.Join(s.PrimaryKey, ","))
	}
	if s.UpdateKey != d.UpdateKey {
		add("update_key", s.UpdateKey)
	}
	if s.SQL != "" {
		parts = append(parts, "sql")
	}
	if s.Path != "" {
		add("path", s.Path)
	}
	if s.ForEach != "" {
		add("for_each", s.ForEach)
	}
	if s.Paginate != d.Paginate {
		add("paginate", s.Paginate)
	}
	if s.Where != "" {
		parts = append(parts, "where")
	}
	if s.Steps != "" {
		parts = append(parts, "steps")
	}
	if len(s.Select) > 0 {
		parts = append(parts, plural(len(s.Select), "col"))
	}
	if s.Limit > 0 {
		add("limit", strconv.Itoa(s.Limit))
	}
	return strings.Join(parts, "  ")
}
