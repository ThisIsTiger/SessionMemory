package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"session-memory/internal/platform"
	"session-memory/internal/source/codex"
	"session-memory/internal/vault"
)

// hookWorkers runs the worker a hook starts inside the test, in a goroutine,
// instead of as a process; the test waits for them all before it ends.
func hookWorkers(t *testing.T, gap time.Duration) (events *atomic.Int32, wait func()) {
	t.Helper()
	wg := new(sync.WaitGroup)
	liveWorkers = wg
	events = new(atomic.Int32)
	oldSpawn, oldGap := hookSpawn, hookMinGap
	hookSpawn = func(a *App) error {
		events.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.hookWorker(context.Background())
		}()
		return nil
	}
	hookMinGap = gap
	wait = wg.Wait
	t.Cleanup(func() {
		wg.Wait()
		hookSpawn, hookMinGap = oldSpawn, oldGap
		liveWorkers = nil
	})
	return events, wait
}

// liveWorkers lets a replaced selfCommand be put back only after the workers
// that may still call it are done (cleanups run last-in first).
var liveWorkers *sync.WaitGroup

// relayLog replaces the relay a worker starts and notes every run.
type relayLog struct {
	mu      sync.Mutex
	starts  []time.Time
	running int32
	peak    int32
	work    func(n int) (string, error)
}

func fakeRelay(t *testing.T, work func(n int) (string, error)) *relayLog {
	t.Helper()
	l := &relayLog{work: work}
	old := selfCommand
	t.Cleanup(func() {
		if liveWorkers != nil {
			liveWorkers.Wait()
		}
		selfCommand = old
	})
	selfCommand = func(ctx context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") != "relay --quiet" {
			t.Errorf("a hook ran %v, want relay --quiet", args)
		}
		l.mu.Lock()
		l.starts = append(l.starts, time.Now())
		n := len(l.starts)
		l.mu.Unlock()
		if cur := atomic.AddInt32(&l.running, 1); cur > atomic.LoadInt32(&l.peak) {
			atomic.StoreInt32(&l.peak, cur)
		}
		defer atomic.AddInt32(&l.running, -1)
		if work != nil {
			return work(n)
		}
		return "", nil
	}
	return l
}

func (l *relayLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.starts)
}

func (l *relayLog) at(i int) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts[i]
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readyComputer(t *testing.T) *machine {
	t.Helper()
	m := newComputer(t, t.TempDir(), "a", filepath.Join(t.TempDir(), "cloud"))
	m.must("init")
	return m
}

// A computer that was never set up (or was reset) gets no work, no files and no
// output from a hook: the host that ran it must notice nothing.
func TestHookOnAComputerThatIsNotSetUpDoesNothing(t *testing.T) {
	h := isolate(t)
	events, _ := hookWorkers(t, 0)
	code, out, errs := exec2(t, "hook", "--event", "Stop")
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d out=%q err=%q", code, out, errs)
	}
	if events.Load() != 0 {
		t.Fatal("a worker was started")
	}
	filepath.Walk(h, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.Contains(p, "hook") {
			t.Errorf("a hook file was created: %s", p)
		}
		return nil
	})
}

// exec2 is exec with stdin that never closes, as some hosts leave it.
func exec2(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(args, pr, &out, &errb) }()
	select {
	case code := <-done:
		return code, out.String(), errb.String()
	case <-time.After(5 * time.Second):
		t.Fatal("the command waited for the host's stdin")
		return 0, "", ""
	}
}

