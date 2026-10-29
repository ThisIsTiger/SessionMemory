package main

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"session-memory/internal/errcode"
	"session-memory/internal/platform"
)

const autosyncLabel = "com.sessionmemory.relay"

func autosyncPlist() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", autosyncLabel+".plist")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func launchctl(args ...string) (string, error) {
	out, err := osexec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// newAutosyncCmd runs relay in the background (macOS launch agent) so that
// handing work over needs no command at all.
func newAutosyncCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "autosync", Short: "Run relay in the background every few minutes (macOS)"}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	needMac := func() error {
		if runtime.GOOS != "darwin" {
			return errcode.New(errcode.Unsupported, "autosync is only available on macOS; run relay from a scheduler on this system")
		}
		return nil
	}
	enable := &cobra.Command{Use: "enable", Short: "Install and start the background job", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := needMac(); err != nil {
				return err
			}
			interval, _ := cmd.Flags().GetInt("interval")
			if interval < 60 {
				return errcode.New(errcode.Usage, "--interval must be at least 60 seconds")
			}
			if a.Cfg.Transport.Weiyun != "" {
				if err := a.saveWeiyunToken(); err != nil {
					return err
				}
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return err
			}
			logPath := filepath.Join(a.Cfg.StateDir(), "autosync.log")
			env := "<key>PATH</key><string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>"
			if h := os.Getenv("SM_HOME"); h != "" {
				env += "<key>SM_HOME</key><string>" + xmlEscape(h) + "</string>"
			}
			plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + autosyncLabel + `</string>
<key>ProgramArguments</key><array><string>` + xmlEscape(exe) + `</string><string>relay</string><string>--quiet</string></array>
<key>RunAtLoad</key><true/>
<key>StartInterval</key><integer>` + strconv.Itoa(interval) + `</integer>
<key>ProcessType</key><string>Background</string>
<key>EnvironmentVariables</key><dict>` + env + `</dict>
<key>StandardOutPath</key><string>` + xmlEscape(logPath) + `</string>
<key>StandardErrorPath</key><string>` + xmlEscape(logPath) + `</string>
</dict></plist>
`
			p := autosyncPlist()
			if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				return err
			}
			if err := platform.AtomicWrite(p, []byte(plist), 0644); err != nil {
				return err
			}
			launchctl("bootout", domain+"/"+autosyncLabel) // replace a running older definition
			if out, err := launchctl("bootstrap", domain, p); err != nil {
				launchctl("load", "-w", p) // the old interface; its exit status is not to be trusted
				// only launchd's own answer says whether the job is there
				if _, perr := launchctl("print", domain+"/"+autosyncLabel); perr != nil {
					return errcode.New(errcode.Internal, "launchd refused to load the job ("+strings.TrimSpace(out)+"). The job file is in place and loads at your next login (log out and in, or restart)")
				}
			}
			fmt.Fprintf(a.Out, "autosync is on: relay runs every %d seconds; log: %s\n", interval, logPath)
			return nil
		}}
	enable.Flags().Int("interval", 120, "seconds between runs")
	disable := &cobra.Command{Use: "disable", Short: "Stop and remove the background job", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := needMac(); err != nil {
				return err
			}
			launchctl("bootout", domain+"/"+autosyncLabel)
			if err := os.Remove(autosyncPlist()); err != nil && !os.IsNotExist(err) {
				return err
			}
			fmt.Fprintln(a.Out, "autosync is off")
			return nil
		}}
	status := &cobra.Command{Use: "status", Short: "Show whether the background job runs and its last log lines", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := needMac(); err != nil {
				return err
			}
			if _, err := os.Stat(autosyncPlist()); err != nil {
				fmt.Fprintln(a.Out, "autosync is off")
				return nil
			}
			if _, err := launchctl("print", domain+"/"+autosyncLabel); err != nil {
				fmt.Fprintln(a.Out, "autosync is NOT running: the job file exists but launchd has not loaded it (it loads at the next login, or run autosync enable from your own Terminal)")
				return nil
			}
			fmt.Fprintln(a.Out, "autosync is on: running (scheduled)")
			accts := a.weiyunAccounts()
			stopped := 0
			for _, ac := range accts {
				if until, paused := a.quotaPausedUntil(ac.id); paused {
					stopped++
					if a.loadQuota()[ac.id].Rate {
						fmt.Fprintf(a.Out, "Weiyun account %s: asked too often for now; left alone until %s\n", ac.id, until.Format("15:04"))
					} else {
						fmt.Fprintf(a.Out, "Weiyun account %s: daily quota used up; left alone until %s\n", ac.id, until.Format("2006-01-02 15:04"))
					}
				}
			}
			if stopped > 0 && stopped < len(accts) {
				fmt.Fprintln(a.Out, "syncing continues through the other account")
			} else if stopped > 0 {
				fmt.Fprintln(a.Out, "paused: every account is out of quota; background runs resume when the first one is back (a run you start yourself still tries)")
			} else if w := idleWait(a.loadIdle().Quiet); w > 0 {
				fmt.Fprintf(a.Out, "quiet: nothing has changed for a while, so it checks about every %d minutes (any change brings it back to every 2)\n", int(w.Minutes()))
			}
			if b, err := os.ReadFile(filepath.Join(a.Cfg.StateDir(), "autosync.log")); err == nil {
				lines := strings.Split(strings.TrimSpace(string(b)), "\n")
				if len(lines) > 8 {
					lines = lines[len(lines)-8:]
				}
				fmt.Fprintln(a.Out, strings.Join(lines, "\n"))
			}
			return nil
		}}
	c.AddCommand(enable, disable, status)
	return c
}
