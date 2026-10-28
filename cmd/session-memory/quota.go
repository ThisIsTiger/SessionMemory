package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"session-memory/internal/platform"
)

// Weiyun refuses every call once the day's quota is used up. Background runs
// then stop asking until the quota can be back, instead of failing every
// two minutes (and keeping the quota gone for the other computer too).
// A run the user starts by hand always tries.

type quotaPause struct {
	Until   time.Time `json:"until"`
	Strikes int       `json:"strikes"` // refusals in a row; reset by any call that goes through
}

// one pause per Weiyun account (primary, backup)
type quotaState map[string]quotaPause

func (a *App) quotaFile() string { return filepath.Join(a.Cfg.StateDir(), "weiyun-quota.json") }

func (a *App) loadQuota() quotaState {
	q := quotaState{}
	if b, err := os.ReadFile(a.quotaFile()); err == nil && json.Unmarshal(b, &q) != nil {
		return quotaState{} // an unreadable (older) file holds no pause
	}
	return q
}

func (a *App) saveQuota(q quotaState) {
	if len(q) == 0 {
		os.Remove(a.quotaFile())
		return
	}
	b, _ := json.Marshal(q)
	platform.AtomicWrite(a.quotaFile(), b, 0600)
}

// quotaPausedUntil says until when background runs hold off asking this
// account, if they do.
func (a *App) quotaPausedUntil(label string) (time.Time, bool) {
	p, ok := a.loadQuota()[label]
	if !ok || !a.Now().Before(p.Until) {
		return time.Time{}, false
	}
	return p.Until, true
}

// allAccountsPaused is true when there are accounts and every one is paused.
func (a *App) allAccountsPaused() bool {
	accts := a.weiyunAccounts()
	for _, ac := range accts {
		if _, paused := a.quotaPausedUntil(ac.id); !paused {
			return false
		}
	}
	return len(accts) > 0
}

// recordQuota notes a refusal by one account. The first one waits for the next
// day (shortly after local midnight); if the quota is still gone then, the wait
// is an hour longer each time, up to six hours, since the reset time is not
// known.
func (a *App) recordQuota(label string) {
	now := a.Now()
	all := a.loadQuota()
	q, had := all[label]
	if had && now.Before(q.Until) {
		return // already paused; a run started by hand does not move it
	}
	q.Strikes++
	if q.Strikes == 1 {
		y, m, d := now.Date()
		q.Until = time.Date(y, m, d+1, 0, 5, 0, 0, now.Location())
	} else {
		h := q.Strikes - 1
		if h > 6 {
			h = 6
		}
		q.Until = now.Add(time.Duration(h) * time.Hour)
	}
	all[label] = q
	a.saveQuota(all)
}

func (a *App) clearQuota(label string) {
	all := a.loadQuota()
	if _, ok := all[label]; ok {
		delete(all, label)
		a.saveQuota(all)
	}
}

// A background run that found nothing to do makes the next ones rarer: after
// three quiet rounds the wait doubles each time, up to ten minutes. Anything
// that moved (something published, received, captured or installed) brings it
// back to the normal rhythm. Runs started by hand never wait.
type idleState struct {
	LastPoll time.Time `json:"last_poll"`
	Quiet    int       `json:"quiet"`
}

func (a *App) idleFile() string { return filepath.Join(a.Cfg.StateDir(), "autosync-idle.json") }

func (a *App) loadIdle() idleState {
	var s idleState
	if b, err := os.ReadFile(a.idleFile()); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

const (
	idleBase  = 2 * time.Minute
	idleCap   = 10 * time.Minute
	idleSlack = 15 * time.Second // the scheduler's ticks are not exact
)

func idleWait(quiet int) time.Duration {
	if quiet < 3 {
		return 0
	}
	if quiet > 6 {
		return idleCap
	}
	w := idleBase << (quiet - 2)
	if w > idleCap {
		w = idleCap
	}
	return w
}

// idleSkip reports whether a background run should sit this tick out.
func (a *App) idleSkip() bool {
	s := a.loadIdle()
	w := idleWait(s.Quiet)
	return w > 0 && !s.LastPoll.IsZero() && a.Now().Sub(s.LastPoll) < w-idleSlack
}

func (a *App) noteRound(active bool) {
	s := a.loadIdle()
	if active {
		s.Quiet = 0
	} else if s.Quiet < 100 {
		s.Quiet++
	}
	s.LastPoll = a.Now()
	b, _ := json.Marshal(s)
	platform.AtomicWrite(a.idleFile(), b, 0600)
}

// Older versions keyed the pause and the "behind" mark by slot ("primary",
// "backup"); they now belong to the account (slot#fingerprint). A leftover
// "behind" mark is taken over by the account currently in that slot, so the
// fact that an account missed writes is not lost; a leftover pause is dropped.
func (a *App) migrateAccountKeys(accounts []wyAccount) {
	slot := map[string]string{}
	for _, ac := range accounts {
		slot[ac.label] = ac.id
	}
	// a pause under a slot name may have been recorded for another account's token:
	// it is dropped (one call finds out again) rather than handed to whoever is there now
	q := a.loadQuota()
	changed := false
	for k := range q {
		if _, ok := slot[k]; ok {
			delete(q, k)
			changed = true
		}
	}
	if changed {
		a.saveQuota(q)
	}
	f := filepath.Join(a.Cfg.StateDir(), "weiyun-behind.json")
	b := map[string]bool{}
	if raw, err := os.ReadFile(f); err == nil && json.Unmarshal(raw, &b) == nil {
		moved := false
		for k, v := range b {
			if id, ok := slot[k]; ok {
				b[id] = v
				delete(b, k)
				moved = true
			}
		}
		if moved {
			out, _ := json.Marshal(b)
			platform.AtomicWrite(f, out, 0600)
		}
	}
}
