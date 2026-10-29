package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"session-memory/internal/device"
	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
	syncpkg "session-memory/internal/sync"
)

// Codex can run a command when a reply finishes (hooks). `hooks install` adds
// ours. The background job and the hooks share the work by what each is good
// at:
//
//   - a hook knows that something was just written, so it decides WHEN local
//     progress is uploaded: once a quiet minute has passed since the last
//     reply (at most hookMaxWait after the first one of a conversation). That
//     is one upload per burst of conversation, not one per reply and not one per
//     background round, and it asks the cloud nothing when there is nothing new;
//   - the background job is the only one that learns what other computers sent,
//     so it keeps polling, and it is the safety net: while hooks are plainly
//     working it leaves Codex's fresh progress to them, but uploads anything
//     that has waited hookSafetyAge (hooks not trusted, not run, or a worker
//     that died), and everything that is not Codex's, as before.

var (
	// hookQuiet is how long after the last reply the upload waits, so a
	// conversation goes up in one batch.
	hookQuiet = 75 * time.Second
	// hookMaxWait bounds the wait when replies keep coming.
	hookMaxWait = 5 * time.Minute
	// hookPoll is how often a waiting worker looks again (a new reply moves the
	// deadline).
	hookPoll = 2 * time.Second
	// hookSafetyAge is how long the background job leaves Codex progress to the
	// hooks before it uploads it itself; hookLead is how far before a pending
	// revision a hook event still counts as "hooks are working".
	hookSafetyAge = 10 * time.Minute
	hookLead      = 10 * time.Minute
	// hookMaxRounds bounds how many uploads one worker runs back to back.
	hookMaxRounds = 5
	// hookSpawn starts the worker in its own process; tests replace it.
	hookSpawn = func(a *App) error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := osexec.Command(exe, "hook", "--worker")
		cmd.Env = os.Environ()
		platform.DetachProcess(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
)

// hookState is what the hook remembers, for `hooks status`.
type hookState struct {
	Event string    `json:"event"`
	At    time.Time `json:"at"`
	Count int       `json:"count"`
}

func (a *App) hookStateFile() string { return filepath.Join(a.Cfg.StateDir(), "hook-state.json") }
func (a *App) hookDirtyFile() string { return filepath.Join(a.Cfg.StateDir(), "hook.dirty") }
func (a *App) hookLockFile() string  { return filepath.Join(a.Cfg.StateDir(), "locks", "hook.lock") }

func (a *App) loadHookState() hookState {
	var s hookState
	if b, err := os.ReadFile(a.hookStateFile()); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func (a *App) saveHookState(s hookState) {
	b, _ := json.Marshal(s)
	platform.AtomicWrite(a.hookStateFile(), b, 0600)
}

// hookDirtySince is when the first reply was written that has not been
// uploaded yet; false when none is waiting.
func (a *App) hookDirtySince() (time.Time, bool) {
	b, err := os.ReadFile(a.hookDirtyFile())
	if err != nil {
		return time.Time{}, false
	}
	t, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return t, true // an unreadable time counts as long ago: upload now
}

// markHookDirty notes a reply that is not uploaded yet, keeping the time of
// the first one.
func (a *App) markHookDirty() error {
	f, err := os.OpenFile(a.hookDirtyFile(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(a.Now().Format(time.RFC3339Nano))
	return err
}

// hookWorkerRunning says whether a worker holds the lock.
func (a *App) hookWorkerRunning() bool {
	f, err := platform.Touch(a.hookLockFile())
	if err != nil {
		return false
	}
	defer f.Close()
	if platform.LockExclusiveNonBlocking(f) != nil {
		return true
	}
	platform.Unlock(f)
	return false
}

// hookLog keeps what went wrong where nobody is looking: a hook must never
// print into, or fail, the agent that ran it.
func (a *App) hookLog(format string, args ...any) {
	p := filepath.Join(a.Cfg.StateDir(), "autosync.log")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] hook: %s\n", a.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// drain reads what the host sends on stdin (a JSON description of the event,
// not needed here) so that its writer never meets a closed pipe, but never
// waits long for a host that keeps the pipe open.
func drain(in io.Reader) {
	if in == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, io.LimitReader(in, 1<<20))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
	}
}

// hookEvent handles one event from a host. It does not look at Weiyun (not even
// the saved login): that is the worker's push, which ends at once when every
// account is out of quota.
func (a *App) hookEvent(event string) {
	if _, err := device.At(a.Cfg.StateDir()).Load(); err != nil {
		return // this computer is not set up (or was reset): nothing to hand over
	}
	s := a.loadHookState()
	s.Event, s.At, s.Count = event, a.Now(), s.Count+1
	a.saveHookState(s)
	// The marker goes first: a worker that is finishing looks for it after it
	// lets go of the lock, so an event is never lost between the two.
	if err := a.markHookDirty(); err != nil {
		a.hookLog("%v", err)
		return
	}
	if a.hookWorkerRunning() {
		return // it sees the new time when it looks again
	}
	if err := hookSpawn(a); err != nil {
		a.hookLog("cannot start the worker: %v", err)
	}
}

// hookWorker uploads for the hooks. At most one worker runs; it waits for a
// quiet moment, uploads, and goes round again if a reply came in meanwhile.
func (a *App) hookWorker(ctx context.Context) {
	for round := 0; round < hookMaxRounds; round++ {
		f, err := platform.Touch(a.hookLockFile())
		if err != nil {
			a.hookLog("%v", err)
			return
		}
		if err := platform.LockExclusiveNonBlocking(f); err != nil {
			f.Close()
			return // another worker is on it
		}
		a.runHookPush(ctx)
		platform.Unlock(f)
		f.Close()
		// after letting go: an event that came while the lock was still held did
		// not start a worker, and is picked up here
		if _, dirty := a.hookDirtySince(); !dirty || ctx.Err() != nil {
			return
		}
	}
}

// waitForQuiet returns when the upload is due: hookQuiet after the last reply,
// or hookMaxWait after the first one that is waiting, whichever comes first.
// It returns false when nothing is waiting or the worker is told to stop.
func (a *App) waitForQuiet(ctx context.Context) bool {
	for {
		since, dirty := a.hookDirtySince()
		if !dirty {
			return false
		}
		deadline := a.loadHookState().At.Add(hookQuiet)
		if cap := since.Add(hookMaxWait); cap.Before(deadline) {
			deadline = cap
		}
		wait := deadline.Sub(a.Now())
		if wait <= 0 {
			return true
		}
		if wait > hookPoll {
			wait = hookPoll
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false
		}
	}
}

// runHookPush waits for the quiet moment, then uploads once.
func (a *App) runHookPush(ctx context.Context) {
	if !a.waitForQuiet(ctx) {
		return
	}
	os.Remove(a.hookDirtyFile())
	if out, err := selfCommand(ctx, "relay", "--quiet", "--push-only"); err != nil || out != "" {
		if err != nil {
			a.hookLog("relay failed: %v", err)
		}
		if out != "" {
			if f, ferr := os.OpenFile(filepath.Join(a.Cfg.StateDir(), "autosync.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); ferr == nil {
				fmt.Fprintln(f, out)
				f.Close()
			}
		}
	}
}

// hooksCover says whether the background job should leave what is pending to
// the hooks this round: every pending item is a Codex revision, a hook has run
// since about the time the oldest was written (so hooks are working here), and
// the oldest has not yet waited hookSafetyAge. Anything else (another agent, a
// receipt, an interrupted publish, no sign of hooks, an old item) is uploaded
// now, as without hooks.
func hooksCover(p syncpkg.Pending, lastHook, now time.Time) bool {
	if p.Journal || p.Outbox > 0 || len(p.Revisions) == 0 || lastHook.IsZero() {
		return false
	}
	var oldest time.Time
	for _, r := range p.Revisions {
		if r.AgentProfile.Agent != codex.AgentName {
			return false
		}
		t, err := session.ParseTime(r.CreatedAt)
		if err != nil {
			return false
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return now.Sub(oldest) < hookSafetyAge && !lastHook.Before(oldest.Add(-hookLead))
}

func newHookCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "hook", Short: "Run by Codex after a reply (installed by `hooks install`); starts an upload of local progress in the background", Args: cobra.NoArgs, Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// whatever happens, the host that ran this must see success and no output
			defer func() { recover() }()
			if worker, _ := cmd.Flags().GetBool("worker"); worker {
				ctx, cancel := context.WithTimeout(cmd.Context(), 40*time.Minute)
				defer cancel()
				a.hookWorker(ctx)
				return nil
			}
			event, _ := cmd.Flags().GetString("event")
			drain(a.In)
			a.hookEvent(event)
			return nil
		}}
	c.Flags().String("event", "", "the host event that fired")
	c.Flags().Bool("worker", false, "(internal) run the relays that hooks asked for")
	return c
}

// codexHomesToHook lists the Codex homes hooks go into: the ones that exist.
func (a *App) codexHomesToHook() []string {
	var out []string
	for _, h := range a.Cfg.CodexHomes() {
		if st, err := os.Stat(h); err == nil && st.IsDir() {
			out = append(out, h)
		}
	}
	return out
}

func (a *App) hookCommands() (map[string]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	cmds := map[string]string{}
	for _, ev := range codex.HookEvents {
		cmds[ev] = codex.HookCommand(exe, os.Getenv("SM_HOME"), ev)
	}
	return cmds, nil
}

// homesWithOurHooks lists the Codex homes whose hooks file holds a
// session-memory handler.
func (a *App) homesWithOurHooks() []string {
	var out []string
	for _, h := range a.codexHomesToHook() {
		if st, err := codex.ReadHooks(h, nil); err == nil && len(st.Installed) > 0 {
			out = append(out, h)
		}
	}
	return out
}

func hookHost(cmd *cobra.Command) error {
	host, _ := cmd.Flags().GetString("host")
	if host != "codex" {
		return errcode.New(errcode.Unsupported, "hooks are only available for Codex (--host codex)")
	}
	return nil
}

type hookHomeStatus struct {
	Home      string            `json:"home"`
	File      string            `json:"file"`
	Installed []string          `json:"installed"`
	Missing   []string          `json:"missing,omitempty"`
	Outdated  []string          `json:"outdated,omitempty"`
	Trust     map[string]string `json:"trust,omitempty"`
	Problems  []string          `json:"problems,omitempty"`
}

// featureOff says whether config.toml of a Codex home switches hooks off.
func featureOff(home string) bool {
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return false
	}
	inFeatures := false
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "[") {
			inFeatures = l == "[features]"
			continue
		}
		if inFeatures {
			l = strings.ReplaceAll(l, " ", "")
			if strings.HasPrefix(l, "hooks=false") || strings.HasPrefix(l, "codex_hooks=false") {
				return true
			}
		}
	}
	return false
}

