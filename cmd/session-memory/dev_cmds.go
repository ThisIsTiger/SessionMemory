//go:build dev

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"session-memory/internal/errcode"
	"session-memory/internal/jsonl"
	"session-memory/internal/p0"
	"session-memory/internal/platform"
	"session-memory/internal/source/codex"
)

func (a *App) realHomes() []string {
	h := append([]string{}, a.Cfg.CodexHomes()...)
	h = append(h, a.Cfg.WorkBuddyHomes()...)
	return append(h, a.Cfg.ClaudeHome())
}

func (a *App) sandboxDir(name string) (string, error) {
	dir := filepath.Join(p0.SandboxRoot(a.Cfg.StateDir()), name)
	if err := p0.RequireSandbox(dir, a.realHomes()); err != nil {
		return "", err
	}
	return dir, nil
}

func registerDev(root *cobra.Command, a *App) {
	dev := &cobra.Command{Use: "dev", Short: "P0 experiment tooling (not in release builds)"}
	sb := &cobra.Command{Use: "sandbox", Short: "Isolated agent homes"}
	newc := &cobra.Command{Use: "new <name>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ag, _ := cmd.Flags().GetString("agent")
		dir, err := p0.NewSandbox(a.Cfg.StateDir(), args[0], ag)
		if err != nil {
			return err
		}
		if a.JSON {
			return a.printJSON(map[string]string{"dir": dir})
		}
		fmt.Fprintf(a.Out, "%s\nsource %s/env.sh before starting the client\n", dir, dir)
		return nil
	}}
	newc.Flags().String("agent", "codex", "codex or workbuddy")
	mark := &cobra.Command{Use: "mark <dir>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if ok, _ := cmd.Flags().GetBool("yes-this-is-not-my-daily-profile"); !ok {
			return errcode.New(errcode.Usage, "pass --yes-this-is-not-my-daily-profile to confirm")
		}
		return p0.MarkExternal(args[0])
	}}
	mark.Flags().Bool("yes-this-is-not-my-daily-profile", false, "confirm the directory is isolated")
	rm := &cobra.Command{Use: "rm <name>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return p0.RemoveSandbox(a.Cfg.StateDir(), args[0])
	}}
	sb.AddCommand(newc, mark, rm)

	inv := &cobra.Command{Use: "inventory", Short: "Read-only structural inventory of an agent home", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ag, _ := cmd.Flags().GetString("agent")
		home, _ := cmd.Flags().GetString("home-dir")
		out, _ := cmd.Flags().GetString("out")
		if home == "" {
			return errcode.New(errcode.Usage, "--home-dir is required")
		}
		res, err := p0.Collect(cmd.Context(), ag, home)
		if err != nil {
			return errcode.Wrap(errcode.NotFound, "cannot read the home", err)
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		if out != "" {
			return os.WriteFile(out, append(b, '\n'), 0600)
		}
		_, err = a.Out.Write(append(b, '\n'))
		return err
	}}
	inv.Flags().String("agent", "codex", "agent name")
	inv.Flags().String("home-dir", "", "agent home to inspect (read-only)")
	inv.Flags().String("out", "", "write JSON here instead of stdout")

	scrub := &cobra.Command{Use: "scrub", Short: "Scrub a JSONL file for use as a fixture", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		in, _ := cmd.Flags().GetString("in")
		out, _ := cmd.Flags().GetString("out")
		seed, _ := cmd.Flags().GetString("seed")
		if in == "" || out == "" {
			return errcode.New(errcode.Usage, "--in and --out are required")
		}
		src, err := jsonl.OpenSource(in)
		if err != nil {
			return err
		}
		rc, _, err := src.OpenAt(0)
		if err != nil {
			return err
		}
		defer rc.Close()
		f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return errcode.Wrap(errcode.Usage, "cannot create the output file (it must not exist)", err)
		}
		defer f.Close()
		return p0.Scrub(rc, f, p0.ScrubOptions{Seed: seed, Shift: 0})
	}}
	scrub.Flags().String("in", "", "input JSONL (.zst ok)")
	scrub.Flags().String("out", "", "output file")
	scrub.Flags().String("seed", "sm", "id-mapping seed")

	cx := &cobra.Command{Use: "codex", Short: "Codex sandbox experiments"}
	threadRe := regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	write := &cobra.Command{Use: "write", Short: "Place a rollout as a NEW thread in a sandbox (files only, no database)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		sbName, _ := cmd.Flags().GetString("sandbox")
		rollout, _ := cmd.Flags().GetString("rollout")
		tid, _ := cmd.Flags().GetString("thread-id")
		dir, err := a.sandboxDir(sbName)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(rollout)
		if err != nil {
			return err
		}
		if tid == "" {
			var first struct {
				Payload struct {
					ID string `json:"id"`
				} `json:"payload"`
			}
			if i := bytes.IndexByte(data, '\n'); i > 0 && json.Unmarshal(data[:i], &first) == nil {
				tid = first.Payload.ID
			}
		}
		if tid == "" {
			tid = threadRe.FindString(string(data[:min(len(data), 4096)]))
		}
		if tid == "" {
			return errcode.New(errcode.Usage, "cannot determine the thread id; pass --thread-id")
		}
		now := time.Now()
		p := filepath.Join(dir, "codex", "sessions", now.Format("2006"), now.Format("01"), now.Format("02"), "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+tid+".jsonl")
		if err := p0.RequireSandbox(p, a.realHomes()); err != nil {
			return err
		}
		if _, err := os.Stat(p); err == nil {
			return errcode.New(errcode.Conflict, "file already exists")
		}
		if err := platform.AtomicWrite(p, data, 0600); err != nil {
			return err
		}
		fmt.Fprintln(a.Out, p)
		return nil
	}}
	write.Flags().String("sandbox", "", "sandbox name")
	write.Flags().String("rollout", "", "rollout file to place")
	write.Flags().String("thread-id", "", "thread id (default: read from the file)")
	appendc := &cobra.Command{Use: "append", Short: "Replace an existing sandbox thread with a longer rollout (Codex publish protocol)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		sbName, _ := cmd.Flags().GetString("sandbox")
		rollout, _ := cmd.Flags().GetString("rollout")
		tid, _ := cmd.Flags().GetString("thread-id")
		dir, err := a.sandboxDir(sbName)
		if err != nil {
			return err
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "codex", "sessions", "*", "*", "*", "rollout-*-"+tid+".jsonl"))
		sort.Strings(matches)
		if len(matches) != 1 {
			return errcode.New(errcode.NotFound, "expected exactly one plain rollout for this thread in the sandbox")
		}
		if err := p0.RequireSandbox(matches[0], a.realHomes()); err != nil {
			return err
		}
		data, err := os.ReadFile(rollout)
		if err != nil {
			return err
		}
		return codex.ReplaceUnderLocks(filepath.Join(dir, "codex"), tid, matches[0], data)
	}}
	appendc.Flags().String("sandbox", "", "sandbox name")
	appendc.Flags().String("rollout", "", "longer rollout file")
	appendc.Flags().String("thread-id", "", "thread id")
	locks := &cobra.Command{Use: "locks", Short: "Probe thread-writer-locks (read-only)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		home, _ := cmd.Flags().GetString("home-dir")
		st, err := codex.LockStatus(home)
		if err != nil {
			return errcode.Wrap(errcode.NotFound, "no thread-writer-locks directory", err)
		}
		if a.JSON {
			return a.printJSON(st)
		}
		names := make([]string, 0, len(st))
		for n := range st {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(a.Out, "%s held=%v\n", n, st[n])
		}
		return nil
	}}
	locks.Flags().String("home-dir", "", "codex home")
	cx.AddCommand(write, appendc, locks)

	report := &cobra.Command{Use: "report", Short: "P0 reports"}
	rnew := &cobra.Command{Use: "new <E#>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p := filepath.Join("doc", "p0", args[0]+".md")
		if _, err := os.Stat(p); err == nil {
			return errcode.New(errcode.Conflict, "report already exists")
		}
		tpl := "# " + args[0] + "\n- 环境：客户端与版本 / OS / 隔离方式\n- 假设：\n- 步骤：\n- 观察：\n- 结论：通过 / 失败 / 部分通过\n- 对设计的影响：\n- 遗留问题：\n"
		return platform.AtomicWrite(p, []byte(tpl), 0644)
	}}
	report.AddCommand(rnew)
	dev.AddCommand(sb, inv, scrub, cx, report)
	root.AddCommand(dev)
}
