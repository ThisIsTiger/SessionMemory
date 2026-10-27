package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/transport"
	"session-memory/internal/vault"
)

// TestLiveWeiyunTwoMachines runs the two-machine flow through the real Weiyun
// service. It needs WEIYUN_MCP_TOKEN and SM_LIVE_WEIYUN=1, uses a throwaway
// remote directory and deletes it afterwards.
func TestLiveWeiyunTwoMachines(t *testing.T) {
	if os.Getenv("SM_LIVE_WEIYUN") != "1" || os.Getenv(envWeiyunToken) == "" {
		t.Skip("set SM_LIVE_WEIYUN=1 and WEIYUN_MCP_TOKEN")
	}
	name := fmt.Sprintf("SessionMemory-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		m := transport.NewMirror(transport.NewFolder(t.TempDir()), transport.NewWeiyunClient(os.Getenv(envWeiyunToken)), name)
		if err := m.RemoveTestVault(context.Background()); err != nil {
			t.Errorf("cleanup of %s failed: %v", name, err)
		}
	})
	root := t.TempDir()
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)

	A.must("init")
	A.must("transport", "attach-weiyun", name)
	acodex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)
	tid := codexgen.ThreadIDFor(1)
	p := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 1, Turns: 3}), 0600)
	var tr struct {
		SessionID  string `json:"session_id"`
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal([]byte(A.must("track", "--native-session", tid, "--json")), &tr); err != nil {
		t.Fatal(err)
	}
	A.must("sync")

	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "join.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B.must("config", "set", "transport.weiyun", name)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	B.must("sync")
	if out := B.must("revisions", tr.SessionID, "--json"); !strings.Contains(out, tr.RevisionID) {
		t.Fatalf("B did not receive the revision over Weiyun: %s", out)
	}

	orig, _ := os.ReadFile(p)
	os.WriteFile(p, append(orig, []byte(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"z"}}`+"\n")...), 0600)
	A.must("checkpoint", tr.SessionID)
	B.must("sync")
	var rv struct {
		Revisions []struct {
			RevisionID string `json:"revision_id"`
		}
		Heads []string
	}
	json.Unmarshal([]byte(B.must("revisions", tr.SessionID, "--json")), &rv)
	if len(rv.Revisions) != 2 || len(rv.Heads) != 1 {
		t.Fatalf("B revisions=%d heads=%d", len(rv.Revisions), len(rv.Heads))
	}
}