func TestHookStartsOneRelayAndRemembersIt(t *testing.T) {
	m := readyComputer(t)
	events, wait := hookWorkers(t, 0)
	rl := fakeRelay(t, nil)

	code, out, errs := m.run("hook", "--event", "Stop")
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d out=%q err=%q", code, out, errs)
	}
	wait()
	if events.Load() != 1 || rl.count() != 1 {
		t.Fatalf("workers %d, relays %d", events.Load(), rl.count())
	}
	var st hookState
	b, err := os.ReadFile(filepath.Join(m.home, "sm", "hook-state.json"))
	if err != nil || json.Unmarshal(b, &st) != nil || st.Event != "Stop" || st.Count != 1 || st.At.IsZero() {
		t.Fatalf("state %+v (%v)", st, err)
	}
	if !exists(filepath.Join(m.home, "sm", "hook-lastrun")) {
		t.Fatal("the time of the relay was not kept")
	}
	if exists(filepath.Join(m.home, "sm", "hook.again")) {
		t.Fatal("the marker stayed after the relay ran")
	}
	m.must("hook", "--event", "SessionStart")
	wait()
	b, _ = os.ReadFile(filepath.Join(m.home, "sm", "hook-state.json"))
	json.Unmarshal(b, &st)
	if st.Event != "SessionStart" || st.Count != 2 || rl.count() != 2 {
		t.Fatalf("second event: %+v, relays %d", st, rl.count())
	}
}

// Someone is working: the background rounds go back to the normal rhythm
// instead of waiting out an idle back-off of up to ten minutes.
func TestHookEndsTheIdleBackoff(t *testing.T) {
	m := readyComputer(t)
	hookWorkers(t, 0)
	fakeRelay(t, nil)
	idle := filepath.Join(m.home, "sm", "autosync-idle.json")
	b, _ := json.Marshal(idleState{LastPoll: time.Now(), Quiet: 6})
	os.WriteFile(idle, b, 0600)
	a := &App{Now: time.Now}
	a.Cfg.Home = filepath.Join(m.home, "sm")
	if !a.idleSkip() {
		t.Fatal("the test needs a back-off in place")
	}
	m.must("hook", "--event", "Stop")
	var got idleState
	raw, _ := os.ReadFile(idle)
	json.Unmarshal(raw, &got)
	if got.Quiet != 0 || a.idleSkip() {
		t.Fatalf("idle state after the hook: %+v, still skipping: %v", got, a.idleSkip())
	}
}

// A reply that finishes while a relay is already running must still be sent:
// the running worker goes round again once it is done.
func TestHookEventDuringARelayIsNotLost(t *testing.T) {
	m := readyComputer(t)
	events, wait := hookWorkers(t, 0)
	release := make(chan struct{})
	rl := fakeRelay(t, func(n int) (string, error) {
		if n == 1 {
			<-release
		}
		return "", nil
	})
	m.must("hook", "--event", "Stop")
	eventually(t, "the first relay to start", func() bool { return rl.count() == 1 })
	m.must("hook", "--event", "Stop") // arrives while the first relay is still running
	if events.Load() != 2 {
		t.Fatalf("workers = %d", events.Load())
	}
	time.Sleep(100 * time.Millisecond) // the second worker found the lock held and left
	if rl.count() != 1 {
		t.Fatalf("a second relay ran at the same time (%d)", rl.count())
	}
	close(release)
	wait()
	if rl.count() != 2 || rl.peak != 1 {
		t.Fatalf("relays = %d, at most %d at once; want 2 and 1", rl.count(), rl.peak)
	}
	if exists(filepath.Join(m.home, "sm", "hook.again")) {
		t.Fatal("the marker stayed")
	}
}

// A burst of replies is a few relays, never one per reply, never two at once,
// and the last reply is always covered by a relay that started after it.
func TestHookBurstIsCoalesced(t *testing.T) {
	m := readyComputer(t)
	_, wait := hookWorkers(t, 0)
	rl := fakeRelay(t, func(int) (string, error) { time.Sleep(60 * time.Millisecond); return "", nil })
	var lastEvent time.Time
	for i := 0; i < 12; i++ {
		lastEvent = time.Now()
		m.must("hook", "--event", "Stop")
	}
	wait()
	n := rl.count()
	if n < 1 || n >= 12 {
		t.Fatalf("%d relays for 12 events", n)
	}
	if rl.peak != 1 {
		t.Fatalf("%d relays ran at once", rl.peak)
	}
	if rl.at(n - 1).Before(lastEvent) {
		t.Fatal("the last reply was not covered by any relay")
	}
}

