package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"session-memory/internal/agent"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
)

func projectPath(root, ref string) string { return filepath.Join(root, "projects", ref+".json") }

func loadProject(root, ref string) (session.ProjectMap, error) {
	var pm session.ProjectMap
	if !session.ValidProjectRef(ref) {
		return pm, errcode.New(errcode.Usage, "bad project ref")
	}
	b, err := os.ReadFile(projectPath(root, ref))
	if err != nil {
		return pm, errcode.New(errcode.NotFound, "unknown project ref; run project map first")
	}
	return pm, json.Unmarshal(b, &pm)
}

func parseRules(rules []string) ([]session.PathRuleJSON, error) {
	var out []session.PathRuleJSON
	for _, r := range rules {
		from, to, ok := strings.Cut(r, "=")
		if !ok || from == "" || to == "" {
			return nil, errcode.New(errcode.Usage, "--rule must be FROM=TO")
		}
		out = append(out, session.PathRuleJSON{From: from, To: to})
	}
	return out, nil
}

func newProjectCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "project", Short: "Project directory mapping"}
	m := &cobra.Command{Use: "map [ref]", Short: "Create or update a project mapping on this device", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("path")
			rules, _ := cmd.Flags().GetStringArray("rule")
			label, _ := cmd.Flags().GetString("label")
			if path == "" {
				return errcode.New(errcode.Usage, "--path is required")
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			if st, err := os.Stat(abs); err != nil || !st.IsDir() {
				return errcode.New(errcode.Usage, "--path must be an existing directory")
			}
			rj, err := parseRules(rules)
			if err != nil {
				return err
			}
			ref := session.NewProjectRef()
			if len(args) == 1 {
				ref = args[0]
			}
			if !session.ValidProjectRef(ref) {
				return errcode.New(errcode.Usage, "bad project ref")
			}
			pm := session.ProjectMap{SchemaVersion: 1, ProjectRef: ref, Label: label, LocalPath: abs, Rules: rj}
			b, _ := json.MarshalIndent(pm, "", "  ")
			if err := platform.AtomicWrite(projectPath(a.Cfg.StateDir(), ref), append(b, '\n'), 0600); err != nil {
				return err
			}
			if a.JSON {
				return a.printJSON(pm)
			}
			fmt.Fprintln(a.Out, ref)
			return nil
		}}
	m.Flags().String("path", "", "local project directory")
	m.Flags().StringArray("rule", nil, "FROM=TO path prefix rewrite (repeatable)")
	m.Flags().String("label", "", "display label")
	c.AddCommand(m)
	return c
}

func newRestoreCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "restore <session_id>", Short: "Install a synced session into this device's agent", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("agent")
			mode, _ := cmd.Flags().GetString("mode")
			if mode != "native" {
				return errcode.New(errcode.Unsupported, "only --mode native is available; use the convert command for another agent's session")
			}
			ad, err := a.adapter(name)
			if err != nil {
				return err
			}
			opt, err := installOptions(a, cmd, name)
			if err != nil {
				return err
			}
			planOnly := opt.PlanOnly
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			plan, rec, err := r.Eng.Restore(cmd.Context(), ad, args[0], opt)
			if err != nil {
				return err
			}
			if a.JSON {
				return a.printJSON(map[string]any{"plan": plan, "receipt": rec})
			}
			for _, w := range plan.Writes {
				fmt.Fprintf(a.Out, "%s %s (%d bytes)\n", w.Op, w.Path, w.Size)
			}
			for _, warning := range plan.Warnings {
				fmt.Fprintf(a.Out, "warning: %s\n", warning)
			}
			if planOnly {
				fmt.Fprintf(a.Out, "plan %s (nothing written)\n", plan.Digest[:16])
			} else if rec != nil {
				fmt.Fprintf(a.Out, "installed revision %s\n", plan.RevisionID[:16])
			}
			return nil
		}}
	c.Flags().String("agent", "codex", "agent name")
	c.Flags().String("mode", "native", "native")
	addInstallFlags(c)
	return c
}