func (a *App) hookStatus(ctx context.Context, home string, cmds map[string]string, withTrust bool) (hookHomeStatus, error) {
	hs := hookHomeStatus{Home: home, File: codex.HooksPath(home), Installed: []string{}}
	st, err := codex.ReadHooks(home, cmds)
	if err != nil {
		hs.Problems = append(hs.Problems, err.Error())
		return hs, nil
	}
	for _, ev := range codex.HookEvents {
		switch {
		case !st.Installed[ev]:
			hs.Missing = append(hs.Missing, ev)
		case !st.Current[ev]:
			hs.Installed = append(hs.Installed, ev)
			hs.Outdated = append(hs.Outdated, ev)
		default:
			hs.Installed = append(hs.Installed, ev)
		}
	}
	if featureOff(home) {
		hs.Problems = append(hs.Problems, "hooks are switched off in "+filepath.Join(home, "config.toml")+" ([features] hooks = false); Codex will not run them until that is changed")
	}
	if withTrust && len(hs.Installed) > 0 {
		hs.Trust = map[string]string{}
		list, err := codex.SessionMemoryHookTrust(ctx, home, os.Getenv("SM_CODEX_BIN"))
		if err != nil {
			hs.Problems = append(hs.Problems, "could not ask Codex whether the hooks are trusted: "+err.Error())
		}
		for _, h := range list {
			for _, ev := range codex.HookEvents {
				if strings.EqualFold(h.Event, ev) {
					status := h.Trust
					if !h.Enabled {
						status += ", disabled"
					}
					hs.Trust[ev] = status
				}
			}
		}
	}
	return hs, nil
}

