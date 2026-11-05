package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"tummy/internal/config"
	"tummy/internal/daemon"
	"tummy/internal/db"
	"tummy/internal/state"
)

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run replications on their schedules and serve queued runs; SIGHUP reloads",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open(flagState)
			if err != nil {
				return err
			}
			defer st.Close()
			d := daemon.New(flagHome, st, logger)
			if v := os.Getenv("TUMMY_TICK"); v != "" { // tests: a fast heartbeat
				if t, err := time.ParseDuration(v); err == nil && t > 0 {
					d.Tick = t
				}
			}
			d.Connections = flagConnections
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			go func() {
				for {
					select {
					case <-hup:
						d.Reload()
					case <-cmd.Context().Done():
						return
					}
				}
			}()
			return d.Run(cmd.Context())
		},
	}
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Replications with schedule and next run; exit 1 if any is broken",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			files, err := config.ListReplicationFiles(flagHome)
			if err != nil {
				return err
			}
			conns, connErr := loadConns()
			st, err := openState(cmd.Context())
			if err != nil {
				return err
			}
			defer st.Close()
			sched := daemonSchedules(cmd.Context(), st)
			bad := false
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSOURCE\tTARGET\tSTREAMS\tSCHEDULE\tENABLED\tNEXT RUN\tERROR")
			for _, f := range files {
				name := config.NameOf(f)
				r, err := config.LoadReplication(f)
				if err != nil {
					bad = true
					fmt.Fprintf(w, "%s\t\t\t\t\t\t\t%s\n", name, oneLine(strings.TrimPrefix(err.Error(), f+": ")))
					continue
				}
				var problems []string
				conn := func(n string) string {
					if conns != nil && conns.Conn[n] == nil {
						problems = append(problems, "unknown connection "+n)
						return n + "?"
					}
					return n
				}
				src, tgt := conn(r.Source), conn(r.Target)
				next := nextRun(name, r, sched, time.Now())
				if sc, ok := sched[name]; ok && sc.Error != "" {
					problems = append(problems, "daemon: "+oneLine(sc.Error))
				}
				bad = bad || len(problems) > 0
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%v\t%s\t%s\n", name, src, tgt, len(r.Resolved), r.Schedule, r.IsEnabled(), next, strings.Join(problems, "; "))
			}
			w.Flush()
			if connErr != nil {
				return connErr
			}
			if bad {
				return exitCode(1)
			}
			return nil
		},
	}
}

// nextRun is when r runs next: the time a live daemon serving this home published, else
// the schedule's next fire from now. "" when nothing will fire.
func nextRun(name string, r *config.Replication, sched map[string]state.Schedule, now time.Time) string {
	if sched != nil {
		if sc := sched[name]; sc.NextRun != nil {
			return sc.NextRun.Local().Format(time.DateTime)
		}
		return ""
	}
	if r == nil || !r.IsEnabled() || r.Schedule == "" {
		return ""
	}
	s, err := config.ParseSchedule(r.Schedule)
	if err != nil {
		return ""
	}
	return config.NextRun(s, now).Local().Format(time.DateTime)
}

// daemonSchedules returns what a live daemon serving this home published, else nothing.
func daemonSchedules(ctx context.Context, st *state.Store) map[string]state.Schedule {
	dm, err := st.DaemonStatus(ctx)
	if err != nil || !dm.Alive() || !dm.Serves(flagHome) {
		return nil
	}
	sched, _ := st.Schedules(ctx)
	return sched
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Daemon, load errors, failed runs and missed schedules; exit 1 on any",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			files, err := config.ListReplicationFiles(flagHome)
			if err != nil {
				return err
			}
			st, err := openState(ctx)
			if err != nil {
				return err
			}
			defer st.Close()
			dm, err := st.DaemonStatus(ctx)
			if err != nil {
				return err
			}
			latest, err := st.Latest(ctx)
			if err != nil {
				return err
			}
			sched := daemonSchedules(ctx, st)
			serving := sched != nil
			now := time.Now()
			bad, scheduled := false, false
			conns, connErr := loadConns()
			if connErr != nil {
				bad = true
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATUS\tLAST RUN\tNEXT RUN\tDETAIL")
			for _, f := range files {
				name := config.NameOf(f)
				status, detail := "ok", ""
				r, err := config.LoadReplication(f)
				switch {
				case err != nil:
					status, detail = "error", strings.TrimPrefix(err.Error(), f+": ")
				case sched[name].Error != "":
					status, detail = "error", "daemon: "+sched[name].Error
				case conns != nil && conns.Conn[r.Source] == nil:
					status, detail = "error", "unknown connection "+r.Source
				case conns != nil && conns.Conn[r.Target] == nil:
					status, detail = "error", "unknown connection "+r.Target
				}
				if err == nil && r.IsEnabled() && r.Schedule != "" {
					scheduled = true
				}
				var last, lastStream time.Time
				for stream, run := range latest[name] {
					if run.Started.After(last) {
						last = run.Started
					}
					if stream != "*" && run.Started.After(lastStream) {
						lastStream = run.Started
					}
				}
				if status == "ok" {
					// failed before running before cancelled; a "*" row is a failure before
					// any stream ran, superseded by a later stream run
					rank := map[string]int{"cancelled": 1, "running": 2, "error": 3}
					best := 0
					for stream, run := range latest[name] {
						if stream == "*" && run.Started.Before(lastStream) || rank[run.Status] <= best {
							continue
						}
						best = rank[run.Status]
						status, detail = state.StatusWord(run.Status), state.StreamLabel(stream)
						if run.Error != "" {
							detail += ": " + run.Error
						}
						if run.Status == "running" {
							detail = ""
						}
					}
				}
				if err != nil {
					r = nil
				}
				next := nextRun(name, r, sched, now)
				if sc, ok := sched[name]; ok && sc.NextRun != nil && now.Sub(*sc.NextRun) > 2*time.Minute && status == "ok" {
					status = "missed"
				}
				lastS := ""
				if !last.IsZero() {
					lastS = last.Local().Format(time.DateTime)
				}
				bad = bad || (status != "ok" && status != "running" && status != "cancelled")
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, status, lastS, next, oneLine(detail))
			}
			w.Flush()

			if connErr != nil {
				fmt.Printf("\n%s\n", oneLine(connErr.Error()))
			}
			switch {
			case serving:
				fmt.Printf("\ndaemon running, pid %d, since %s\n", dm.PID, dm.Started.Local().Format(time.DateTime))
			case dm.Alive():
				fmt.Printf("\ndaemon running for %s, not %s\n", dm.Home, absPath(flagHome))
				bad = bad || scheduled
			default:
				fmt.Println("\ndaemon not running")
				bad = bad || scheduled
			}
			if bad {
				return exitCode(1)
			}
			return nil
		},
	}
}

