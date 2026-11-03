package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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
			res, err := probe.Conn(cmd.Context(), c, probe.AllSchemas)
			if err != nil {
				return err
			}
			if c.Type == "api" {
				fmt.Printf("ok  HTTP %d  %s\n", res.Status, res.Took.Round(time.Millisecond))
			} else {
				fmt.Printf("ok  %d tables  %s\n", len(res.Tables), res.Took.Round(time.Millisecond))
			}
			return nil
		},
	}

	var schema string
	discover := &cobra.Command{
		Use:   "discover NAME",
		Short: "List the tables of a database connection",
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
			res, err := probe.Conn(cmd.Context(), c, schema)
			if err != nil {
				return err
			}
			for _, t := range res.Tables {
				fmt.Println(t)
			}
			return nil
		},
	}
	discover.Flags().StringVar(&schema, "schema", "", "schema (default: the connection's schema)")

	cmd.AddCommand(list, test, discover)
	return cmd
}
