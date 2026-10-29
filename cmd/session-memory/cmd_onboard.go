package main

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/device"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
	"session-memory/internal/transport"
)

const (
	weiyunDirName  = "SessionMemory"
	weiyunSkillURL = "https://cdn.addon.tencentsuite.com/static/tencent-weiyun.zip"
)

// selfPath is this program's full path: what a person or agent types, since
// the folder it lives in is usually not on their PATH.
func selfPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "session-memory"
}

func needsWeiyunHint() string {
	return "NEEDS_WEIYUN: Weiyun is not connected on this computer. Install the Weiyun skill (" + weiyunSkillURL + "), finish its authorization (scan with WeChat or QQ), then run `" + selfPath() + " setup` again."
}

// How setup ended up connected; the last line says which.
type vaultHow int

const (
	vaultCreated   vaultHow = iota // nothing was in the Weiyun: this is the first computer
	vaultJoined                    // the vault was already there
	vaultConnected                 // this computer was set up before
)

func (h vaultHow) doneLine() string {
	switch h {
	case vaultCreated:
		return "done: FIRST_COMPUTER this computer is connected and is the first one in this Weiyun. If another computer was already set up, this login is probably a different Weiyun account: use the same WeChat/QQ login there."
	case vaultJoined:
		return "done: JOINED this computer joined the vault that was already in your Weiyun."
	}
	return "done: ALREADY_CONNECTED this computer was already set up; nothing else to do."
}

// relayOutcome says what a failed relay means for setup. Network trouble and
// the Weiyun quota pass by themselves (pending). Sessions that need a decision
// or a check (a conflict, a divergence, an interrupted install) do not: waiting
// never fixes them, so they are reported apart, with how many there are
// (blocked; 0 when the output does not say).
func relayOutcome(out string, err error) (pending bool, blocked int, needsAction bool) {
	s := strings.ToLower(out + " " + err.Error())
	if strings.Contains(s, string(errcode.RecoveryNeeded)) || strings.Contains(s, "need attention") {
		// "project x: updated 1, up to date 0, 2 need attention" lines add up;
		// otherwise the summary error "2 session(s) need attention"
		lower := strings.ToLower(out) // err repeats out
		for _, m := range blockedCount.FindAllStringSubmatch(lower, -1) {
			n, _ := strconv.Atoi(m[1])
			blocked += n
		}
		if blocked == 0 {
			if m := blockedSummary.FindStringSubmatch(lower); m != nil {
				blocked, _ = strconv.Atoi(m[1])
			}
		}
		return false, blocked, true
	}
	for _, k := range []string{"117401", strings.ToLower(transport.ErrQuota.Error()), "quota",
		"dial tcp", "no such host", "connection refused", "connection reset", "network is unreachable", "i/o timeout", "client.timeout", "deadline exceeded", "unexpected eof", "http 5"} {
		if strings.Contains(s, k) {
			return true, 0, false
		}
	}
	return false, 0, false
}

var (
	blockedCount   = regexp.MustCompile(`, (\d+) need attention`)
	blockedSummary = regexp.MustCompile(`(\d+) session\(s\) need attention`)
)

// needsActionLine is what setup prints when sessions are blocked: plain words
// for the user, no promise that waiting helps.
func needsActionLine(blocked int) string {
	what := "some sessions"
	if blocked == 1 {
		what = "1 session"
	} else if blocked > 1 {
		what = fmt.Sprintf("%d sessions", blocked)
	}
	return "NEEDS_ACTION: " + what + " did not sync and will NOT fix themselves (the same session was changed on two computers, or an earlier install was interrupted). Nothing was deleted, and everything else is connected and keeps syncing. Tell the user in plain words; to see which ones run `" + selfPath() + " relay`. Do not run adopt or choose unless the user asks."
}

