package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"tummy/internal/blob"
	"tummy/internal/probe"
)

func connsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conns",
		Short: "Connections",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List connections, passwords masked",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conns, err := loadConns()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTYPE\tURL")
			for _, n := range conns.Names() {
				c := conns.Conn[n]
				fmt.Fprintf(w, "%s\t%s\t%s\n", n, c.Type, c.Target())
			}
			return w.Flush()
		},
	}

	var timeout time.Duration
	test := &cobra.Command{
		Use:   "test NAME",
		Short: "Open a database and count the tables of every schema, or reach an API's base_url",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			conns, err := loadConns()
			if err != nil {
				return err
			}
			c, ok := conns.Conn[args[0]]
			if !ok {
				_, err := conns.Get(args[0])
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			res, err := probe.Conn(ctx, c, probe.AllSchemas)
			if err != nil {
				return err
			}
			switch c.Type {
			case "api":
				fmt.Printf("ok  HTTP %d  %s\n", res.Status, res.Took.Round(time.Millisecond))
			case "azblob":
				fmt.Printf("ok  %d files  %s\n", len(res.Tables), res.Took.Round(time.Millisecond))
			default:
				fmt.Printf("ok  %d tables  %s\n", len(res.Tables), res.Took.Round(time.Millisecond))
			}
			return nil
		},
	}

	test.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "give up after")

	var schema, match string
	discover := &cobra.Command{
		Use:   "discover NAME",
		Short: "List the tables of a database connection, or the files of a container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			conns, err := loadConns()
			if err != nil {
				return err
			}
			c, ok := conns.Conn[args[0]]
			if !ok {
				_, err := conns.Get(args[0])
				return err
			}
			if c.Type == "api" {
				return fmt.Errorf("conn %s: an API connection has no tables", c.Name)
			}
			if match != "" && c.Type != "azblob" {
				return fmt.Errorf("conn %s: --match is for azblob connections", c.Name)
			}
			if match != "" {
				if _, err := blob.Patterns(match); err != nil {
					return fmt.Errorf("--match %q: %w", match, err)
				}
			}
			res, err := probe.Conn(cmd.Context(), c, schema)
			if err != nil {
				return err
			}
			for _, t := range res.Tables {
				if match == "" || blob.Match(match, t) {
					fmt.Println(t)
				}
			}
			return nil
		},
	}
	discover.Flags().StringVar(&schema, "schema", "", "schema (default: the connection's schema)")
	discover.Flags().StringVar(&match, "match", "", "azblob: files matching a stream path, as in 'reports/file[123].csv'")

	cmd.AddCommand(list, test, discover)
	return cmd
}
