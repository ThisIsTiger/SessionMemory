package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/config"
	"session-memory/internal/errcode"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/transport"
	"session-memory/internal/vault"
)

func runCheck(t *testing.T, m *machine) checkReport {
	t.Helper()
	var rep checkReport
	if err := json.Unmarshal([]byte(m.must("check", "--json")), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func (r checkReport) mentions(s string) bool {
	return strings.Contains(strings.Join(r.Details, "\n"), s) || strings.Contains(strings.Join(r.Next, "\n"), s)
}

func putJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func withJob(t *testing.T, st jobState) {
	old := backgroundJob
	backgroundJob = func() jobState { return st }
	t.Cleanup(func() { backgroundJob = old })
}

func TestCheckOnAComputerThatIsNotSetUp(t *testing.T) {
	isolate(t)
	code, out, errs := exec(t, "check", "--json")
	var rep checkReport
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || rep.Level != "not_set_up" || !rep.mentions("setup") {
		t.Fatalf("exit %d out=%s err=%s", code, out, errs)
	}
	if code, out, _ := exec(t, "check"); code != 0 || !strings.Contains(out, "还没有设置") {
		t.Fatalf("plain output: %d %s", code, out)
	}
}

func TestCheckSaysWhatIsRightAndWhatIsWrong(t *testing.T) {
	m := readyComputer(t)
	state := filepath.Join(m.home, "sm")
	now := time.Now()

	rep := runCheck(t, m)
	if rep.Level != "ok" || rep.Headline != "同步正常" || len(rep.Next) != 0 || !rep.mentions("还没有看到别的电脑") {
		t.Fatalf("a fresh computer: %+v", rep)
	}
	if out := m.must("check"); !strings.HasPrefix(out, "同步正常\n") {
		t.Fatalf("plain output:\n%s", out)
	}

	withJob(t, jobOff)
	if rep := runCheck(t, m); rep.Level != "problem" || !rep.mentions("autosync enable") || !rep.mentions("开启同步.command") {
		t.Fatalf("job off: %+v", rep)
	}
	withJob(t, jobNotLoaded)
	if rep := runCheck(t, m); rep.Level != "problem" || !rep.mentions("没有加载") {
		t.Fatalf("job not loaded: %+v", rep)
	}

	withJob(t, jobRunning)
	putJSON(t, filepath.Join(state, "autosync-idle.json"), idleState{LastPoll: now.Add(-3 * time.Hour)})
	if rep := runCheck(t, m); rep.Level != "attention" || !rep.mentions("3 小时前") || !rep.mentions("卡住") {
		t.Fatalf("stale job: %+v", rep)
	}
	putJSON(t, filepath.Join(state, "last-round.json"), roundResult{At: now.Add(-10 * time.Minute), OK: true})
	if rep := runCheck(t, m); rep.Level != "ok" || !rep.mentions("后台同步已开启，10 分钟前检查过") {
		t.Fatalf("healthy job: %+v", rep)
	}

	// a login that stopped working: the person is told what to do about it, without the error text of the service
	putJSON(t, filepath.Join(state, "last-round.json"), roundResult{At: now, Kind: "auth", Error: "weiyun token rejected", Since: now.Add(-5 * time.Hour)})
	if rep := runCheck(t, m); rep.Level != "problem" || !rep.mentions("登录失效") || !rep.mentions("重新授权") {
		t.Fatalf("auth failure: %+v", rep)
	}
	// a short outage says nothing alarming, a long one does
	putJSON(t, filepath.Join(state, "last-round.json"), roundResult{At: now, Kind: "other", Error: "network down", Since: now.Add(-5 * time.Minute)})
	if rep := runCheck(t, m); rep.Level != "ok" || !rep.mentions("会自动重试") {
		t.Fatalf("short outage: %+v", rep)
	}
	putJSON(t, filepath.Join(state, "last-round.json"), roundResult{At: now, Kind: "other", Error: "network down", Since: now.Add(-3 * time.Hour)})
	if rep := runCheck(t, m); rep.Level != "problem" || !rep.mentions("连续失败 3 小时") {
		t.Fatalf("long outage: %+v", rep)
	}
	putJSON(t, filepath.Join(state, "last-round.json"), roundResult{At: now, Kind: "attention", Error: "2 session(s) need attention", Since: now})
	if rep := runCheck(t, m); rep.Level != "attention" || !rep.mentions("没能装进本机") || !rep.mentions("relay") || !rep.mentions("adopt") {
		t.Fatalf("sessions that need a decision: %+v", rep)
	}
}

func TestCheckCountsSessionsAndSeesTheOtherComputer(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := newComputer(t, root, "a", shared)
	A.must("init")
	proj := filepath.Join(A.home, "code", "app")
	os.MkdirAll(proj, 0700)
	addCodexSession(A, 31, proj)
	addCodexSession(A, 32, proj)
	A.must("project", "track", "--cwd", proj)
	A.must("relay", "--client-closed")
	if rep := runCheck(t, A); !rep.mentions("Codex 2 个") || !rep.mentions("都已上传") || rep.Level != "ok" {
		t.Fatalf("after the upload: %+v", rep)
	}

	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B := newComputer(t, root, "b", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	B.must("relay", "--client-closed")
	rep := runCheck(t, B)
	if !rep.mentions("别的电脑（设备") || !rep.mentions("项目 app（2 个 Codex 对话）") || !rep.mentions("叫 app 的文件夹") || rep.Level != "attention" || rep.mentions("还没有看到别的电脑") {
		t.Fatalf("B sees A: %+v", rep)
	}
}

func TestCheckNamesAPausedWeiyun(t *testing.T) {
	m := &machine{t: t, home: filepath.Join(t.TempDir(), "a"), keys: vault.MemStore{}}
	os.MkdirAll(m.home, 0700)
	m.must("init")
	m.must("config", "set", "transport.weiyun", "SessionMemory")
	if rep := runCheck(t, m); rep.Level != "problem" || !rep.mentions("微云没有接通") || !rep.mentions("NEEDS_WEIYUN") {
		t.Fatalf("no Weiyun login: %+v", rep)
	}
	const token = "SYNTHETIC_TOKEN"
	t.Setenv(envWeiyunToken, token)
	sum := sha256.Sum256([]byte(token))
	id := "primary#" + hex.EncodeToString(sum[:])[:6]
	putJSON(t, filepath.Join(m.home, "sm", "weiyun-quota.json"), quotaState{id: {Until: time.Now().Add(3 * time.Hour), Strikes: 1}})
	rep := runCheck(t, m)
	if rep.Level != "attention" || !rep.mentions("额度用完") || !rep.mentions("不会丢") {
		t.Fatalf("paused: %+v", rep)
	}
	if b, _ := json.Marshal(rep); strings.Contains(string(b), token) {
		t.Fatal("the token is in the report")
	}
}

func TestCheckDoesNotWaitForARunningSync(t *testing.T) {
	m := readyComputer(t)
	unlock, err := lockStore(filepath.Join(m.home, "sm"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	old := checkLockWait
	checkLockWait = 20 * time.Millisecond
	defer func() { checkLockWait = old }()
	if rep := runCheck(t, m); rep.Level != "ok" || !rep.mentions("正在同步中") {
		t.Fatalf("%+v", rep)
	}
}

func notices(t *testing.T) *[]string {
	t.Helper()
	var got []string
	old := showNotification
	showNotification = func(title, body string) { got = append(got, title+": "+body) }
	t.Cleanup(func() { showNotification = old })
	return &got
}

func newNotifyApp(t *testing.T) *App {
	cfg := config.DefaultConfig()
	cfg.Home = t.TempDir()
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	a := &App{Cfg: cfg, quietRun: true}
	a.Now = func() time.Time { return now }
	return a
}

func TestNotificationsAreRareAndOnlyFromBackgroundRuns(t *testing.T) {
	got := notices(t)
	a := newNotifyApp(t)
	clock := a.Now()
	a.Now = func() time.Time { return clock }

	a.quietRun = false
	a.notify("k", 0, "typed by hand")
	if len(*got) != 0 {
		t.Fatalf("a command the person typed must not notify: %v", *got)
	}
	a.quietRun = true
	a.notify("waiting", 6*time.Hour, "first")
	a.notify("waiting", 6*time.Hour, "again")
	if len(*got) != 1 {
		t.Fatalf("the same notice twice within its time: %v", *got)
	}
	a.notify("other", 6*time.Hour, "different notice")
	if len(*got) != 2 {
		t.Fatalf("another key is its own notice: %v", *got)
	}
	clock = clock.Add(7 * time.Hour)
	a.notify("waiting", 6*time.Hour, "after a while")
	if len(*got) != 3 || !strings.Contains((*got)[2], "after a while") {
		t.Fatalf("after its time it is shown again: %v", *got)
	}
	a.notify("installed", 0, "one")
	a.notify("installed", 0, "two")
	if len(*got) != 5 {
		t.Fatalf("every=0 means every time: %v", *got)
	}
	off := false
	a.Cfg.Notify = &off
	a.notify("never", 0, "silenced")
	if len(*got) != 5 {
		t.Fatalf("notify=false must silence: %v", *got)
	}
}

func TestFailedRoundsTellThePersonOnlyWhatTheyCanActOn(t *testing.T) {
	got := notices(t)
	a := newNotifyApp(t)
	clock := a.Now()
	a.Now = func() time.Time { return clock }

	a.noteOutcome(fmt.Errorf("weiyun account primary: %w", transport.ErrAuth))
	a.noteOutcome(fmt.Errorf("weiyun account primary: %w", transport.ErrAuth))
	if len(*got) != 1 || !strings.Contains((*got)[0], "登录失效") {
		t.Fatalf("a rejected login is told once: %v", *got)
	}
	if r, ok := a.loadRound(); !ok || r.OK || r.Kind != "auth" || strings.Contains(r.Error, "\n") {
		t.Fatalf("round record: %+v", r)
	}

	a.noteOutcome(nil)
	if r, _ := a.loadRound(); !r.OK || r.Kind != "" {
		t.Fatalf("a good round clears the failure: %+v", r)
	}

	// a network error is silent until it has lasted an hour
	n := len(*got)
	a.noteOutcome(errors.New("dial tcp: no route to host"))
	clock = clock.Add(30 * time.Minute)
	a.noteOutcome(errors.New("dial tcp: no route to host"))
	if len(*got) != n {
		t.Fatalf("a short outage must stay silent: %v", *got)
	}
	clock = clock.Add(31 * time.Minute)
	a.noteOutcome(errors.New("dial tcp: no route to host"))
	if len(*got) != n+1 || !strings.Contains((*got)[n], "一小时") {
		t.Fatalf("an hour-long outage is told: %v", *got)
	}

	a.noteOutcome(errcode.New(errcode.RecoveryNeeded, "2 session(s) need attention"))
	if last := (*got)[len(*got)-1]; !strings.Contains(last, "没能装进这台电脑") {
		t.Fatalf("sessions that need a decision: %v", *got)
	}
}

func TestBackgroundRelayTellsWhenAConversationArrived(t *testing.T) {
	got := notices(t)
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := newComputer(t, root, "a", shared)
	A.must("init")
	projA := filepath.Join(A.home, "code", "app")
	os.MkdirAll(projA, 0700)
	tid := codexgen.ThreadIDFor(41)
	ap := filepath.Join(A.home, "codex", "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(ap), 0700)
	os.WriteFile(ap, codexgen.Rollout(codexgen.Options{Seed: 41, Turns: 2, Cwd: projA}), 0600)
	A.must("project", "track", "--cwd", projA)
	A.must("relay", "--client-closed")

	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B := newComputer(t, root, "b", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	os.MkdirAll(filepath.Join(B.home, "work", "app"), 0700) // the one folder named like the project

	B.must("relay", "--quiet", "--client-closed")
	if len(*got) != 1 || !strings.Contains((*got)[0], "1 个已装进本机") || !strings.Contains((*got)[0], "项目：app") || !strings.Contains((*got)[0], "Codex") {
		t.Fatalf("B should be told once that a conversation arrived: %v", *got)
	}
	if files, _ := filepath.Glob(filepath.Join(B.home, "codex", "sessions", "*", "*", "*", "*.jsonl")); len(files) != 1 {
		t.Fatalf("the conversation is not installed: %v", files)
	}
	B.must("relay", "--quiet", "--client-closed")
	if len(*got) != 1 {
		t.Fatalf("nothing new, nothing said: %v", *got)
	}
	if r, ok := B.stateRound(); !ok || !r.OK {
		t.Fatalf("the round was not recorded: %+v", r)
	}
}

func (m *machine) stateRound() (roundResult, bool) {
	a := &App{}
	cfg := config.DefaultConfig()
	cfg.Home = filepath.Join(m.home, "sm")
	a.Cfg = cfg
	return a.loadRound()
}

func TestSetupEndsWithWordsForThePerson(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want []string
	}{
		{"first", tellUser(vaultCreated, 12, 0, false, false, 0, false, 60), []string{"最近 60 天的 12 个对话", "同一个微信/QQ", "检查会话同步"}},
		{"joined", tellUser(vaultJoined, 5, 2, false, false, 0, true, 60), []string{"加入你微云里原有的同步库", "2 个对话本来就在微云里", "Cmd+Q", "弹出通知"}},
		{"again", tellUser(vaultConnected, 5, 0, true, false, 0, false, 60), []string{"之前就接好了", "不用管"}},
		{"blocked", tellUser(vaultCreated, 5, 0, false, true, 3, false, 60), []string{"有 3 个对话暂时没有同步", "没有删除任何东西"}},
	}
	for _, c := range cases {
		for _, w := range c.want {
			if !strings.Contains(c.got, w) {
				t.Errorf("%s: %q lacks %q", c.name, c.got, w)
			}
		}
		for _, bad := range []string{"adopt", "choose", "relay", "`"} {
			if strings.Contains(c.got, bad) {
				t.Errorf("%s: the words for the person hold %q: %s", c.name, bad, c.got)
			}
		}
	}
}

// What the last relay round refused is what check says, with the reason; it is
// not described as waiting for an app to be quit, which would never help.
func TestCheckExplainsWhatRelayRefused(t *testing.T) {
	_, B, ref := twoComputersWithSessionIn(t, "/Users/ann/Documents/work/app")
	// B has just joined: let the received session be installed, then pretend the
	// agent refused a newer version of it
	B.must("relay", "--client-closed")
	var sid string
	for _, line := range strings.Split(strings.TrimSpace(B.must("list")), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			sid = f[0]
		}
	}
	state := filepath.Join(B.home, "sm")
	putJSON(t, filepath.Join(state, "blocked-sessions.json"), blockedState{At: time.Now(), Sessions: map[string]blockedEntry{
		sid: {Code: "needs_action", Reason: "WorkBuddy target must have exactly one active local account; use a separate profile", Agent: "codex", Since: time.Now().Add(-26 * time.Hour)},
	}})
	rep := runCheck(t, B)
	if rep.Level != "attention" || !rep.mentions("不止一个账号") || !rep.mentions("1 个项目：app") || !rep.mentions("26 小时前起一直如此") || rep.mentions("正在运行") {
		t.Fatalf("%+v", rep)
	}
	for _, n := range rep.Next {
		if strings.Contains(n, "退出") {
			t.Fatalf("quitting the app does not help here, yet: %q", n)
		}
	}
	_ = ref

	// a reason nobody has put into words is shown as it is, and the assistant is told to pass it on
	putJSON(t, filepath.Join(state, "blocked-sessions.json"), blockedState{At: time.Now(), Sessions: map[string]blockedEntry{
		sid: {Code: "unsupported", Reason: "something new and unexpected", Agent: "codex", Since: time.Now()},
	}})
	if rep := runCheck(t, B); !rep.mentions("装不进去：something new and unexpected") || len(rep.Next) == 0 {
		t.Fatalf("%+v", rep)
	}

	// a record from long ago says nothing about now
	putJSON(t, filepath.Join(state, "blocked-sessions.json"), blockedState{At: time.Now().Add(-8 * time.Hour), Sessions: map[string]blockedEntry{
		sid: {Code: "needs_action", Reason: "something", Agent: "codex", Since: time.Now().Add(-9 * time.Hour)},
	}})
	if rep := runCheck(t, B); rep.mentions("something") {
		t.Fatalf("a stale record was believed: %+v", rep)
	}
}