// selfCommand runs one of this program's own commands; tests replace it.
var selfCommand = func(ctx context.Context, args ...string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	out, err := osexec.CommandContext(ctx, exe, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// runSelf runs one of our own commands, so setup is exactly the commands a
// person would type, one after the other.
func runSelf(ctx context.Context, args ...string) (string, error) {
	out, err := selfCommand(ctx, args...)
	if err != nil {
		return out, errcode.New(errcode.Internal, "session-memory "+strings.Join(args, " ")+": "+out)
	}
	return out, nil
}

// newSetupCmd is the one command for a new computer, and for repairing one:
// connect to Weiyun, create or join the vault, pick up the sessions of the
// installed agents, start the background sync and sync once. It asks nothing
// and can be run again at any time. Its last line says how it ended.
func newSetupCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "setup", Short: "One step for a new computer: connect, create or join the vault, start background sync, sync once", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			days, _ := cmd.Flags().GetInt("days")
			noAuto, _ := cmd.Flags().GetBool("no-autosync")
			say := func(format string, args ...any) { fmt.Fprintf(a.Out, format+"\n", args...) }

			if len(a.weiyunAccounts()) == 0 && a.Cfg.Transport.Folder == "" {
				return errcode.New(errcode.Usage, needsWeiyunHint())
			}
			agents := a.installedAgents() // before anything is created in the cloud
			if len(agents) == 0 {
				return errcode.New(errcode.NotFound, "no Codex or WorkBuddy found on this computer; open one of them once, then run `"+selfPath()+" setup` again")
			}
			how, err := a.joinOrCreateVault(ctx, say)
			if err != nil {
				return err
			}
			if a.Cfg.Transport.Weiyun != "" {
				if err := a.saveWeiyunToken(); err != nil { // background runs do not see this shell
					say("note: could not keep the Weiyun token for background sync: %v", err)
				}
			}
			if has(agents, "workbuddy") && !a.Cfg.Agents.WorkBuddy.AllowRegistryWrite {
				if err := config.Set("agents.workbuddy.allow_registry_write", "true"); err != nil {
					return errcode.Wrap(errcode.Usage, err.Error(), err)
				}
				a.Cfg.Agents.WorkBuddy.AllowRegistryWrite = true
			}
			n, skipped, err := a.trackAll(ctx, agents, days)
			if err != nil {
				return err
			}
			touch(a.keyMarker(markTrackNew)) // from now on every sync follows new project folders
			say("sessions of the last %d days (%s): %d covered, %d skipped (no project folder)", days, strings.Join(agents, ", "), n, skipped)
			a.repairCloudKey(ctx, say)

			if !noAuto {
				a.startBackgroundSync(ctx, say)
			}
			relay := []string{"relay"}
			if closed, _ := cmd.Flags().GetBool("client-closed"); closed {
				relay = append(relay, "--client-closed")
			}
			out, err := runSelf(ctx, relay...) // publishes everything not yet uploaded
			if out != "" {
				say("%s", out)
			}
			if err != nil {
				pending, blocked, needsAction := relayOutcome(out, err)
				switch {
				case needsAction:
					say("%s", needsActionLine(blocked))
				case pending:
					say("PENDING: connected; the first upload will finish by itself")
				default:
					return err
				}
			}
			if has(agents, "workbuddy") {
				say("WorkBuddy: sessions from your other computers are added after WorkBuddy is fully quit (Cmd+Q), within about ten minutes; reopen it afterwards.")
			}
			say("%s", how.doneLine())
			return nil
		}}
	c.Flags().Int("days", 60, "only sessions touched in the last N days are picked up")
	c.Flags().Bool("client-closed", false, "the agent apps are closed, so sessions can be installed right away")
	c.Flags().Bool("no-autosync", false, "do not start the background sync")
	return c
}

