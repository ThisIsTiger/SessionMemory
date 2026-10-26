package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"session-memory/internal/errcode"
	"session-memory/internal/source/codex"
	"session-memory/internal/source/workbuddy"
)

func newAgentCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "agent", Short: "Agent adapters"}
	c.AddCommand(&cobra.Command{
		Use: "probe <agent>", Short: "Read-only structural probe of an agent's data", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "codex":
				res, err := codex.New(a.Cfg).Probe(cmd.Context())
				if err != nil {
					return err
				}
				if a.JSON {
					return a.printJSON(res)
				}
				fmt.Fprintf(a.Out, "agent: %s\ncompatibility: %s\n", res.Agent, res.Compatibility)
				for _, n := range res.Notes {
					fmt.Fprintf(a.Out, "note: %s\n", n)
				}
				for i, h := range res.Homes {
					fmt.Fprintf(a.Out, "home %d: exists=%v version=%s\n", i+1, h.Exists, h.ClientVer)
					fmt.Fprintf(a.Out, "  stores: %s\n", flagList(h.Stores))
					fmt.Fprintf(a.Out, "  file forms: %s\n", countList(h.FileForms))
					fmt.Fprintf(a.Out, "  capabilities: %s\n", strings.Join(h.Capabilities, ", "))
				}
				return nil
			case "workbuddy":
				res, err := workbuddy.New(a.Cfg).Probe(cmd.Context())
				if err != nil {
					return err
				}
				if a.JSON {
					return a.printJSON(res)
				}
				fmt.Fprintf(a.Out, "agent: %s\ncompatibility: %s\n", res.Agent, res.Compatibility)
				for i, h := range res.Homes {
					fmt.Fprintf(a.Out, "home %d: exists=%v\n  stores: %s\n  file forms: %s\n  capabilities: %s\n", i+1, h.Exists, flagList(h.Stores), countList(h.FileForms), strings.Join(h.Capabilities, ", "))
				}
				return nil
			case "claude":
				return errcode.New(errcode.Unsupported, args[0]+" probe is not implemented yet")
			}
			return errcode.New(errcode.Usage, "unknown agent: "+args[0])
		},
	})
	return c
}

func flagList(m map[string]bool) string {
	var out []string
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func countList(m map[string]int) string {
	var out []string
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
