package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"tummy/internal/txt"

	"github.com/spf13/cobra"

	"tummy/internal/config"
	"tummy/internal/daemon"
	"tummy/internal/state"
)

var (
	flagHome        string
	flagConnections string
	flagState       string
	flagVerbose     bool
	flagQuiet       bool
	logger          = slog.New(slog.NewTextHandler(io.Discard, nil))
)

// exitCode ends the program with a status and no message; the command printed its own.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // a second signal ends the process at once
	}()
	err := newRoot().ExecuteContext(ctx)
	stop()
	var code exitCode
	switch {
	case errors.As(err, &code):
		os.Exit(int(code))
	case err != nil:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "tummy",
		Short:         "Move tables between databases and REST APIs",
		Version:       version(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			lvl := slog.LevelInfo
			switch {
			case flagVerbose:
				lvl = slog.LevelDebug
			case flagQuiet:
				lvl = slog.LevelError
			}
			logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
			if flagConnections == "" {
				flagConnections = config.ConnectionsPath(flagHome)
			}
			return config.LoadEnvFile(config.EnvFile(flagHome))
		},
		RunE: runTUI,
	}
	root.SetVersionTemplate("tummy {{.Version}}\n")
	pf := root.PersistentFlags()
	pf.StringVar(&flagHome, "home", config.Home(), "config directory (TUMMY_HOME, XDG_CONFIG_HOME)")
	pf.StringVar(&flagConnections, "connections", "", "connections file (TUMMY_CONNECTIONS; default HOME/connections.toml)")
	pf.StringVar(&flagState, "state", state.DefaultPath(), "state database (TUMMY_STATE, XDG_STATE_HOME)")
	pf.BoolVarP(&flagVerbose, "verbose", "v", false, "debug logging")
	pf.BoolVarP(&flagQuiet, "quiet", "q", false, "errors only")
	root.MarkFlagsMutuallyExclusive("verbose", "quiet")
	root.AddCommand(connsCmd(), runCmd(), runsCmd(), listCmd(), checkCmd(), statusCmd(), serveCmd(), tuiCmd())
	return root
}

// version is the module version, or the VCS revision of a build without one.
func version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v + " " + bi.GoVersion
	}
	v := "dev"
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" {
		v += " " + rev + dirty
	}
	return v + " " + bi.GoVersion
}

func loadConns() (*config.Connections, error) {
	return config.LoadConnections(flagConnections)
}

// openState opens the state database and marks runs whose process is gone as failed.
func openState(ctx context.Context) (*state.Store, error) {
	st, err := state.Open(flagState)
	if err != nil {
		return nil, err
	}
	if _, err := st.AbandonDead(ctx, daemon.LockDir(flagHome), daemon.LegacyLockDir(st.Path)); err != nil {
		logger.Warn("abandon", "err", err)
	}
	return st, nil
}

// oneLine joins a multi-line error so a table cell keeps the whole cause.
func oneLine(s string) string { return txt.OneLine(s) }
