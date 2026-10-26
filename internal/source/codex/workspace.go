package codex

import (
	"context"
	"encoding/json"
	"session-memory/internal/agent"
	"session-memory/internal/errcode"
	"session-memory/internal/jsonl"
)

// Workspace reads only the structural cwd of a captured session, never
// inferring a project path from a filename or a historical tool argument.
func (a *Adapter) Workspace(ctx context.Context, snap agent.SnapshotReader) (string, error) {
	for _, art := range snap.Artifacts() {
		rc, err := snap.Open(ctx, art.Path)
		if err != nil {
			return "", err
		}
		r := jsonl.NewReader(rc, 0, jsonl.Options{MaxRecordBytes: a.maxRecord()})
		rec, err := r.Next()
		r.Close()
		if err != nil {
			return "", err
		}
		var env struct {
			Type    string      `json:"type"`
			Payload metaPayload `json:"payload"`
		}
		if json.Unmarshal(rec.Line, &env) == nil && env.Type == "session_meta" && env.Payload.Cwd != "" {
			return env.Payload.Cwd, nil
		}
	}
	return "", errcode.New(errcode.NeedsAction, "snapshot does not identify a project directory; select --cwd")
}