func untrusted(hs hookHomeStatus) bool {
	for _, t := range hs.Trust {
		if t != "trusted" {
			return true
		}
	}
	return false
}

func newHooksCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "hooks", Short: "Optional: upload right after each Codex reply (instead of waiting for the next background round)"}
	hostFlag := func(cmd *cobra.Command) { cmd.Flags().String("host", "codex", "which agent's hooks (only codex)") }

	install := &cobra.Command{Use: "install", Short: "Add the session-memory hooks to Codex (your own hooks stay as they are)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := hookHost(cmd); err != nil {
				return err
			}
			cmds, err := a.hookCommands()
			if err != nil {
				return err
			}
			homes := a.codexHomesToHook()
			if len(homes) == 0 {
				return errcode.New(errcode.NotFound, "no Codex home found on this computer; open Codex once, then run this again")
			}
			var failed []string
			for _, h := range homes {
				changed, err := codex.InstallHooks(h, cmds)
				if err != nil {
					fmt.Fprintf(a.Err, "%s: %v\n", h, err)
					failed = append(failed, h)
					continue
				}
				if changed {
					fmt.Fprintf(a.Out, "hooks added to %s\n", codex.HooksPath(h))
				} else {
					fmt.Fprintf(a.Out, "hooks already in place in %s\n", codex.HooksPath(h))
				}
			}
			if len(failed) == len(homes) {
				return errcode.New(errcode.Internal, "no hooks were added")
			}
			fmt.Fprintln(a.Out, "Codex runs a new hook only after you trust it: in Codex type /hooks and trust the session-memory entry, once. Until then nothing changes: syncing keeps running in the background as before.")
			return nil
		}}
	hostFlag(install)

	uninstall := &cobra.Command{Use: "uninstall", Short: "Take the session-memory hooks out of Codex (your own hooks stay)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := hookHost(cmd); err != nil {
				return err
			}
			total := 0
			for _, h := range a.codexHomesToHook() {
				n, err := codex.RemoveHooks(h)
				if err != nil {
					return err
				}
				total += n
			}
			fmt.Fprintf(a.Out, "removed %d session-memory hook(s) from Codex\n", total)
			return nil
		}}
	hostFlag(uninstall)

	status := &cobra.Command{Use: "status", Short: "Show whether the hooks are in place, trusted by Codex, and when one last fired", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := hookHost(cmd); err != nil {
				return err
			}
			cmds, err := a.hookCommands()
			if err != nil {
				return err
			}
			var homes []hookHomeStatus
			for _, h := range a.codexHomesToHook() {
				hs, err := a.hookStatus(cmd.Context(), h, cmds, true)
				if err != nil {
					return err
				}
				homes = append(homes, hs)
			}
			last := a.loadHookState()
			if a.JSON {
				doc := map[string]any{"codex": homes}
				if since, waiting := a.hookDirtySince(); waiting {
					doc["upload_waiting_since"] = since.Format(time.RFC3339)
				}
				if !last.At.IsZero() {
					doc["last_event"] = map[string]any{"event": last.Event, "at": last.At.Format(time.RFC3339), "count": last.Count}
				}
				return a.printJSON(doc)
			}
			if len(homes) == 0 {
				fmt.Fprintln(a.Out, "no Codex home found on this computer")
			}
			for _, hs := range homes {
				switch {
				case len(hs.Installed) == 0:
					fmt.Fprintf(a.Out, "%s: hooks are NOT installed (run `session-memory hooks install`)\n", hs.Home)
				default:
					fmt.Fprintf(a.Out, "%s: installed for %s\n", hs.Home, strings.Join(hs.Installed, ", "))
				}
				if len(hs.Installed) > 0 && len(hs.Missing) > 0 {
					fmt.Fprintf(a.Out, "  missing: %s (run `session-memory hooks install`)\n", strings.Join(hs.Missing, ", "))
				}
				if len(hs.Outdated) > 0 {
					fmt.Fprintf(a.Out, "  points at another copy of the program for %s (run `session-memory hooks install` to update; Codex asks for trust again)\n", strings.Join(hs.Outdated, ", "))
				}
				evs := make([]string, 0, len(hs.Trust))
				for ev := range hs.Trust {
					evs = append(evs, ev)
				}
				sort.Strings(evs)
				for _, ev := range evs {
					fmt.Fprintf(a.Out, "  %s: %s\n", ev, hs.Trust[ev])
				}
				if untrusted(hs) {
					fmt.Fprintln(a.Out, "  Codex does not run it yet: in Codex type /hooks and trust the session-memory entry")
				}
				for _, p := range hs.Problems {
					fmt.Fprintf(a.Out, "  problem: %s\n", p)
				}
			}
			if last.At.IsZero() {
				fmt.Fprintln(a.Out, "no hook has fired on this computer yet")
			} else {
				fmt.Fprintf(a.Out, "last hook: %s at %s (%d in all)\n", last.Event, last.At.Format("2006-01-02 15:04:05"), last.Count)
			}
			if since, waiting := a.hookDirtySince(); waiting {
				fmt.Fprintf(a.Out, "an upload is waiting for a quiet moment (replies since %s)\n", since.Format("15:04:05"))
			}
			return nil
		}}
	hostFlag(status)
	c.AddCommand(install, uninstall, status)
	return c
}