// joinOrCreateVault makes this computer a member of the vault in the user's
// Weiyun: it joins the one that is there, and creates it when there is none.
func (a *App) joinOrCreateVault(ctx context.Context, say func(string, ...any)) (vaultHow, error) {
	root := a.Cfg.StateDir()
	_, loaded := device.At(root).Load()
	if loaded == nil {
		if a.Cfg.Transport.Weiyun == "" && a.Cfg.Transport.Folder == "" {
			_, err := runSelf(ctx, "transport", "attach-weiyun", weiyunDirName)
			return vaultConnected, err
		}
		return vaultConnected, nil
	}
	if a.Cfg.Transport.Weiyun == "" && a.Cfg.Transport.Folder == "" {
		if err := config.Set("transport.weiyun", weiyunDirName); err != nil {
			return 0, errcode.Wrap(errcode.Usage, err.Error(), err)
		}
		a.Cfg.Transport.Weiyun = weiyunDirName
	}
	if _, _, err := a.fetchCloudKey(ctx); err == nil {
		if _, err := runSelf(ctx, "device", "join", "--from-cloud"); err != nil {
			return 0, err
		}
		os.Remove(a.keyMarker(markKeyHint)) // the guide says the key stays in the cloud; no second question
		say("joined the vault already in your Weiyun")
		return vaultJoined, nil
	} else if errcode.CodeOf(err) != errcode.NotFound {
		return 0, err
	}
	m, err := a.weiyunMirror(a.Cfg.Transport.Weiyun)
	if err != nil {
		return 0, err
	}
	// not knowing whether a vault is there must never end in a second one
	vaultFile := ""
	if m != nil {
		if err := m.Pull(ctx); err != nil {
			return 0, errcode.Wrap(errcode.Usage, "cannot reach Weiyun: "+err.Error(), err)
		}
		vaultFile = filepath.Join(a.weiyunCache(), "public", "vault.json")
	} else if a.Cfg.Transport.Folder != "" {
		vaultFile = filepath.Join(config.ExpandPath(a.Cfg.Transport.Folder), "public", "vault.json")
	}
	if vaultFile != "" && exists(vaultFile) {
		return 0, errcode.New(errcode.AuthFailed, "your cloud folder already holds a vault, but its key was removed from the cloud. On the computer that already syncs run `"+selfPath()+" key publish`, then run `"+selfPath()+" setup` here again")
	}
	if _, err := runSelf(ctx, "init"); err != nil {
		return 0, err
	}
	attach := []string{"transport", "attach-weiyun", weiyunDirName}
	if a.Cfg.Transport.Weiyun == "" {
		attach = []string{"transport", "attach-folder", a.Cfg.Transport.Folder}
	}
	if _, err := runSelf(ctx, attach...); err != nil {
		return 0, err
	}
	say("created a new vault in your cloud folder")
	return vaultCreated, nil
}

// repairCloudKey puts the vault key back in the cloud when an earlier attempt
// could not. A key the user removed on purpose stays removed.
func (a *App) repairCloudKey(ctx context.Context, say func(string, ...any)) {
	if !exists(a.keyMarker(markKeyPending)) {
		return
	}
	if _, err := a.publishCloudKey(ctx, false); err != nil {
		say("note: the vault key is not in your cloud folder yet (%v); it is retried on the next setup or relay", err)
		return
	}
	os.Remove(a.keyMarker(markKeyPending))
}