func TestHookKeepsAGapBetweenRelays(t *testing.T) {
	m := readyComputer(t)
	_, wait := hookWorkers(t, 250*time.Millisecond)
	rl := fakeRelay(t, nil)
	m.must("hook", "--event", "Stop")
	eventually(t, "the first relay", func() bool { return rl.count() == 1 })
	m.must("hook", "--event", "Stop")
	wait()
	if rl.count() != 2 {
		t.Fatalf("relays = %d", rl.count())
	}
	if gap := rl.at(1).Sub(rl.at(0)); gap < 240*time.Millisecond {
		t.Fatalf("the second relay started %v after the first", gap)
	}
}

func TestHookWorkerStopsAfterItsRounds(t *testing.T) {
	m := readyComputer(t)
	_, wait := hookWorkers(t, 0)
	old := hookMaxRounds
	hookMaxRounds = 3
	t.Cleanup(func() { hookMaxRounds = old })
	again := filepath.Join(m.home, "sm", "hook.again")
	rl := fakeRelay(t, func(int) (string, error) { // every relay is followed by another reply
		os.WriteFile(again, []byte("x"), 0600)
		return "", nil
	})
	m.must("hook", "--event", "Stop")
	wait()
	if rl.count() != 3 {
		t.Fatalf("relays = %d, want the limit of 3", rl.count())
	}
}

