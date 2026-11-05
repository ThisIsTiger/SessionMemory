package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/transport"
)

// What a background round learned is told to the person in two ways: the
// notification centre says it when it happens (a conversation arrived, one is
// waiting for an app to be quit, something needs a decision), and `check` says
// it whenever asked. Both read the files below.

// showNotification puts a notification on screen; tests replace it. The text
// goes to osascript as arguments, never inside the script.
var showNotification = func(title, body string) {
	if runtime.GOOS != "darwin" {
		return
	}
	osexec.Command("osascript",
		"-e", "on run argv",
		"-e", "display notification (item 1 of argv) with title (item 2 of argv)",
		"-e", "end run",
		body, title).Run()
}

const notifyTitle = "会话同步"

func (a *App) notifiedFile() string { return filepath.Join(a.Cfg.StateDir(), "notified.json") }

// notify shows body unless notifications are off, or the same key was shown
// less than every ago. It is for background runs only: a command the person
// typed prints its result, and a hook's push must stay silent.
func (a *App) notify(key string, every time.Duration, body string) {
	if !a.quietRun || !a.Cfg.NotifyEnabled() {
		return
	}
	seen := map[string]time.Time{}
	if b, err := os.ReadFile(a.notifiedFile()); err == nil {
		json.Unmarshal(b, &seen)
	}
	now := a.Now()
	if t, ok := seen[key]; ok && every > 0 && now.Sub(t) < every {
		return
	}
	seen[key] = now
	for k, t := range seen { // forget what is long past
		if now.Sub(t) > 7*24*time.Hour {
			delete(seen, k)
		}
	}
	if b, err := json.Marshal(seen); err == nil {
		platform.AtomicWrite(a.notifiedFile(), b, 0600)
	}
	showNotification(notifyTitle, body)
}

// roundResult is how the last background round that really ran ended.
type roundResult struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Kind  string    `json:"kind,omitempty"`  // auth | attention | other
	Error string    `json:"error,omitempty"` // short, never holds a token
	Since time.Time `json:"since,omitempty"` // when the current run of failures began
}

func (a *App) roundFile() string { return filepath.Join(a.Cfg.StateDir(), "last-round.json") }

func (a *App) loadRound() (roundResult, bool) {
	var r roundResult
	b, err := os.ReadFile(a.roundFile())
	if err != nil || json.Unmarshal(b, &r) != nil || r.At.IsZero() {
		return roundResult{}, false
	}
	return r, true
}

// roundKind sorts a failed round by what the person can do about it.
func roundKind(err error) string {
	switch {
	case errors.Is(err, transport.ErrAuth):
		return "auth"
	case errcode.CodeOf(err) == errcode.RecoveryNeeded:
		return "attention"
	}
	return "other"
}

// noteOutcome records how a background round ended and tells the person about
// the failures they can do something about: a login that stopped working (at
// most once a day), sessions that need a decision, and any other failure that
// has lasted an hour. A short outage of the network or Weiyun says nothing.
func (a *App) noteOutcome(err error) {
	prev, _ := a.loadRound()
	r := roundResult{At: a.Now(), OK: err == nil}
	if err != nil {
		r.Kind = roundKind(err)
		msg := strings.ReplaceAll(err.Error(), "\n", " ")
		if len(msg) > 300 {
			msg = msg[:300]
		}
		r.Error = msg
		r.Since = prev.Since
		if prev.OK || r.Since.IsZero() {
			r.Since = r.At
		}
	}
	if b, mErr := json.Marshal(r); mErr == nil {
		platform.AtomicWrite(a.roundFile(), b, 0600)
	}
	switch {
	case err == nil:
	case r.Kind == "auth":
		a.notify("auth", 24*time.Hour, "微云的登录失效了，同步已暂停。对 AI 助手说“重新连接微云”就能恢复。")
	case r.Kind == "attention":
		a.notify("attention", 12*time.Hour, "有对话需要你决定怎么处理（同一个对话在两台电脑上都改过了）。请让 AI 助手“检查会话同步”。")
	case a.Now().Sub(r.Since) >= time.Hour:
		a.notify("failing", 6*time.Hour, "会话同步已经连续失败超过一小时。请让 AI 助手“检查会话同步”。")
	}
}

// agentTitle is how an agent is called in front of the person.
func agentTitle(name string) string {
	switch name {
	case "codex":
		return "Codex"
	case "workbuddy":
		return "WorkBuddy"
	case "claude":
		return "Claude"
	}
	return name
}

// projectList names a few projects for a notification.
func projectList(labels []string) string {
	sort.Strings(labels)
	if len(labels) > 3 {
		return strings.Join(labels[:3], "、") + fmt.Sprintf(" 等 %d 个", len(labels))
	}
	return strings.Join(labels, "、")
}
