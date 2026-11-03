package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"tummy/internal/tui"
)

func tuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Interactive view (the default)",
		Args:  cobra.NoArgs,
		RunE:  runTUI,
	}
}

func runTUI(cmd *cobra.Command, _ []string) error {
	st, err := openState(cmd.Context())
	if err != nil {
		return err
	}
	defer st.Close()
	// the TUI appends to tui.log next to the state database; a new one is 0600
	logPath := filepath.Join(filepath.Dir(st.Path), "tui.log")
	if _, err := os.Stat(logPath); errors.Is(err, fs.ErrNotExist) {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.Close()
		}
	}
	return tui.Run(tui.Options{Home: flagHome, Connections: flagConnections, State: st})
}