func TestHookWorkerLeavesWhenAnotherOneIsRunning(t *testing.T) {
	m := readyComputer(t)
	hookWorkers(t, 0)
	rl := fakeRelay(t, nil)
	a := &App{Now: time.Now}
	a.Cfg.Home = filepath.Join(m.home, "sm")
	f, err := platform.Touch(a.hookLockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := platform.LockExclusiveNonBlocking(f); err != nil {
		t.Fatal(err)
	}
	a.hookWorker(context.Background())
	if rl.count() != 0 {
		t.Fatal("a worker ran next to another")
	}
}

func TestHookWorkerKeepsRelayOutputAndFailuresInTheLog(t *testing.T) {
	m := readyComputer(t)
	_, wait := hookWorkers(t, 0)
	fakeRelay(t, func(n int) (string, error) {
		if n == 1 {
			return "[10:00:00] uploaded 2 item(s), received 0 version(s)", nil
		}
		return "", errors.New("exit status 6")
	})
	m.must("hook", "--event", "Stop")
	wait()
	m.must("hook", "--event", "Stop")
	wait()
	log, _ := os.ReadFile(filepath.Join(m.home, "sm", "autosync.log"))
	if !strings.Contains(string(log), "uploaded 2 item(s)") || !strings.Contains(string(log), "hook: relay failed: exit status 6") {
		t.Fatalf("log:\n%s", log)
	}
}

// Whatever goes wrong inside, the host sees success and silence.
func TestHookNeverFailsOrSpeaksToTheHost(t *testing.T) {
	m := readyComputer(t)
	old := hookSpawn
	t.Cleanup(func() { hookSpawn = old })
	hookSpawn = func(*App) error { return errors.New("no process for you") }
	code, out, errs := m.run("hook", "--event", "Stop")
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d out=%q err=%q", code, out, errs)
	}
	if log, _ := os.ReadFile(filepath.Join(m.home, "sm", "autosync.log")); !strings.Contains(string(log), "cannot start the worker: no process for you") {
		t.Fatalf("log: %s", log)
	}
	hookSpawn = func(*App) error { panic("boom") }
	if code, out, errs := m.run("hook", "--event", "Stop"); code != 0 || out != "" || errs != "" {
		t.Fatalf("after a panic: exit %d out=%q err=%q", code, out, errs)
	}
	// the host writes its JSON and keeps the pipe open: no waiting for it
	hookSpawn = func(*App) error { return nil }
	t.Setenv("SM_HOME", filepath.Join(m.home, "sm"))
	t.Setenv("HOME", m.home)
	oldKeys := newKeyStore
	newKeyStore = func() vault.KeyStore { return m.keys }
	defer func() { newKeyStore = oldKeys }()
	pr, pw := io.Pipe()
	defer pw.Close()
	go pw.Write([]byte(`{"hook_event_name":"Stop","session_id":"x"}` + "\n"))
	start := time.Now()
	done := make(chan int, 1)
	var o, e bytes.Buffer
	go func() { done <- run([]string{"hook", "--event", "Stop"}, pr, &o, &e) }()
	select {
	case code := <-done:
		if code != 0 || o.Len() != 0 || e.Len() != 0 || time.Since(start) > 2*time.Second {
			t.Fatalf("exit %d out=%q err=%q after %v", code, o.String(), e.String(), time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook waited for the host's stdin")
	}
}

// The path a host really takes: the hook starts a worker as its own process,
// which outlives the hook and runs the relay. (This test binary stands in for
// the program, with an empty credential store, so the relay fails: what is
// checked is that the chain ran and was recorded, not what the relay did.)
func TestHookStartsARealDetachedWorker(t *testing.T) {
	m := readyComputer(t)
	t.Setenv("SM_TEST_AS_CLI", "1")
	oldSpawn, oldGap := hookSpawn, hookMinGap
	t.Cleanup(func() { hookSpawn, hookMinGap = oldSpawn, oldGap })
	hookMinGap = 0
	hookSpawn = func(a *App) error { // the real one
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
	start := time.Now()
	if code, out, errs := m.run("hook", "--event", "Stop"); code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d out=%q err=%q", code, out, errs)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the hook took %v", time.Since(start))
	}
	eventually(t, "the worker to run a relay", func() bool {
		return exists(filepath.Join(m.home, "sm", "hook-lastrun"))
	})
	eventually(t, "the failed relay to be logged", func() bool {
		b, _ := os.ReadFile(filepath.Join(m.home, "sm", "autosync.log"))
		return strings.Contains(string(b), "hook: relay failed")
	})
	eventually(t, "the worker to let go of its lock", func() bool {
		f, err := platform.Touch(filepath.Join(m.home, "sm", "locks", "hook.lock"))
		if err != nil {
			return false
		}
		defer f.Close()
		if platform.LockExclusiveNonBlocking(f) != nil {
			return false
		}
		platform.Unlock(f)
		return true
	})
}

// ---- hooks install / status / uninstall ----

// fakeCodexBin answers hooks/list the way the real app-server does.
func fakeCodexBin(t *testing.T, status string) string {
	t.Helper()
	// the commands are read from the hooks file the test installed
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"id":1,"result":{"userAgent":"x","codexHome":"/h"}}' ;;
    *'"method":"hooks/list"'*)
      cmds=$(grep -o '"command": *"[^"]*"' "$CODEX_HOME/hooks.json" | sed 's/"command": *//')
      out=$(printf '%s\n' "$cmds" | while IFS= read -r c; do
        ev=stop; if printf '%s' "$c" | grep -q SessionStart; then ev=sessionStart; fi
        printf '{"eventName":"%s","command":%s,"trustStatus":"` + status + `","enabled":true},' "$ev" "$c"
      done)
      echo "{\"id\":2,\"result\":{\"data\":[{\"cwd\":\"/h\",\"hooks\":[${out%,}],\"warnings\":[],\"errors\":[]}]}}" ;;
  esac
