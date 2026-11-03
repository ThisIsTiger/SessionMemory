package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
)

// failingCapture is an adapter whose reads of some native sessions fail.
type failingCapture struct {
	agent.Adapter
	bad map[string]bool
}

func (f failingCapture) Capture(ctx context.Context, ref agent.NativeRef, cur agent.Cursor, sink agent.SegmentSink) (agent.CaptureResult, error) {
	if f.bad[ref.NativeID] {
		return agent.CaptureResult{}, errors.New("simulated read failure")
	}
	return f.Adapter.Capture(ctx, ref, cur, sink)
}

// A session that cannot be tracked must not keep the other sessions of its
// project from being tracked, and the failure must be reported, not dropped.
func TestProjectTrackGoesOnPastASessionThatFails(t *testing.T) {
	root := t.TempDir()
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", filepath.Join(root, "cloud"))
	codex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", codex)
	proj := filepath.Join(A.home, "code", "app")
	os.MkdirAll(proj, 0700)
	add := func(seed int64) string {
		tid := codexgen.ThreadIDFor(seed)
		p := filepath.Join(codex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: seed, Turns: 2, Cwd: proj}), 0600)
		return tid
	}
	bad := add(21)
	good1, good2 := add(22), add(23)
	failing := map[string]bool{bad: true}
	old := wrapAdapter
	wrapAdapter = func(ad agent.Adapter) agent.Adapter { return failingCapture{Adapter: ad, bad: failing} }
	defer func() { wrapAdapter = old }()
	read := func() map[string]string { // native-agnostic: session rows by "heads=" value
		heads := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(A.must("list")), "\n") {
			for _, f := range strings.Fields(line) {
				if strings.HasPrefix(f, "heads=") {
					heads[strings.Fields(line)[0]] = strings.TrimPrefix(f, "heads=")
				}
			}
		}
		return heads
	}
	count := func(h map[string]string, want string) (n int) {
		for _, v := range h {
			if v == want {
				n++
			}
		}
		return n
	}

	code, _, errs := A.run("project", "track", "--cwd", proj)
	if code == 0 || !strings.Contains(errs, bad) {
		t.Fatalf("the failure must be reported with the session it belongs to: code=%d err=%s", code, errs)
	}
	if h := read(); count(h, "1") != 2 {
		t.Fatalf("both healthy sessions must be captured although one failed (%s, %s): %v", good1, good2, h)
	}

	// a later sync follows the project: a new healthy session joins although
	// another new one cannot be read, and that failure is not silent
	bad2 := add(24)
	good3 := add(25)
	failing = map[string]bool{bad2: true}
	wrapAdapter = func(ad agent.Adapter) agent.Adapter { return failingCapture{Adapter: ad, bad: failing} }
	code, out, errs := A.run("sync", "--json")
	// the round is not clean (the new session that failed is bound and cannot be captured yet)
	if code == 0 {
		t.Fatalf("a round with a session that could not be read must not be reported clean: %s", errs)
	}
	var sy map[string]int
	if err := json.Unmarshal([]byte(out), &sy); err != nil || sy["auto_tracked"] != 1 {
		t.Fatalf("auto-track should add the one healthy new session (%s): %v %+v", good3, err, sy)
	}
	if !strings.Contains(errs, bad2) {
		t.Fatalf("the session that could not be tracked must be named on stderr: %q", errs)
	}
	// the first failing session had been bound before its read failed; it is
	// captured by a normal round once it can be read again
	if h := read(); count(h, "1") != 4 {
		t.Fatalf("want the 2 first healthy, the recovered one and the new healthy one captured: %v", h)
	}
}

func TestTestsDoNotSeeTheDevelopersWeiyunLogin(t *testing.T) {
	if os.Getenv("SM_LIVE_WEIYUN") == "1" {
		t.Skip("a live run uses the real login on purpose")
	}
	for _, name := range []string{envWeiyunToken, envWeiyunToken2, "WEIYUN_MCP_URL"} {
		if os.Getenv(name) != "" {
			t.Errorf("%s is set in the test process", name)
		}
	}
}
