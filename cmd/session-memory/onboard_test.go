package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"session-memory/internal/device"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
)

// selfVia makes the program's own commands (what setup runs one after the
// other) run on machine m in this process. "autosync" is never really run: it
// is only recorded, so a test cannot touch launchd. hook may answer a command
// itself. The returned slice is every command run so far.
func selfVia(t *testing.T, m *machine, hook func(args []string) (string, error, bool)) *[]string {
	t.Helper()
	old := selfCommand
	t.Cleanup(func() { selfCommand = old })
	calls := &[]string{}
	selfCommand = func(ctx context.Context, args ...string) (string, error) {
		*calls = append(*calls, strings.Join(args, " "))
		if hook != nil {
			if out, err, ok := hook(args); ok {
				return out, err
			}
		}
		if args[0] == "autosync" {
			return "enabled (fake)", nil
		}
		code, out, errs := m.run(args...)
		if code != 0 {
			return strings.TrimSpace(out + errs), errors.New("exit")
		}
		return strings.TrimSpace(out), nil
	}
	return calls
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// a computer with Codex at <home>/codex, sharing the cloud folder
func newComputer(t *testing.T, root, name, shared string) *machine {
	m := &machine{t: t, home: filepath.Join(root, name), keys: vault.MemStore{}}
	cx := filepath.Join(m.home, "codex")
	os.MkdirAll(cx, 0700)
	os.WriteFile(filepath.Join(cx, "config.toml"), []byte("model_provider = \"custom\"\n"), 0600)
	m.must("config", "set", "transport.folder", shared)
	m.must("config", "set", "agents.codex.home", cx)
	return m
}

// a Codex session of m recorded in cwd
func addCodexSession(m *machine, seed int64, cwd string) {
	tid := codexgen.ThreadIDFor(seed)
	p := filepath.Join(m.home, "codex", "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: seed, Turns: 2, Cwd: cwd}), 0600)
}

func projectLines(m *machine) int {
	out := strings.TrimSpace(m.must("project", "list"))
	if out == "" {
		return 0
	}
	return strings.Count(out, "\n") + 1
}

func sessionLines(m *machine) int {
	out := strings.TrimSpace(m.must("list"))
	if out == "" {
		return 0
	}
	return strings.Count(out, "\n") + 1
}

// One command per computer: the first creates the vault and covers the local
// sessions, the second joins by itself and receives them.
func TestSetupConnectsTwoComputers(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := newComputer(t, root, "a", shared)
	B := newComputer(t, root, "b", shared)
	app := filepath.Join(A.home, "work", "app")
	os.MkdirAll(app, 0700)
	addCodexSession(A, 21, app)
	addCodexSession(A, 22, A.home)                           // a project at the home folder would claim everything below it
	addCodexSession(A, 23, filepath.Join(A.home, "deleted")) // its folder no longer exists

	selfVia(t, A, nil)
	out := A.must("setup", "--no-autosync", "--days", "36500")
	if !strings.Contains(out, "1 covered, 2 skipped (no project folder)") {
		t.Fatalf("A setup:\n%s", out)
	}
	if list := A.must("list"); strings.Count(strings.TrimSpace(list), "\n") != 0 || !strings.Contains(list, "codex") {
		t.Fatalf("A tracks the wrong sessions:\n%s", list)
	}
	// running it again changes nothing and does not fail
	A.must("setup", "--no-autosync", "--days", "36500")

	os.MkdirAll(filepath.Join(B.home, "work", "app"), 0700) // the one folder of that name: found by itself
	selfVia(t, B, nil)
	out = B.must("setup", "--no-autosync", "--client-closed", "--days", "36500")
	if !strings.Contains(out, "joined the vault") {
		t.Fatalf("B did not join by itself:\n%s", out)
	}
	if files, _ := filepath.Glob(filepath.Join(B.home, "codex", "sessions", "*", "*", "*", "*.jsonl")); len(files) != 1 {
		t.Fatalf("B received %v\n%s", files, out)
	}
}

// After setup a sync keeps following new project folders; a subfolder of a
// project that is already mapped makes no new project; home and gone folders
// are left out. Without setup's marker a sync does what it always did.
func TestSyncFollowsNewFoldersAfterSetup(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := newComputer(t, root, "a", shared)
	B := newComputer(t, root, "b", shared)
	app := filepath.Join(A.home, "work", "app")
	os.MkdirAll(app, 0700)
	addCodexSession(A, 21, app)
	selfVia(t, A, nil)
	A.must("setup", "--no-autosync", "--days", "36500")
	if n := projectLines(A); n != 1 {
		t.Fatalf("after setup: %d projects", n)
	}

	fresh := filepath.Join(A.home, "work", "fresh")
	os.MkdirAll(filepath.Join(app, "sub"), 0700)
	os.MkdirAll(fresh, 0700)
	addCodexSession(A, 31, fresh)
	addCodexSession(A, 32, filepath.Join(app, "sub"))
	addCodexSession(A, 33, A.home)
	addCodexSession(A, 34, filepath.Join(A.home, "gone"))
	A.must("relay", "--client-closed")
	if n := projectLines(A); n != 2 {
		t.Fatalf("a new folder is one new project, a subfolder none: %d projects\n%s", n, A.must("project", "list"))
	}
	if n := sessionLines(A); n != 3 {
		t.Fatalf("tracked %d sessions, want app, its subfolder and the new folder\n%s", n, A.must("list"))
	}

	os.MkdirAll(filepath.Join(B.home, "work", "app"), 0700)
	os.MkdirAll(filepath.Join(B.home, "work", "fresh"), 0700)
	selfVia(t, B, nil)
	B.must("setup", "--no-autosync", "--client-closed", "--days", "36500")
	if files, _ := filepath.Glob(filepath.Join(B.home, "codex", "sessions", "*", "*", "*", "*.jsonl")); len(files) != 3 {
		t.Fatalf("B received %v", files)
	}

	// no marker, no new projects: sync is what it was
	os.Remove(A.keyMarker(markTrackNew))
	other := filepath.Join(A.home, "work", "other")
	os.MkdirAll(other, 0700)
	addCodexSession(A, 35, other)
	A.must("sync")
	if n := projectLines(A); n != 2 {
		t.Fatalf("sync without setup's marker made a project: %d", n)
	}
}

// the marker path of a machine (its state directory is SM_HOME)
func (m *machine) keyMarker(name string) string { return filepath.Join(m.home, "sm", name) }

func TestSetupRepairsTheCloudKeyOnlyWhenAsked(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := newComputer(t, root, "a", shared)
	os.MkdirAll(filepath.Join(A.home, "work", "app"), 0700)
	addCodexSession(A, 21, filepath.Join(A.home, "work", "app"))
	selfVia(t, A, nil)
	A.must("setup", "--no-autosync", "--days", "36500")
	key := filepath.Join(shared, "public", "vault-key.json")
	if !exists(key) {
		t.Fatal("setup did not put the key in the cloud folder")
	}
	touch(A.keyMarker(markKeyPending))
	os.Remove(key)
	A.must("setup", "--no-autosync", "--days", "36500")
	if !exists(key) || exists(A.keyMarker(markKeyPending)) {
		t.Fatalf("the key was not put back (key %v, marker %v)", exists(key), exists(A.keyMarker(markKeyPending)))
	}
	// the user removed it on purpose: it stays removed
	touch(A.keyMarker(markKeyPending))
	touch(A.keyMarker(markKeyRemoved))
	os.Remove(key)
	A.must("setup", "--no-autosync", "--days", "36500")
	A.must("relay", "--client-closed")
	if exists(key) {
		t.Fatal("a key removed on purpose was put back")
	}
	// without any marker nothing is republished
	os.Remove(A.keyMarker(markKeyRemoved))
	A.must("setup", "--no-autosync", "--days", "36500")
	if exists(key) {
		t.Fatal("the key was republished without a pending marker")
	}
}

// The last line says how this computer got connected.
func TestSetupLastLineSaysWhatHappened(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := newComputer(t, root, "a", shared)
	B := newComputer(t, root, "b", shared)
	C := newComputer(t, root, "c", filepath.Join(root, "other-cloud"))
	for _, m := range []*machine{A, B, C} {
		os.MkdirAll(filepath.Join(m.home, "work", "app"), 0700)
	}
	addCodexSession(A, 21, filepath.Join(A.home, "work", "app"))
	setup := func(m *machine) string {
		selfVia(t, m, nil)
		return lastLine(m.must("setup", "--no-autosync", "--days", "36500"))
	}
	for _, c := range []struct {
		m    *machine
		want string
	}{{A, "done: FIRST_COMPUTER "}, {A, "done: ALREADY_CONNECTED "}, {B, "done: JOINED "}, {C, "done: FIRST_COMPUTER "}} {
		if got := setup(c.m); !strings.HasPrefix(got, c.want) {
			t.Fatalf("last line %q, want %q", got, c.want)
		}
	}
	if got := setup(C); !strings.Contains(got, "ALREADY_CONNECTED") {
		t.Fatalf("again: %q", got)
	}
	if first := setup(newComputer(t, root, "d", filepath.Join(root, "yet-another"))); !strings.Contains(first, "different Weiyun account") {
		t.Fatalf("FIRST_COMPUTER must say what to do when another computer was set up: %q", first)
	}
}

// A relay that cannot finish now (quota, no network) is not a failed setup:
// the background sync finishes it. Sessions that need a decision are reported
// apart, because waiting never fixes them. Anything else is a failure.
func TestSetupWhenTheFirstUploadCannotFinish(t *testing.T) {
	for name, c := range map[string]struct {
		relayOut string
		pending  bool
		action   string // the NEEDS_ACTION line must start with this
	}{
		"quota":       {"error: internal: weiyun: weiyun.list: weiyun daily quota used up (type:business, code:117401, msg:rate limit exceeded)", true, ""},
		"recovery":    {"project app: updated 0, up to date 1, 2 need attention\nerror: recovery_needed: 2 session(s) need attention (conflict or divergence); see the messages above", false, "NEEDS_ACTION: 2 sessions did not sync"},
		"interrupted": {"error: recovery_needed: files changed after interrupted install; manual recovery required", false, "NEEDS_ACTION: some sessions did not sync"},
		"network":     {"error: internal: Post \"https://x/mcp\": dial tcp: lookup x: no such host", true, ""},
		"other":       {"error: internal: the disk exploded", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			A := newComputer(t, root, "a", filepath.Join(root, "cloud"))
			os.MkdirAll(filepath.Join(A.home, "work", "app"), 0700)
			addCodexSession(A, 21, filepath.Join(A.home, "work", "app"))
			calls := selfVia(t, A, func(args []string) (string, error, bool) {
				if args[0] == "relay" {
					return c.relayOut, errors.New("exit"), true
				}
				return "", nil, false
			})
			code, out, errs := A.run("setup", "--days", "36500")
			if c.action != "" {
				// connected, but the blocked sessions are not promised to fix themselves
				if code != 0 || !strings.Contains(out, c.action) || strings.Contains(out, "PENDING") || strings.Contains(out, "finish by itself") ||
					!strings.HasPrefix(lastLine(out), "done: FIRST_COMPUTER") {
					t.Fatalf("exit %d\n%s\n%s", code, out, errs)
				}
			} else if c.pending {
				if code != 0 || !strings.Contains(out, "PENDING: connected; the first upload will finish by itself") || !strings.HasPrefix(lastLine(out), "done: FIRST_COMPUTER") {
					t.Fatalf("exit %d\n%s\n%s", code, out, errs)
				}
			} else if code == 0 || strings.Contains(out, "PENDING") {
				t.Fatalf("a real failure must stay one: exit %d\n%s\n%s", code, out, errs)
			}
			if runtime.GOOS == "darwin" && !strings.Contains(strings.Join(*calls, "\n"), "autosync enable") {
				t.Fatalf("background sync was not started before the relay: %v", *calls)
			}
		})
	}
}

// Nothing is created in the cloud for a computer that has no agent to sync.
func TestSetupWithoutAnAgentCreatesNothing(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	m := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(m.home, 0700)
	m.must("config", "set", "transport.folder", shared)
	calls := selfVia(t, m, nil)
	code, _, errs := m.run("setup", "--no-autosync")
	if code == 0 || !strings.Contains(errs, "no Codex or WorkBuddy") {
		t.Fatalf("%d %s", code, errs)
	}
	if _, err := device.At(filepath.Join(m.home, "sm")).Load(); err == nil {
		t.Fatal("a device was created")
	}
	if entries, _ := os.ReadDir(shared); len(entries) != 0 || len(*calls) != 0 {
		t.Fatalf("something was created: %v %v", entries, *calls)
	}
}

// A minimal Weiyun: the vault folder with "public" and "manifests"; reading
// "manifests" fails, as a flaky connection would. Calls are counted.
type fakeCloud struct {
	calls   atomic.Int64
	failing bool
}

func newFakeCloud(t *testing.T, failing bool) (*fakeCloud, string) {
	f := &fakeCloud{failing: failing}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var req struct {
			Params struct {
				Name string         `json:"name"`
				Args map[string]any `json:"arguments"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		dirs := func(d ...[2]string) map[string]any {
			var l []map[string]any
			for _, x := range d {
				l = append(l, map[string]any{"dir_key": x[0], "dir_name": x[1]})
			}
			return map[string]any{"pdir_key": "root", "dir_list": l, "file_list": []any{}, "finish_flag": true}
		}
		var out any
		switch key, _ := req.Params.Args["dir_key"].(string); {
		case req.Params.Name != "weiyun.list":
			out = map[string]any{"error": "not supported by this fake"}
		case key == "":
			out = dirs([2]string{"kr", "SessionMemory"})
		case key == "kr":
			out = dirs([2]string{"kp", "public"}, [2]string{"km", "manifests"})
		case key == "km" && f.failing:
			w.WriteHeader(500)
			return
		default:
			out = dirs()
		}
		b, _ := json.Marshal(out)
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}})
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// The cloud could not be read after the key was found missing: that is not
// "no vault there", so no second vault is created.
func TestSetupDoesNotInitWhenTheCloudCannotBeRead(t *testing.T) {
	_, url := newFakeCloud(t, true)
	t.Setenv("WEIYUN_MCP_TOKEN", "tok")
	t.Setenv("WEIYUN_MCP_URL", url)
	root := t.TempDir()
	m := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(filepath.Join(m.home, "codex"), 0700)
	m.must("config", "set", "agents.codex.home", filepath.Join(m.home, "codex"))
	m.must("config", "set", "transport.weiyun", "SessionMemory")
	calls := selfVia(t, m, nil)
	code, _, errs := m.run("setup", "--no-autosync")
	if code == 0 || !strings.Contains(errs, "cannot reach Weiyun") {
		t.Fatalf("%d %s", code, errs)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "init") {
			t.Fatalf("a vault was created: %v", *calls)
		}
	}
	if _, err := device.At(filepath.Join(m.home, "sm")).Load(); err == nil {
		t.Fatal("a device was created")
	}
}

// What relay says about projects that have no folder here yet has nothing for
// a person to fill in, and a background run does not repeat it.
func TestRelayUnmappedHintIsPlainAndQuietRunsStaySilent(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := newComputer(t, root, "a", shared)
	B := newComputer(t, root, "b", shared)
	proj := filepath.Join(A.home, "code", "app")
	os.MkdirAll(proj, 0700)
	addCodexSession(A, 21, proj)
	A.must("init")
	A.must("transport", "attach-folder", shared)
	A.must("project", "track", "--cwd", proj)
	A.must("sync")
	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B.must("device", "join", bundle, "--passphrase-file", pf)

	if out := B.must("relay", "--quiet", "--client-closed"); !strings.Contains(out, "have no folder here yet (app)") {
		t.Fatalf("the quiet run that receives the project says so once:\n%s", out)
	}
	B.must("relay", "--quiet", "--client-closed") // publishes B's own receipt
	if out := B.must("relay", "--quiet", "--client-closed"); out != "" {
		t.Fatalf("a quiet run with only unmapped projects must print nothing:\n%s", out)
	}
	out := B.must("relay", "--client-closed")
	if !strings.Contains(out, "have no folder here yet (app)") || strings.Contains(out, "<") || strings.Contains(out, "--client-closed") {
		t.Fatalf("hint:\n%s", out)
	}
}

// Every marker the program prints for the agent is explained in INSTALL.md, and
// the ones that tell somebody to run something give the full path.
func TestInstallGuideCoversEveryMarker(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "INSTALL.md"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("cmd_onboard.go")
	if err != nil {
		t.Fatal(err)
	}
	markers := map[string]bool{}
	for _, m := range regexp.MustCompile(`\b(NEEDS_[A-Z_]+|PENDING|FIRST_COMPUTER|JOINED|ALREADY_CONNECTED)\b`).FindAllString(string(src), -1) {
		markers[m] = true
	}
	for _, want := range []string{"NEEDS_WEIYUN", "NEEDS_USER", "NEEDS_ACTION", "PENDING", "FIRST_COMPUTER", "JOINED", "ALREADY_CONNECTED"} {
		if !markers[want] {
			t.Errorf("the program no longer prints %s?", want)
		}
	}
	for m := range markers {
		if !strings.Contains(string(guide), m) {
			t.Errorf("INSTALL.md does not explain %s", m)
		}
	}

	if !filepath.IsAbs(selfPath()) {
		t.Fatalf("selfPath %q", selfPath())
	}
	if h := needsWeiyunHint(); !strings.HasPrefix(h, "NEEDS_WEIYUN") || !strings.Contains(h, selfPath()+" setup") {
		t.Fatalf("hint: %s", h)
	}
	if runtime.GOOS != "darwin" {
		return
	}
	for name, block := range map[string]bool{"double-click": false, "terminal": true} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if block { // the file cannot be written, so the person is told to run the command
			os.MkdirAll(filepath.Join(home, "开启同步.command"), 0700)
		}
		old := selfCommand
		selfCommand = func(ctx context.Context, args ...string) (string, error) { return "", errors.New("no launchd here") }
		var out strings.Builder
		a := &App{}
		a.startBackgroundSync(context.Background(), func(f string, v ...any) { fmt.Fprintf(&out, f+"\n", v...) })
		selfCommand = old
		line := strings.TrimSpace(out.String())
		want := filepath.Join(home, "开启同步.command")
		if block {
			want = selfPath()
		}
		if !strings.HasPrefix(line, "NEEDS_USER") || !strings.Contains(line, want) {
			t.Errorf("%s: %q", name, line)
		}
	}
}

// Day-to-day relay asks Weiyun for nothing extra once setup has run: the new
// folder search is local, and the key repair needs its marker.
func TestSteadyRelayMakesNoExtraWeiyunCalls(t *testing.T) {
	f, url := newFakeCloud(t, false)
	t.Setenv("WEIYUN_MCP_TOKEN", "tok")
	t.Setenv("WEIYUN_MCP_URL", url)
	root := t.TempDir()
	m := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(filepath.Join(m.home, "codex"), 0700)
	m.must("config", "set", "agents.codex.home", filepath.Join(m.home, "codex"))
	m.must("init")
	m.must("config", "set", "transport.weiyun", "SessionMemory")
	count := func() int64 {
		before := f.calls.Load()
		if code, out, errs := m.run("relay", "--client-closed"); code != 0 {
			t.Fatalf("relay: %d\n%s\n%s", code, out, errs)
		}
		return f.calls.Load() - before
	}
	count() // learns the folder keys
	plain := count()
	if plain == 0 {
		t.Fatal("the relay never reached the fake Weiyun, so the comparison means nothing")
	}
	touch(m.keyMarker(markTrackNew))
	if withMarker := count(); withMarker != plain {
		t.Fatalf("relay after setup makes %d Weiyun calls, a plain one %d", withMarker, plain)
	}
}

func TestSetupNeedsAConnection(t *testing.T) {
	isolate(t)
	t.Setenv(envWeiyunToken, "")
	code, _, errs := exec(t, "setup", "--no-autosync")
	if code == 0 || !strings.Contains(errs, "NEEDS_WEIYUN") {
		t.Fatalf("%d %s", code, errs)
	}
}

// Weiyun already authorised for the agent's own skill: its token is used, and
// the environment and the saved token still come first.
func TestWeiyunTokenFromTheSkillConfig(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv(envWeiyunToken, "")
	t.Setenv(envWeiyunToken2, "")
	os.MkdirAll(filepath.Join(h, ".mcporter"), 0700)
	os.WriteFile(filepath.Join(h, ".mcporter", "mcporter.json"), []byte(`{"mcpServers":{"weiyun":{"headers":{"WyHeader":"mcp_token=skill-tok"}}}}`), 0600)
	a := &App{Keys: vault.MemStore{}}
	if got := tokens(a); got["primary"] != "skill-tok" || len(got) != 1 {
		t.Fatalf("from the skill config: %v", got)
	}
	if err := a.saveWeiyunToken(); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(h, ".mcporter", "mcporter.json"))
	if got := tokens(a); got["primary"] != "skill-tok" {
		t.Fatalf("the token must outlive the skill config: %v", got)
	}
	t.Setenv(envWeiyunToken, "env-tok")
	if got := tokens(a); got["primary"] != "env-tok" {
		t.Fatalf("environment first: %v", got)
	}
}

// A cloud folder that holds a vault whose key was removed must stop setup
// before `init` makes a second, unrelated vault identity on this computer.
func TestSetupMissingFolderKeyDoesNotCreateForeignIdentity(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	a := newComputer(t, root, "a", shared)
	a.must("init")
	a.must("transport", "attach-folder", shared)
	a.must("key", "remove", "--yes")
	b := newComputer(t, root, "b", shared)
	ran := selfVia(t, b, nil)
	code, _, errs := b.run("setup", "--no-autosync")
	if code == 0 {
		t.Fatal("setup must refuse joining a vault with no key")
	}
	if !strings.Contains(errs, "key publish") {
		t.Fatalf("the refusal should say how to republish the key: %s", errs)
	}
	for _, c := range *ran {
		if strings.HasPrefix(c, "init") {
			t.Fatalf("setup ran %q before refusing", c)
		}
	}
	if info, err := device.At(filepath.Join(b.home, "sm")).Load(); err == nil {
		t.Fatalf("failed setup persisted a new unrelated vault identity: %s", info.VaultID)
	}
}

// reset puts the state and the settings aside (nothing deleted), and setup
// can then start again from nothing and rejoin the vault that is in the cloud.
func TestResetPutsStateAsideAndSetupStartsAgain(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := newComputer(t, root, "a", shared)
	app := filepath.Join(A.home, "work", "app")
	os.MkdirAll(app, 0700)
	addCodexSession(A, 31, app)
	selfVia(t, A, nil)
	A.must("setup", "--no-autosync", "--days", "36500")
	state := filepath.Join(A.home, "sm")
	cfgFile := filepath.Join(A.home, ".config", "session-memory", "config.json")
	for _, p := range []string{state, cfgFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s after setup: %v", p, err)
		}
	}

	out := A.must("reset")
	if !strings.Contains(out, "run it again with --yes") && !strings.Contains(out, "Run it again with --yes") {
		t.Fatalf("a plain reset must only say what it would do:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, "device")); err != nil {
		t.Fatalf("a plain reset moved the state: %v", err)
	}

	out = A.must("reset", "--yes")
	if lastLine(out)[:len("done: RESET")] != "done: RESET" {
		t.Fatalf("last line:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, "device")); err == nil {
		t.Fatal("the device identity is still in the state folder")
	}
	if _, err := os.Stat(cfgFile); err == nil {
		t.Fatal("the settings are still in place")
	}
	baks, _ := filepath.Glob(state + ".bak-*")
	cfgBaks, _ := filepath.Glob(cfgFile + ".bak-*")
	if len(baks) != 1 || len(cfgBaks) != 1 {
		t.Fatalf("state and settings must be put aside, not deleted: %v %v", baks, cfgBaks)
	}
	if files, _ := filepath.Glob(filepath.Join(A.home, "codex", "sessions", "*", "*", "*", "*.jsonl")); len(files) != 1 {
		t.Fatalf("the agent's own sessions must not be touched: %v", files)
	}

	// the settings are gone: point at the cloud folder again, then start over
	A.must("config", "set", "transport.folder", shared)
	A.must("config", "set", "agents.codex.home", filepath.Join(A.home, "codex"))
	out = A.must("setup", "--no-autosync", "--client-closed", "--days", "36500")
	if !strings.Contains(out, "done: JOINED") {
		t.Fatalf("after a reset, setup must rejoin the vault in the cloud:\n%s", out)
	}
}