done
`
	p := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHooksInstallStatusUninstall(t *testing.T) {
	m := readyComputer(t)
	cx := filepath.Join(m.home, "codex")
	path := codex.HooksPath(cx)

	out := m.must("hooks", "status")
	if !strings.Contains(out, "NOT installed") || !strings.Contains(out, "no hook has fired") {
		t.Fatalf("status before install:\n%s", out)
	}

	out = m.must("hooks", "install")
	if !strings.Contains(out, "hooks added to "+path) || !strings.Contains(out, "/hooks") {
		t.Fatalf("install output:\n%s", out)
	}
	raw, _ := os.ReadFile(path)
	for _, ev := range codex.HookEvents {
		if !strings.Contains(string(raw), "hook --event "+ev) {
			t.Errorf("%s is not in the file:\n%s", ev, raw)
		}
	}
	if out := m.must("hooks", "install"); !strings.Contains(out, "already in place") {
		t.Fatalf("second install:\n%s", out)
	}

	// Codex has not been told to trust them yet
	t.Setenv("SM_CODEX_BIN", fakeCodexBin(t, "untrusted"))
	out = m.must("hooks", "status")
	if !strings.Contains(out, "installed for SessionStart, Stop") || !strings.Contains(out, "untrusted") || !strings.Contains(out, "type /hooks") {
		t.Fatalf("status with untrusted hooks:\n%s", out)
	}
	t.Setenv("SM_CODEX_BIN", fakeCodexBin(t, "trusted"))
	out = m.must("hooks", "status")
	if strings.Contains(out, "type /hooks") || !strings.Contains(out, "Stop: trusted") {
		t.Fatalf("status with trusted hooks:\n%s", out)
	}

	// a hook that fired is shown
	hookWorkers(t, 0)
	fakeRelay(t, nil)
	m.must("hook", "--event", "Stop")
	if out := m.must("hooks", "status"); !strings.Contains(out, "last hook: Stop at ") {
		t.Fatalf("status after a hook:\n%s", out)
	}
	var doc struct {
		Codex []struct {
			Home      string            `json:"home"`
			Installed []string          `json:"installed"`
			Trust     map[string]string `json:"trust"`
		} `json:"codex"`
		Last map[string]any `json:"last_event"`
	}
	if err := json.Unmarshal([]byte(m.must("--json", "hooks", "status")), &doc); err != nil || len(doc.Codex) != 1 ||
		len(doc.Codex[0].Installed) != 2 || doc.Codex[0].Trust["Stop"] != "trusted" || doc.Last["event"] != "Stop" {
		t.Fatalf("json status: %+v (%v)", doc, err)
	}

	if out := m.must("hooks", "uninstall"); !strings.Contains(out, "removed 2 session-memory hook(s)") {
		t.Fatalf("uninstall:\n%s", out)
	}
	if exists(path) {
		t.Fatal("hooks.json with nothing else in it was left behind")
	}
	if out := m.must("hooks", "status"); !strings.Contains(out, "NOT installed") {
		t.Fatalf("status after uninstall:\n%s", out)
	}
}

func TestHooksStatusSaysWhenItCannotAskCodexOrHooksAreOff(t *testing.T) {
	m := readyComputer(t)
	cx := filepath.Join(m.home, "codex")
	m.must("hooks", "install") // SM_CODEX_BIN points nowhere in tests
	out := m.must("hooks", "status")
	if !strings.Contains(out, "problem: could not ask Codex whether the hooks are trusted") {
		t.Fatalf("status:\n%s", out)
	}
	os.WriteFile(filepath.Join(cx, "config.toml"), []byte("model_provider = \"x\"\n[features]\nhooks = false\n"), 0600)
	if out := m.must("hooks", "status"); !strings.Contains(out, "switched off") {
		t.Fatalf("status with hooks switched off:\n%s", out)
	}
	os.WriteFile(filepath.Join(cx, "config.toml"), []byte("[other]\nhooks = false\n[features]\njs_repl = false\n"), 0600)
	if out := m.must("hooks", "status"); strings.Contains(out, "switched off") {
		t.Fatalf("an unrelated hooks = false counted:\n%s", out)
	}
}

func TestHooksRefuseWhatTheyCannotDo(t *testing.T) {
	m := readyComputer(t)
	if code, _, errs := m.run("hooks", "install", "--host", "workbuddy"); code == 0 || !strings.Contains(errs, "only available for Codex") {
		t.Fatalf("workbuddy: exit %d %s", code, errs)
	}
	// a hooks file that is not valid is not overwritten
	path := codex.HooksPath(filepath.Join(m.home, "codex"))
	os.WriteFile(path, []byte("{ nope"), 0600)
	code, _, errs := m.run("hooks", "install")
	if code == 0 || !strings.Contains(errs, "not touched") {
		t.Fatalf("invalid file: exit %d %s", code, errs)
	}
	if b, _ := os.ReadFile(path); string(b) != "{ nope" {
		t.Fatalf("the file was changed: %q", b)
	}
	if out := m.must("hooks", "status"); !strings.Contains(out, "problem:") {
		t.Fatalf("status of an invalid file:\n%s", out)
	}
	os.Remove(path)
	// no Codex on this computer
	os.RemoveAll(filepath.Join(m.home, "codex"))
	if code, _, errs := m.run("hooks", "install"); code == 0 || !strings.Contains(errs, "no Codex home") {
		t.Fatalf("no Codex: exit %d %s", code, errs)
	}
}

// reset puts the computer back to "never set up", and that includes the hooks
// session-memory added; the user's own stay.
func TestResetTakesTheHooksOut(t *testing.T) {
	m := readyComputer(t)
	cx := filepath.Join(m.home, "codex")
	path := codex.HooksPath(cx)
	mine := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/mine.sh"}]}]}}`
	os.WriteFile(path, []byte(mine), 0600)
	m.must("hooks", "install")
	if out := m.must("reset"); !strings.Contains(out, "take the session-memory hooks out of "+path) || !strings.Contains(string(mustRead(t, path)), "hook --event") {
		t.Fatalf("the plan (or the file) changed before --yes:\n%s", out)
	}
	out := m.must("reset", "--yes")
	if !strings.Contains(out, "session-memory hooks removed from "+path) {
		t.Fatalf("reset:\n%s", out)
	}
	var doc, want map[string]any
	json.Unmarshal(mustRead(t, path), &doc)
	json.Unmarshal([]byte(mine), &want)
	if !jsonEqual(doc, want) {
		t.Fatalf("hooks.json after reset: %v", doc)
	}
	// a hook that fires after that finds nothing set up
	events, _ := hookWorkers(t, 0)
	if code, o, e := m.run("hook", "--event", "Stop"); code != 0 || o != "" || e != "" || events.Load() != 0 {
		t.Fatalf("hook after reset: exit %d out=%q err=%q workers %d", code, o, e, events.Load())
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// The whole path with real relays: a reply on A is not on B until something
// sends it, and the hook is what sends it, with no command typed on A.
func TestHookHandsAReplyOverWithoutWaitingForTheBackgroundRound(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := newComputer(t, root, "a", shared)
	B := newComputer(t, root, "b", shared)
	app := filepath.Join(A.home, "work", "app")
	os.MkdirAll(app, 0700)
	os.MkdirAll(filepath.Join(B.home, "work", "app"), 0700)
	addCodexSession(A, 21, app)
	selfVia(t, A, nil)
	A.must("setup", "--no-autosync", "--days", "36500")
	selfVia(t, B, nil)
	B.must("setup", "--no-autosync", "--client-closed", "--days", "36500")
	bFile := func() string {
		files, _ := filepath.Glob(filepath.Join(B.home, "codex", "sessions", "*", "*", "*", "*.jsonl"))
		if len(files) != 1 {
			t.Fatalf("B has %v", files)
		}
		return string(mustRead(t, files[0]))
	}
	if strings.Contains(bFile(), "a reply for the hook") {
		t.Fatal("the reply is on B before it was written")
	}

	files, _ := filepath.Glob(filepath.Join(A.home, "codex", "sessions", "*", "*", "*", "*.jsonl"))
	f, _ := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"a reply for the hook"}}` + "\n")
	f.Close()

	selfVia(t, B, nil)
	B.must("relay", "--client-closed")
	if strings.Contains(bFile(), "a reply for the hook") {
		t.Fatal("B has the reply although nothing on A sent it")
	}

	// The worker runs once the hook command has returned (as it does in life,
	// where it is another process): these in-process machines swap a global
	// credential store while a command runs, so two commands cannot overlap.
	hookWorkers(t, 0)
	var started []*App
	hookSpawn = func(a *App) error { started = append(started, a); return nil }
	selfVia(t, A, nil) // the relay the worker starts runs on A
	if out := A.must("hook", "--event", "Stop"); out != "" {
		t.Fatalf("the hook printed %q", out)
	}
	if len(started) != 1 {
		t.Fatalf("the hook started %d workers", len(started))
	}
	started[0].hookWorker(context.Background())

	selfVia(t, B, nil)
	B.must("relay", "--client-closed")
	if !strings.Contains(bFile(), "a reply for the hook") {
		t.Fatal("B did not get the reply after the hook on A")
	}
}
