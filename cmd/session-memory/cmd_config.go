package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"session-memory/internal/config"
	"session-memory/internal/errcode"
)

func newConfigCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Show or change configuration"}
	c.AddCommand(
		&cobra.Command{Use: "show", Short: "Show every key with its source", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
			src, err := config.LoadSources()
			if err != nil {
				return errcode.Wrap(errcode.Usage, err.Error(), err)
			}
			if a.JSON {
				type row struct {
					Key    string `json:"key"`
					Value  string `json:"value"`
					Source string `json:"source"`
				}
				rows := make([]row, len(src))
				for i, s := range src {
					rows[i] = row{s.Key, s.Value, s.Source}
				}
				return a.printJSON(rows)
			}
			w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
			for _, s := range src {
				fmt.Fprintf(w, "%s\t%s\t(%s)\n", s.Key, s.Value, s.Source)
			}
			return w.Flush()
		}},
		&cobra.Command{Use: "get <key>", Short: "Print one effective value", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
			if !config.IsValidKey(args[0]) {
				return errcode.New(errcode.Usage, "unknown key: "+args[0])
			}
			src, err := config.LoadSources()
			if err != nil {
				return errcode.Wrap(errcode.Usage, err.Error(), err)
			}
			for _, s := range src {
				if s.Key == args[0] {
					if a.JSON {
						return a.printJSON(map[string]string{"key": s.Key, "value": s.Value, "source": s.Source})
					}
					_, err := fmt.Fprintln(a.Out, s.Value)
					return err
				}
			}
			return nil
		}},
		&cobra.Command{Use: "set <key> <value>", Short: "Set a key in the global config", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
			if !config.IsValidKey(args[0]) {
				return errcode.New(errcode.Usage, "unknown key: "+args[0])
			}
			if err := config.Set(args[0], args[1]); err != nil {
				return errcode.Wrap(errcode.Usage, err.Error(), err)
			}
			if a.JSON {
				return a.printJSON(map[string]string{"key": args[0], "value": args[1]})
			}
			return nil
		}},
		&cobra.Command{Use: "path", Short: "Print the global config path", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
			p := config.GlobalConfigPath()
			if a.JSON {
				return a.printJSON(map[string]string{"path": p})
			}
			_, err := fmt.Fprintln(a.Out, p)
			return err
		}},
	)
	return c
}
