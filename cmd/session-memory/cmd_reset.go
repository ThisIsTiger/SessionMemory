package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"session-memory/internal/config"
)

// aside is where a file or folder goes when it is put aside rather than
// deleted: the same place with the time added to its name.
func aside(path, stamp string) string { return path + ".bak-" + stamp }

// newResetCmd puts this computer back to "never set up", so that setup (or a
// test of it) can start again from nothing. Nothing is deleted: the state and
// the settings are renamed, and the Weiyun folder, the credential store and
// the sessions of Codex and WorkBuddy are not touched.
func newResetCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "reset", Short: "Stop background sync and put this computer's state aside, so setup starts again from nothing", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			yes, _ := cmd.Flags().GetBool("yes")
			stamp := time.Now().Format("20060102-150405")
			home, _ := os.UserHomeDir()
			state := a.Cfg.StateDir()
			var move []string
			for _, p := range []string{state, config.GlobalConfigPath()} {
				if _, err := os.Stat(p); err == nil {
					move = append(move, p)
				}
			}
			job := autosyncPlist()
			_, jobErr := os.Stat(job)
			hasJob := runtime.GOOS == "darwin" && jobErr == nil
			shortcut := filepath.Join(home, "Desktop", "开启同步.command")
			_, shortcutErr := os.Stat(shortcut)

			if !yes {
				fmt.Fprintln(a.Out, "This would, on this computer only:")
				if hasJob {
					fmt.Fprintln(a.Out, "  - stop the background sync")
				}
				for _, p := range move {
					fmt.Fprintf(a.Out, "  - put %s aside as %s\n", p, aside(p, stamp))
				}
				if shortcutErr == nil {
					fmt.Fprintf(a.Out, "  - put %s aside\n", shortcut)
				}
				fmt.Fprintln(a.Out, "Nothing is deleted. Run it again with --yes to do it.")
				return nil
			}

			if hasJob {
				if _, err := runSelf(cmd.Context(), "autosync", "disable"); err != nil {
					return err // a running job would only recreate what is put aside
				}
				fmt.Fprintln(a.Out, "background sync is off")
			}
			for _, p := range move {
				to := aside(p, stamp)
				if err := os.Rename(p, to); err != nil {
					return err
				}
				fmt.Fprintf(a.Out, "put aside: %s -> %s\n", p, to)
			}
			if shortcutErr == nil {
				if err := os.Rename(shortcut, aside(shortcut, stamp)); err != nil {
					return err
				}
			}
			fmt.Fprintln(a.Out, "Not touched: the vault folder in your Weiyun, the saved Weiyun login in the credential store, and the sessions in Codex and WorkBuddy.")
			fmt.Fprintf(a.Out, "done: RESET this computer is back to never set up. Run `%s setup` to start again; delete the .bak-%s items once you no longer need them.\n", selfPath(), stamp)
			return nil
		}}
	c.Flags().Bool("yes", false, "do it (without this, only say what would happen)")
	return c
}
