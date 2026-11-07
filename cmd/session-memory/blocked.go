package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"session-memory/internal/platform"
)

// A session that a relay round could not bring up to date here (the app
// refuses to take it, or it forked) is written down with the reason, so that
// `check` can say what is really holding it back instead of guessing from what
// is merely not installed yet.

type blockedEntry struct {
	Code   string    `json:"code"`
	Reason string    `json:"reason"`
	Agent  string    `json:"agent"`
	Since  time.Time `json:"since"`
}

type blockedState struct {
	At       time.Time               `json:"at"`
	Sessions map[string]blockedEntry `json:"sessions"`
}

func (a *App) blockedFile() string { return filepath.Join(a.Cfg.StateDir(), "blocked-sessions.json") }

func (a *App) loadBlocked() (blockedState, bool) {
	var s blockedState
	b, err := os.ReadFile(a.blockedFile())
	if err != nil || json.Unmarshal(b, &s) != nil || s.At.IsZero() {
		return blockedState{}, false
	}
	return s, true
}

func blockedReason(reason string) string {
	reason = strings.ReplaceAll(reason, "\n", " ")
	if len(reason) > 200 {
		reason = reason[:200]
	}
	return reason
}

// noteBlocked replaces the record with what the round that just ran found and
// says how many of those sessions were not in it before (or are refused for
// another reason now). A session that stays blocked for the same reason keeps
// its first date.
func (a *App) noteBlocked(now []blockedSession) (newly int) {
	prev, _ := a.loadBlocked()
	s := blockedState{At: a.Now(), Sessions: map[string]blockedEntry{}}
	for _, b := range now {
		reason := blockedReason(b.Reason)
		e := blockedEntry{Code: b.Code, Reason: reason, Agent: b.Agent, Since: s.At}
		if old, ok := prev.Sessions[b.ID]; ok && old.Reason == reason {
			e.Since = old.Since
		} else {
			newly++
		}
		s.Sessions[b.ID] = e
	}
	if len(s.Sessions) == 0 {
		os.Remove(a.blockedFile())
		return 0
	}
	if b, err := json.Marshal(s); err == nil {
		platform.AtomicWrite(a.blockedFile(), b, 0600)
	}
	return newly
}

// blockedSession is one session an install was refused for.
type blockedSession struct {
	ID, Code, Reason, Agent string
}

// how long a record of blocked sessions is believed: relay rewrites it every
// round, so an old one means rounds are not running and `check` says that
// elsewhere.
const blockedFresh = 6 * time.Hour

// explainBlocked puts a refusal into words for the person and says whether
// there is anything they can do about it.
func explainBlocked(reason string) (what string, canAct bool) {
	switch {
	case strings.Contains(reason, "exactly one active local account"):
		return "WorkBuddy 里有不止一个账号的对话，程序没法确定该装到哪个账号下，所以装不进去（已知限制，目前没有办法在程序里解决）", false
	case strings.Contains(reason, "local or unmanaged content"):
		return "本机的 WorkBuddy 里已经有同一个对话的文件，但程序没跟踪过它，不敢覆盖，所以装不进去（已知限制）", false
	case strings.Contains(reason, "only in compressed form"):
		return "Codex 里这个对话只有压缩存档的形式，没法往里写（已知限制）", false
	}
	return "装不进去：" + reason, true
}
