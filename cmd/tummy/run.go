package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"tummy/internal/config"
	"tummy/internal/daemon"
	"tummy/internal/engine"
	"tummy/internal/state"
)

func runCmd() *cobra.Command {
	var (
		streams    []string
		src, tgt   string
		stream     string
		object     string
		mode       string
		primaryKey []string
		updateKey  string
		where      string
		limit      int
	)
	adhocFlags := []string{"src", "tgt", "stream", "object", "mode", "primary-key", "update-key", "where", "limit"}
	cmd := &cobra.Command{
		Use:   "run NAME|FILE | run --src CONN --stream TABLE --tgt CONN",
		Short: "Run a replication, or one ad-hoc stream",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			adhocSet := false
			for _, f := range adhocFlags {
				adhocSet = adhocSet || cmd.Flags().Changed(f)
			}
			var r *config.Replication
			var err error
			switch {
			case len(args) == 1 && adhocSet:
				return errors.New("NAME or FILE cannot be combined with --src, --tgt, --stream and the other ad-hoc flags")
			case len(args) == 1:
				r, err = loadReplicationArg(args[0])
			case adhocSet && cmd.Flags().Changed("streams"):
				return errors.New("--streams applies to NAME or FILE")
			case adhocSet:
				r, err = adhoc(src, tgt, stream, object, mode, primaryKey, updateKey, where, limit)
			default:
				return errors.New("NAME, FILE or --src --stream --tgt")
			}
			if err != nil {
				return err
			}
			if unknown := r.UnknownStreams(streams); len(unknown) > 0 {
				return fmt.Errorf("%s: no stream %s", r.Name, strings.Join(unknown, ", "))
			}

			conns, err := loadConns()
			if err != nil {
				return err
			}
			st, err := openState(cmd.Context())
			if err != nil {
				return err
			}
			defer st.Close()

			prog := newProgress(!flagQuiet)
			e := &engine.Engine{Conns: conns, State: st, Log: logger, Trigger: "manual", LockDir: daemon.LockDir(flagHome), OnEvent: prog.handle}
			if prog.tty && !flagVerbose {
				// the live line replaces the per-stream log lines on a terminal
				e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
			}
			results, runErr := e.Run(cmd.Context(), r, streams)
			prog.finish()
			if flagQuiet {
				for _, res := range results {
					if res.Err != nil {
						fmt.Fprintf(os.Stderr, "%s: %s\n", res.Stream, oneLine(res.Err.Error()))
					}
				}
			} else {
				printResults(results)
			}
			switch {
			case cmd.Context().Err() != nil:
				return errors.New("interrupted")
			case runErr != nil:
				return runErr
			case len(results) == 0:
				return fmt.Errorf("%s: no streams ran", r.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&streams, "streams", nil, "only these streams")
	cmd.Flags().StringVar(&src, "src", "", "source connection")
	cmd.Flags().StringVar(&stream, "stream", "", "source table, schema.table or pattern")
	cmd.Flags().StringVar(&tgt, "tgt", "", "target connection")
	cmd.Flags().StringVar(&object, "object", "", "target table")
	cmd.Flags().StringVar(&mode, "mode", "full-refresh", "full-refresh, truncate, incremental, backfill, snapshot")
	cmd.Flags().StringSliceVar(&primaryKey, "primary-key", nil, "primary key columns")
	cmd.Flags().StringVar(&updateKey, "update-key", "", "update key column")
	cmd.Flags().StringVar(&where, "where", "", "filter clause")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows")
	return cmd
}

// loadReplicationArg loads a file path, or HOME/replications/NAME.toml.
func loadReplicationArg(arg string) (*config.Replication, error) {
	if fi, err := os.Stat(arg); err == nil && !fi.IsDir() {
		return config.LoadReplication(arg)
	}
	if !config.ValidName(arg) {
		return nil, fmt.Errorf("%s: no such file", arg)
	}
	p, _ := config.ReplicationPath(flagHome, arg)
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("no replication %s (no file %s)", arg, p)
	}
	return config.LoadReplication(p)
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func adhoc(src, tgt, stream, object, mode string, pk []string, uk, where string, limit int) (*config.Replication, error) {
	if src == "" || tgt == "" || stream == "" {
		return nil, errors.New("an ad-hoc run needs --src, --tgt and --stream")
	}
	s := config.Stream{Mode: mode, Object: object, PrimaryKey: pk, UpdateKey: uk, Where: where, Limit: limit}
	if err := config.ValidateStream(stream, &s); err != nil {
		return nil, err
	}
	return &config.Replication{
		Stream: s, Source: src, Target: tgt, Name: "adhoc-" + unsafeName.ReplaceAllString(src+"-"+stream, "_"),
		Resolved: []config.ResolvedStream{{Name: stream, Stream: s}},
	}, nil
}

func printResults(results []engine.Result) {
	if len(results) == 0 {
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STREAM\tOBJECT\tROWS\tTIME\tSTATUS")
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", r.Stream, r.Object, r.Rows, r.Took.Round(time.Millisecond), resultStatus(r.Err))
	}
	w.Flush()
}

// resultStatus is ok, failed: cause, or cancelled.
func resultStatus(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, engine.ErrCancelled):
		return oneLine(err.Error())
	}
	return "failed: " + oneLine(err.Error())
}

// runStatuses are the --status words; success and error are accepted for ok and failed.
var runStatuses = []string{"ok", "failed", "running", "skipped", "cancelled"}

func runsCmd() *cobra.Command {
	var (
		limit  int
		status string
		since  string
	)
	cmd := &cobra.Command{
		Use:   "runs [NAME]",
		Short: "Run history, newest first",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f := state.RunFilter{Limit: limit, Status: state.StatusOf(status)}
			if len(args) == 1 {
				f.Replication = args[0]
			}
			if status != "" && !slices.Contains(runStatuses, state.StatusWord(status)) {
				return fmt.Errorf("--status %q: want %s", status, strings.Join(runStatuses, ", "))
			}
			if since != "" {
				t, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				f.Since = t
			}
			st, err := openState(cmd.Context())
			if err != nil {
				return err
			}
			defer st.Close()
			runs, err := st.Find(cmd.Context(), f)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tREPLICATION\tSTREAM\tOBJECT\tTRIGGER\tSTARTED\tTIME\tROWS\tSTATUS")
			for _, r := range runs {
				took := ""
				if r.Ended != nil {
					took = r.Ended.Sub(r.Started).Round(time.Millisecond).String()
				}
				st := state.StatusWord(r.Status)
				if r.Error != "" {
					st += ": " + oneLine(r.Error)
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", r.ID, r.Replication, state.StreamLabel(r.Stream), r.Object, r.Trigger,
					r.Started.Local().Format(time.DateTime), took, r.Rows, st)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 30, "rows to show, 0 for all")
	cmd.Flags().StringVar(&status, "status", "", "ok, failed, running, skipped or cancelled")
	cmd.Flags().StringVar(&since, "since", "", "24h, 7d, 2026-11-01 or 2026-11-01 08:00")
	return cmd
}

// parseSince reads a duration back from now (Go durations plus Nd for days) or a local
// date or date and time.
func parseSince(s string, now time.Time) (time.Time, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if d, err := time.ParseDuration(n + "h"); err == nil {
			return now.Add(-24 * d), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.DateOnly, "2006-01-02 15:04", time.DateTime, time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since %q: want 24h, 7d, 2026-11-01 or 2026-11-01 08:00", s)
}
