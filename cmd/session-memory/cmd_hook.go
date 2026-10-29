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
	"session-memory/internal/source/codex"
)

// Codex can run a command when a session starts and when a reply finishes
// (hooks). `hooks install` adds ours; each time Codex runs it, `hook` starts
// one relay in the background and returns at once, so a finished reply is sent
// to the other computer within seconds instead of at the next two-minute
// round. The background job stays: it is what picks up work from the other
// computer and what covers a hook that Codex does not run (hooks need the
// user's trust, given once in Codex's /hooks).

var (
	// hookMinGap is the least time between two relays started by hooks, so a
	// run of quick replies does not hit Weiyun once per reply.
	hookMinGap = 30 * time.Second
	// hookMaxRounds bounds how many relays one worker runs back to back.
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
func (a *App) hookAgainFile() string { return filepath.Join(a.Cfg.StateDir(), "hook.again") }
func (a *App) hookRunFile() string   { return filepath.Join(a.Cfg.StateDir(), "hook-lastrun") }
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

// lastHookRun is when a relay started by a hook last began. It has a file of
// its own: the worker writes it while hooks update the state above.
func (a *App) lastHookRun() time.Time {
	b, err := os.ReadFile(a.hookRunFile())
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return t
}

// noteActivity brings the background rounds back to the normal rhythm: someone
// is working, so the next ones should not wait out an idle back-off.
func (a *App) noteActivity() {
	s := a.loadIdle()
	if s.Quiet == 0 {
		return
	}
	s.Quiet = 0
	b, _ := json.Marshal(s)
	platform.AtomicWrite(a.idleFile(), b, 0600)
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
// the saved login): a relay that finds every account out of quota ends at once.
func (a *App) hookEvent(event string) {
	if _, err := device.At(a.Cfg.StateDir()).Load(); err != nil {
		return // this computer is not set up (or was reset): nothing to hand over
	}
	s := a.loadHookState()
	s.Event, s.At, s.Count = event, a.Now(), s.Count+1
	a.saveHookState(s)
	a.noteActivity()
	// The marker goes first: a worker that is finishing looks for it after it
	// lets go of the lock, so an event is never lost between the two.
	if err := platform.AtomicWrite(a.hookAgainFile(), []byte(a.Now().Format(time.RFC3339)), 0600); err != nil {
		a.hookLog("%v", err)
		return
	}
	if err := hookSpawn(a); err != nil {
		a.hookLog("cannot start the worker: %v", err)
	}
}

// hookWorker runs the relays the hooks asked for. At most one worker runs; an
// event that arrives meanwhile leaves the marker, and the running worker goes
// round again.
func (a *App) hookWorker(ctx context.Context) {
	for round := 0; round < hookMaxRounds; round++ {
		f, err := platform.Touch(a.hookLockFile())
		if err != nil {
			a.hookLog("%v", err)
			return
		}
		if err := platform.LockExclusiveNonBlocking(f); err != nil {
			f.Close()
			return // another worker is on it and will see the marker
		}
		again := a.runHookRelay(ctx)
		platform.Unlock(f)
		f.Close()
		if !again {
			return
		}
	}
}

// runHookRelay waits out the minimum gap, then runs one relay. It returns
// whether a hook fired in the meantime (a marker is waiting).
func (a *App) runHookRelay(ctx context.Context) bool {
	if wait := hookMinGap - a.Now().Sub(a.lastHookRun()); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false
		}
	}
	os.Remove(a.hookAgainFile())
	a.noteActivity()
	platform.AtomicWrite(a.hookRunFile(), []byte(a.Now().Format(time.RFC3339Nano)), 0600)
	if out, err := selfCommand(ctx, "relay", "--quiet"); err != nil || out != "" {
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
	_, err := os.Stat(a.hookAgainFile())
	return err == nil
}

func newHookCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "hook", Short: "Run by Codex at the start of a session and after a reply (installed by `hooks install`); starts a relay in the background", Args: cobra.NoArgs, Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// whatever happens, the host that ran this must see success and no output
			defer func() { recover() }()
			if worker, _ := cmd.Flags().GetBool("worker"); worker {
				ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
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
	c := &cobra.Command{Use: "hooks", Short: "Optional: hand work over right after each Codex reply (instead of waiting for the next background round)"}
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
			fmt.Fprintln(a.Out, "Codex runs a new hook only after you trust it: in Codex type /hooks and trust the "+fmt.Sprint(len(codex.HookEvents))+" session-memory entries, once. Until then nothing changes: syncing keeps running in the background as before.")
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
					fmt.Fprintln(a.Out, "  Codex does not run them yet: in Codex type /hooks and trust the session-memory entries")
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
			return nil
		}}
	hostFlag(status)
	c.AddCommand(install, uninstall, status)
	return c
}