// addInstallFlags registers the flags shared by restore and convert.
func addInstallFlags(c *cobra.Command) {
	c.Flags().String("home-dir", "", "target agent home (default: the configured one)")
	c.Flags().Bool("plan", false, "show what would be written")
	c.Flags().Bool("client-closed", false, "confirm the client is not running")
	c.Flags().String("cwd", "", "local project directory (default: mapped session directory)")
	c.Flags().Bool("allow-code-mismatch", false, "explicitly continue with recorded code-state gaps; recorded in the import receipt")
	c.Flags().String("revision", "", "install this revision instead of the effective head")
	c.Flags().String("project", "", "project_ref whose path rules to apply")
	c.Flags().StringArray("rule", nil, "FROM=TO path prefix rewrite (repeatable)")
}

// installOptions turns the shared flags into engine options for target agent name.
func installOptions(a *App, cmd *cobra.Command, name string) (engine.RestoreOptions, error) {
	home, _ := cmd.Flags().GetString("home-dir")
	planOnly, _ := cmd.Flags().GetBool("plan")
	closed, _ := cmd.Flags().GetBool("client-closed")
	allowMismatch, _ := cmd.Flags().GetBool("allow-code-mismatch")
	cwd, _ := cmd.Flags().GetString("cwd")
	rev, _ := cmd.Flags().GetString("revision")
	proj, _ := cmd.Flags().GetString("project")
	rules, _ := cmd.Flags().GetStringArray("rule")
	if home == "" {
		if name == "workbuddy" {
			home = a.Cfg.WorkBuddyHome()
		} else {
			home = a.Cfg.CodexHome()
		}
	}
	if name == "workbuddy" && !a.Cfg.Agents.WorkBuddy.AllowRegistryWrite {
		return engine.RestoreOptions{}, errcode.New(errcode.Unsupported, "enable agents.workbuddy.allow_registry_write to use the version-limited native writer")
	}
	rj, err := parseRules(rules)
	if err != nil {
		return engine.RestoreOptions{}, err
	}
	if proj != "" {
		pm, err := loadProject(a.Cfg.StateDir(), proj)
		if err != nil {
			return engine.RestoreOptions{}, err
		}
		rj = append(rj, pm.Rules...)
		if cwd == "" {
			cwd = pm.LocalPath
		}
	}
	var pmap agent.PathMap
	for _, r := range rj {
		pmap.Rules = append(pmap.Rules, agent.PathRule{From: r.From, To: r.To})
	}
	return engine.RestoreOptions{Home: home, PathMap: pmap, ClientClosed: closed, PlanOnly: planOnly, RevisionID: rev, Cwd: cwd, AllowCodeMismatch: allowMismatch}, nil
}

func newVerifyCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "verify [session_id]", Short: "Check local state; --deep re-hashes captured data", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deep, _ := cmd.Flags().GetBool("deep")
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			only := ""
			if len(args) == 1 {
				only = args[0]
			}
			bs, err := r.Store.Bindings(only)
			if err != nil {
				return err
			}
			problems := 0
			for _, b := range bs {
				if b.State != session.StateActive {
					continue
				}
				ad, err := a.adapter(b.AgentProfile.Agent)
				if err != nil {
					continue
				}
				vb, ok := ad.(interface {
					VerifyBinding(context.Context, agent.NativeRef, map[string]session.ArtifactState, bool) ([]string, error)
				})
				if !ok {
					continue
				}
				bad, err := vb.VerifyBinding(cmd.Context(), agent.NativeRef{Home: b.NativeHome, NativeID: b.NativeSessionID}, b.Artifacts, deep)
				if err != nil {
					return err
				}
				problems += len(bad)
				for _, p := range bad {
					fmt.Fprintf(a.Err, "mismatch: %s\n", p)
				}
				if b.Unsafe != nil {
					problems++
					fmt.Fprintf(a.Err, "unsafe record in %s at offset %d\n", b.Unsafe.Path, b.Unsafe.Offset)
				}
			}
			if a.JSON {
				a.printJSON(map[string]any{"problems": problems, "deep": deep})
			} else if problems == 0 {
				fmt.Fprintln(a.Out, "ok")
			}
			if problems > 0 {
				return errcode.New(errcode.RecoveryNeeded, fmt.Sprintf("%d problem(s) found", problems))
			}
			return nil
		}}
	c.Flags().Bool("deep", false, "re-hash every captured segment against the source")
	return c
}