func has(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// installedAgents are the agents that have data on this computer.
func (a *App) installedAgents() []string {
	var out []string
	if a.Cfg.Agents.Codex.IsEnabled() {
		if st, err := os.Stat(a.Cfg.CodexHome()); err == nil && st.IsDir() {
			out = append(out, "codex")
		}
	}
	if a.Cfg.Agents.WorkBuddy.IsEnabled() {
		if st, err := os.Stat(a.Cfg.WorkBuddyHome()); err == nil && st.IsDir() {
			out = append(out, "workbuddy")
		}
	}
	return out
}

// trackAll is setup's use of trackFolders: local only, nothing is published
// here; the relay that follows uploads everything not yet uploaded.
func (a *App) trackAll(ctx context.Context, agents []string, days int) (tracked, skipped int, err error) {
	r, err := a.openRT(ctx, true)
	if err != nil {
		return 0, 0, err
	}
	defer r.Close()
	return a.trackFolders(ctx, r, agents, days, false)
}

// trackFolders brings every recent session of the given agents under
// SessionMemory, one project per working folder. It publishes nothing. A
// session whose folder is the home folder itself (or "/", unknown or gone) is
// left out and counted as skipped: a project there would claim everything
// below it. With onlyNew it only takes on folders that no project covers yet
// and sessions that are not tracked, which is what a sync after setup needs.
func (a *App) trackFolders(ctx context.Context, r *rt, agents []string, days int, onlyNew bool) (tracked, skipped int, err error) {
	home, _ := os.UserHomeDir()
	since := a.Now().Add(-time.Duration(days) * 24 * time.Hour)
	bound := map[string]bool{}
	if onlyNew {
		bs, _ := r.Store.Bindings("")
		for _, b := range bs {
			bound[b.AgentProfile.Agent+"|"+b.NativeHome+"|"+b.NativeSessionID] = true
		}
	}
	existing := loadProjects(a.Cfg.StateDir())
	for _, name := range agents {
		ad, err := a.adapter(name)
		if err != nil {
			continue
		}
		list, err := ad.Discover(ctx, agent.DiscoverOptions{})
		if err != nil {
			return tracked, skipped, err
		}
		seen := map[string]bool{}
		for _, s := range list {
			if t, err := session.ParseTime(s.UpdatedAt); err == nil && t.Before(since) {
				continue
			}
			if onlyNew && bound[name+"|"+s.Ref.Home+"|"+s.Ref.NativeID] {
				continue
			}
			cwd := filepath.Clean(s.Cwd)
			if s.Cwd == "" || cwd == "/" || cwd == filepath.Clean(home) {
				skipped++
				continue
			}
			if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
				skipped++
				continue
			}
			if onlyNew && coveredByProject(existing, cwd) {
				continue
			}
			seen[cwd] = true
		}
		var dirs []string
		for d := range seen {
			dirs = append(dirs, d)
		}
		// the deepest first, so a folder inside another keeps its own project
		sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
		for _, dir := range dirs {
			pm := session.ProjectMap{SchemaVersion: 1, ProjectRef: session.NewProjectRef()}
			for _, p := range existing {
				if p.LocalPath == dir {
					pm = p
				}
			}
			pm.LocalPath = dir
			if pm.Label == "" {
				pm.Label = filepath.Base(dir)
			}
			if !has(pm.Watch, name) {
				pm.Watch = append(pm.Watch, name)
			}
			if err := saveProject(a, pm); err != nil {
				return tracked, skipped, err
			}
			existing = append(existing, pm)
			t, already, err := trackProject(ctx, r, ad, pm)
			if err != nil {
				return tracked, skipped, err
			}
			tracked += t
			if !onlyNew {
				tracked += already
			}
		}
	}
	return tracked, skipped, nil
}

func coveredByProject(projects []session.ProjectMap, dir string) bool {
	for _, p := range projects {
		if p.LocalPath != "" && pathWithin(dir, p.LocalPath) {
			return true
		}
	}
	return false
}

// startBackgroundSync starts the background job. Some agents run commands in a
// sandbox that may not talk to launchd; then a double-clickable file is left
// that does it from the user's own session.
func (a *App) startBackgroundSync(ctx context.Context, say func(string, ...any)) {
	exe := selfPath()
	if runtime.GOOS != "darwin" {
		say("background sync is only available on macOS; run `%s relay` now and then", exe)
		return
	}
	if _, err := runSelf(ctx, "autosync", "enable"); err == nil {
		say("background sync is on (every few minutes, nothing to do)")
		return
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Desktop")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		dir = home
	}
	file := filepath.Join(dir, "开启同步.command")
	script := "#!/bin/bash\n\"" + exe + "\" autosync enable\necho\necho \"完成，可以关闭这个窗口。\"\n"
	if err := os.WriteFile(file, []byte(script), 0755); err != nil {
		say("NEEDS_USER: background sync could not be started from here. In Terminal run: %s autosync enable", exe)
		return
	}
	say("NEEDS_USER: background sync could not be started from this agent. Ask the user to double-click %s once (a Terminal window opens; close it once it says 完成); nothing else is needed", file)
}
