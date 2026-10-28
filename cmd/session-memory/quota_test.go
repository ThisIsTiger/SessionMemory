package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/config"
	"session-memory/internal/vault"
)

func quotaApp(t *testing.T, now *time.Time) *App {
	cfg := config.DefaultConfig()
	cfg.Home = t.TempDir()
	return &App{Cfg: cfg, Now: func() time.Time { return *now }}
}

// The first refusal holds background runs until just after the next local
// midnight; if the quota is still gone then, each further wait is an hour
// longer (the reset time is not known), and any call that goes through ends it.
func TestQuotaPauseWaitsForTomorrowThenBacksOff(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 28, 20, 7, 0, 0, loc)
	a := quotaApp(t, &now)
	if _, paused := a.quotaPausedUntil("primary"); paused {
		t.Fatal("paused before any refusal")
	}
	a.recordQuota("primary")
	until, paused := a.quotaPausedUntil("primary")
	if !paused || !until.Equal(time.Date(2026, 10, 29, 0, 5, 0, 0, loc)) {
		t.Fatalf("first refusal: %v %v", until, paused)
	}
	// a hand-started run that is refused again does not move it
	now = now.Add(time.Hour)
	a.recordQuota("primary")
	if u2, _ := a.quotaPausedUntil("primary"); !u2.Equal(until) {
		t.Fatalf("pause moved to %v", u2)
	}
	// after midnight it is tried once; still refused: one more hour
	now = time.Date(2026, 10, 29, 0, 6, 0, 0, loc)
	if _, paused := a.quotaPausedUntil("primary"); paused {
		t.Fatal("still paused after the date")
	}
	a.recordQuota("primary")
	if u3, ok := a.quotaPausedUntil("primary"); !ok || !u3.Equal(now.Add(time.Hour)) {
		t.Fatalf("second refusal: %v %v", u3, ok)
	}
	a.clearQuota("primary")
	if _, paused := a.quotaPausedUntil("primary"); paused {
		t.Fatal("pause survived a call that went through")
	}
	a.recordQuota("primary") // starts again from the next day, not from the old count
	if u4, _ := a.quotaPausedUntil("primary"); !u4.Equal(time.Date(2026, 10, 30, 0, 5, 0, 0, loc)) {
		t.Fatalf("after reset: %v", u4)
	}
}

func TestIdleBackoffGrowsAndResets(t *testing.T) {
	for q, want := range map[int]time.Duration{0: 0, 2: 0, 3: 4 * time.Minute, 4: 8 * time.Minute, 5: 10 * time.Minute, 50: 10 * time.Minute} {
		if got := idleWait(q); got != want {
			t.Errorf("idleWait(%d) = %v, want %v", q, got, want)
		}
	}
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	a := quotaApp(t, &now)
	if a.idleSkip() {
		t.Fatal("skipped with no history")
	}
	for i := 0; i < 3; i++ { // three quiet rounds
		a.noteRound(false)
		now = now.Add(2 * time.Minute)
	}
	if !a.idleSkip() { // the next tick, 2 minutes after the last poll, waits (needs 4)
		t.Fatal("did not wait after three quiet rounds")
	}
	now = now.Add(2 * time.Minute)
	if a.idleSkip() {
		t.Fatal("still waiting after 4 minutes")
	}
	a.noteRound(true)
	now = now.Add(2 * time.Minute)
	if a.idleSkip() {
		t.Fatal("activity did not restore the normal rhythm")
	}
}

// A background run refused for quota stops the next background runs from
// asking again; a run started by hand still tries.
func TestRelayQuietHoldsBackAfterQuotaRefusal(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "type:business, code:117401, msg:rate limit exceeded, daily quota used up. Please try again tomorrow"}})
	}))
	defer srv.Close()
	root := t.TempDir()
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	A.must("init")
	A.must("config", "set", "transport.weiyun", "SessionMemory")
	t.Setenv("WEIYUN_MCP_TOKEN", "tok")
	t.Setenv("WEIYUN_MCP_URL", srv.URL)

	if code, _, errs := A.run("relay", "--quiet"); code == 0 || !strings.Contains(errs, "quota") {
		t.Fatalf("first quiet run: exit %d %s", code, errs)
	}
	first := calls
	if first == 0 {
		t.Fatal("the first run never reached Weiyun")
	}
	if _, err := os.Stat(filepath.Join(A.home, "sm", "weiyun-quota.json")); err != nil {
		t.Fatalf("refusal not recorded: %v", err)
	}
	if code, out, errs := A.run("relay", "--quiet"); code != 0 || out != "" || errs != "" || calls != first {
		t.Fatalf("second quiet run: exit %d out=%q err=%q, Weiyun calls %d -> %d", code, out, errs, first, calls)
	}
	if code, _, _ := A.run("relay"); code == 0 || calls == first {
		t.Fatalf("a run started by hand must still try: exit %d, calls %d -> %d", code, first, calls)
	}
}

func TestAccountKeysMoveFromSlotsToAccounts(t *testing.T) {
	now := time.Date(2026, 10, 28, 22, 0, 0, 0, time.UTC)
	a := quotaApp(t, &now)
	os.WriteFile(a.quotaFile(), []byte(`{"backup":{"until":"2026-10-29T00:05:00Z","strikes":1}}`), 0600)
	behind := filepath.Join(a.Cfg.StateDir(), "weiyun-behind.json")
	os.WriteFile(behind, []byte(`{"backup":true}`), 0600)
	a.migrateAccountKeys([]wyAccount{{label: "primary", id: "primary#aaaaaa"}, {label: "backup", id: "backup#bbbbbb"}})
	if _, paused := a.quotaPausedUntil("backup#bbbbbb"); paused {
		t.Fatal("a pause recorded under a slot name must not be handed to the account now in it")
	}
	if raw, _ := os.ReadFile(behind); !strings.Contains(string(raw), "backup#bbbbbb") || strings.Contains(string(raw), `"backup":`) {
		t.Fatalf("behind mark not moved: %s", raw)
	}
}
