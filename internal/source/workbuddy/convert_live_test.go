package workbuddy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/engine"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
	"session-memory/internal/store"
	"session-memory/internal/vault"
)

// TestLiveConvertToCodex is driven by an out-of-tree script that points a real
// Codex client at the produced home (see doc/p0-convert-report.md). It does
// nothing unless SM_LIVE_OUT is set. SM_LIVE_PHASE selects "convert" (build a
// converted Codex thread from a synthetic WorkBuddy session) or "capture"
// (capture what the real Codex appended afterwards).
func TestLiveConvertToCodex(t *testing.T) {
	out := os.Getenv("SM_LIVE_OUT")
	if out == "" {
		t.Skip("SM_LIVE_OUT not set")
	}
	ctx := context.Background()
	type state struct{ VaultID, Key, DeviceID, SessionID, Revision, NativeID string }
	statePath := filepath.Join(out, "state.json")
	var s state
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &s)
	} else {
		k, _ := vault.GenerateKey()
		s = state{VaultID: session.NewVaultID(), Key: hex.EncodeToString(k), DeviceID: session.NewDeviceID()}
	}
	kb, _ := hex.DecodeString(s.Key)
	v, err := vault.New(s.VaultID, kb)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(out, "sm"), v)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = filepath.Join(out, "codex")
	cfg.Agents.WorkBuddy.Home = filepath.Join(out, "wb-home")
	cx := codex.New(cfg)
	e := &engine.Engine{Store: st, DeviceID: s.DeviceID}
	save := func() { b, _ := json.Marshal(s); os.WriteFile(statePath, b, 0600) }

	switch os.Getenv("SM_LIVE_PHASE") {
	case "convert":
		proj := filepath.Join(out, "proj")
		os.MkdirAll(proj, 0700)
		wb := New(cfg)
		id := "0192f3a4-1111-7000-8000-00000000c0de"
		dir := filepath.Join(out, "wb-home", "projects", strings.Trim(strings.ReplaceAll("/wb/source", "/", "-"), "-"))
		os.MkdirAll(dir, 0700)
		body := line(map[string]any{"type": "message", "id": "u1", "sessionId": id, "timestamp": int64(1793000000000), "role": "user", "cwd": "/wb/source",
			"content": []any{map[string]any{"type": "input_text", "text": "<user_query>\nSM_P0_ALPHA please remember this marker\n</user_query>"}}}) +
			line(map[string]any{"type": "message", "id": "a1", "parentId": "u1", "sessionId": id, "timestamp": int64(1793000000001), "role": "assistant", "cwd": "/wb/source",
				"content": []any{map[string]any{"type": "output_text", "text": "SM_P0_BETA noted"}}})
		os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0600)
		b, rev, err := e.Track(ctx, wb, agent.NativeRef{Home: cfg.Agents.WorkBuddy.Home, NativeID: id}, "")
		if err != nil {
			t.Fatal(err)
		}
		_, rec, err := e.ConvertRestore(ctx, wb, cx, b.SessionID, engine.RestoreOptions{Home: cfg.Agents.Codex.Home, Cwd: proj, ClientClosed: true, AllowCodeMismatch: true})
		if err != nil {
			t.Fatal(err)
		}
		s.SessionID, s.Revision, s.NativeID = b.SessionID, rev.RevisionID, rec.NativeSessionID
		save()
	case "capture":
		var bind session.Binding
		bs, _ := st.Bindings(s.SessionID)
		for _, b := range bs {
			if b.AgentProfile.Agent == "codex" && b.State == session.StateActive {
				bind = b
			}
		}
		if bind.NativeSessionID != s.NativeID {
			t.Fatalf("no active converted Codex binding: %+v", bs)
		}
		rev, changed, err := e.Checkpoint(ctx, cx, bind.BindingID, "", session.CaptureCheckpoint)
		if err != nil || !changed {
			t.Fatalf("checkpoint: changed=%v err=%v", changed, err)
		}
		if rev.ParentRevisionID != s.Revision || rev.AgentProfile.Agent != "codex" {
			t.Fatalf("continuation revision wrong: %+v", rev)
		}
		b, _ := json.Marshal(map[string]any{"revision": rev.RevisionID, "parent": rev.ParentRevisionID})
		os.WriteFile(filepath.Join(out, "capture.json"), b, 0600)
	default:
		t.Fatal("SM_LIVE_PHASE must be convert or capture")
	}
}