func sameDir(a, b string) bool { return absPath(a) == absPath(b) }

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func checkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check [NAME|FILE]",
		Short: "Validate connections and replications, show resolved streams; exit 1 on errors",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bad := false
			conns, err := loadConns()
			if err != nil {
				fmt.Printf("%s\n  error  %s\n\n", flagConnections, oneLine(err.Error()))
				bad = true
			}
			var files []string
			if len(args) == 1 {
				if fi, err := os.Stat(args[0]); err == nil && !fi.IsDir() {
					files = []string{args[0]}
				} else if p, err := config.ReplicationPath(flagHome, args[0]); err == nil {
					files = []string{p}
				} else {
					return err
				}
			} else if files, err = config.ListReplicationFiles(flagHome); err != nil {
				return err
			}
			var sched map[string]state.Schedule
			if _, err := os.Stat(flagState); err == nil {
				if st, err := openState(cmd.Context()); err == nil {
					sched = daemonSchedules(cmd.Context(), st)
					st.Close()
				}
			}
			for _, f := range files {
				if !checkFile(cmd.Context(), f, conns, sched) {
					bad = true
				}
			}
			if bad {
				return exitCode(1)
			}
			return nil
		},
	}
}

// checkFile prints one replication as it would run. It reports whether it is sound.
// sched is what a live daemon published, nil without one.
func checkFile(ctx context.Context, path string, conns *config.Connections, sched map[string]state.Schedule) bool {
	fmt.Println(path)
	defer fmt.Println()
	fail := func(err error) bool {
		fmt.Printf("  error     %s\n", oneLine(strings.TrimPrefix(err.Error(), path+": ")))
		return false
	}
	r, err := config.LoadReplication(path)
	if err != nil {
		return fail(err)
	}
	ok := true
	conn := func(label, name string) *config.Conn {
		if conns == nil {
			fmt.Printf("  %-9s %s\n", label, name)
			return nil
		}
		c, err := conns.Get(name)
		if err != nil {
			fmt.Printf("  %-9s %s\n", label, name)
			ok = fail(err)
			return nil
		}
		fmt.Printf("  %-9s %s (%s)\n", label, name, c.Type)
		return c
	}
	src := conn("source", r.Source)
	if tgt := conn("target", r.Target); tgt != nil && tgt.Type == "api" {
		ok = fail(fmt.Errorf("target %s is an API connection", r.Target))
	}
	switch {
	case r.Schedule == "":
		fmt.Println("  schedule  none")
	default:
		if !sameDir(filepath.Dir(path), config.ReplicationsDir(flagHome)) {
			sched = nil // the daemon serves the home's file of this name, not this one
		}
		line := "  schedule  " + r.Schedule
		if next := nextRun(r.Name, r, sched, time.Now()); next != "" {
			line += "  next " + next
		}
		if !r.IsEnabled() {
			line += "  disabled"
		}
		fmt.Println(line)
	}
	if r.OnError != "" {
		fmt.Printf("  on_error  %s\n", r.OnError)
	}

	streams := r.Resolved
	wild := false
	for _, s := range r.Resolved {
		wild = wild || s.IsWildcard()
	}
	if wild && src != nil && src.Type != "api" {
		d, err := db.Open(ctx, src.DB())
		if err != nil {
			ok = fail(err)
		} else {
			defer d.Close()
			expanded, err := r.Expand(func(schema string) ([]string, error) {
				ts, err := d.ListTables(ctx, schema)
				var out []string
				for _, t := range ts {
					out = append(out, t.String())
				}
				return out, err
			})
			if err != nil {
				ok = fail(err)
			} else {
				streams = expanded
			}
		}
	}
	fmt.Println("  streams")
	for _, s := range streams {
		line := "    " + s.Name
		if s.From != "" {
			line += "  from " + s.From
		}
		if s.Disabled {
			line += "  disabled"
		}
		fmt.Println(line)
		var set, inherited []string
		own := map[string]bool{}
		for _, k := range s.Set {
			own[k] = true
		}
		for _, kv := range s.Values() {
			if kv.Key == "disabled" {
				continue
			}
			pair := kv.Key + " = " + oneLine(kv.Value)
			if own[kv.Key] {
				set = append(set, pair)
			} else {
				inherited = append(inherited, pair)
			}
		}
		if len(set) > 0 {
			fmt.Printf("      set        %s\n", strings.Join(set, ", "))
		}
		if len(inherited) > 0 {
			fmt.Printf("      inherited  %s\n", strings.Join(inherited, ", "))
		}
	}
	if len(streams) == 0 {
		ok = fail(fmt.Errorf("no streams match"))
	}
	if ok {
		fmt.Println("  ok")
	}
	return ok
}
