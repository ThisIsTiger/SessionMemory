package codex

import (
	"bytes"
	"context"
	"io"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/canonical"
	"session-memory/internal/session"
	"session-memory/internal/testutil/codexgen"
)

type bufSnap struct{ data []byte }

func (b bufSnap) Artifacts() []session.Artifact {
	return []session.Artifact{{Path: "sessions/2026/09/01/rollout-x.jsonl", Kind: session.KindStream}}
}
func (b bufSnap) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.data)), nil
}

var _ agent.SnapshotReader = bufSnap{}

func TestExtractLinksUILayerAndMarksHumanInput(t *testing.T) {
	data := codexgen.Rollout(codexgen.Options{Seed: 1, Turns: 2, ToolCalls: 1, Ordinals: true, Compacted: true, Images: true})
	var items []canonical.Item
	if err := New(configDefault()).Extract(context.Background(), bufSnap{data}, func(it canonical.Item) error { items = append(items, it); return nil }); err != nil {
		t.Fatal(err)
	}
	if iss := canonical.Validate(items); len(iss) != 0 {
		t.Fatalf("validation issues: %v", iss)
	}
	var users, assistants, calls, results, reasoning, compaction int
	for _, it := range items {
		switch {
		case it.Kind == canonical.KindMessage && it.Role == canonical.RoleUser && it.Human:
			users++
			if len(it.Links) == 0 && it.Norm["text"] != "" && it.Norm["images"] == nil {
				t.Errorf("event_msg user_message not linked to %s", it.ID)
			}
		case it.Kind == canonical.KindMessage && it.Role == canonical.RoleAssistant:
			assistants++
			if len(it.Links) != 1 {
				t.Errorf("assistant links = %d", len(it.Links))
			}
		case it.Kind == canonical.KindToolCall:
			calls++
		case it.Kind == canonical.KindToolResult:
			results++
		case it.Kind == canonical.KindReasoning:
			reasoning++
		case it.Kind == canonical.KindCompaction:
			compaction++
			if r, _ := it.Norm["readable"].(bool); !r {
				t.Error("a compacted record with a message must be readable")
			}
		}
		if it.ID == "" || it.Raw.Length == 0 {
			t.Errorf("item without id or raw ref: %+v", it)
		}
	}
	if users != 3 || assistants != 2 || calls != 2 || results != 2 || reasoning != 2 || compaction != 1 {
		t.Errorf("users=%d assistants=%d calls=%d results=%d reasoning=%d compaction=%d", users, assistants, calls, results, reasoning, compaction)
	}
	if items[0].Kind != canonical.KindSessionMeta || items[1].ID == "" {
		t.Error("first items wrong")
	}
	// ids come from ordinals, so they are stable across runs
	var again []canonical.Item
	New(configDefault()).Extract(context.Background(), bufSnap{data}, func(it canonical.Item) error { again = append(again, it); return nil })
	for i := range items {
		if items[i].ID != again[i].ID {
			t.Fatal("ids are not stable")
		}
	}
	if items[1].ID != "o1" {
		t.Errorf("ordinal id = %s", items[1].ID)
	}
}
