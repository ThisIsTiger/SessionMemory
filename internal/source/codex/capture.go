package codex

import (
	"context"
	"sort"
	"strings"

	"session-memory/internal/agent"
	"session-memory/internal/jsonl"
	"session-memory/internal/session"
	"session-memory/internal/streamcap"
)

func (a *Adapter) maxRecord() int64 {
	if n := a.Cfg.Agents.Codex.MaxRecordBytes; n > 0 {
		return n
	}
	return jsonl.DefaultMaxRecordBytes
}

func toStream(fs []file) []streamcap.File {
	out := make([]streamcap.File, len(fs))
	for i, f := range fs {
		out[i] = streamcap.File{Rel: f.rel, Zst: f.zst, Mode: f.mode}
	}
	return out
}

// Capture streams new bytes of every rollout of the thread to sink (spec §8.2).
func (a *Adapter) Capture(ctx context.Context, ref agent.NativeRef, cur agent.Cursor, sink agent.SegmentSink) (agent.CaptureResult, error) {
	threads, _ := threadFiles(ref.Home)
	files := threads[strings.ToLower(ref.NativeID)]
	res := agent.CaptureResult{State: agent.CaptureOK, Cursor: agent.Cursor{Artifacts: map[string]session.ArtifactState{}}}
	if len(files) == 0 {
		res.State = agent.CaptureDetached
		return res, nil
	}
	byLogic := map[string][]file{}
	var logics []string
	for _, f := range files {
		if _, ok := byLogic[f.logic]; !ok {
			logics = append(logics, f.logic)
		}
		byLogic[f.logic] = append(byLogic[f.logic], f)
	}
	sort.Strings(logics)
	for _, logic := range logics {
		st, state, changed, diags, err := streamcap.CaptureStream(ctx, ref.Home, logic, toStream(byLogic[logic]), cur.Artifacts[logic], sink,
			streamcap.Options{MaxRecordBytes: a.maxRecord()})
		if err != nil {
			return res, err
		}
		res.Diags = append(res.Diags, diags...)
		res.Changed = res.Changed || changed
		if st != nil {
			res.Cursor.Artifacts[logic] = *st
		} else if old, ok := cur.Artifacts[logic]; ok {
			res.Cursor.Artifacts[logic] = old
		}
		if state != agent.CaptureOK && (res.State == agent.CaptureOK || state == agent.CaptureUnsafe) {
			res.State = state
		}
	}
	return res, nil
}

// VerifyBinding checks captured artifacts against the files on disk.
func (a *Adapter) VerifyBinding(ctx context.Context, ref agent.NativeRef, arts map[string]session.ArtifactState, deep bool) ([]string, error) {
	threads, _ := threadFiles(ref.Home)
	byLogic := map[string][]streamcap.File{}
	for _, f := range threads[strings.ToLower(ref.NativeID)] {
		byLogic[f.logic] = append(byLogic[f.logic], streamcap.File{Rel: f.rel, Zst: f.zst, Mode: f.mode})
	}
	bad, err := streamcap.VerifyArtifacts(ref.Home, byLogic, arts, deep)
	sort.Strings(bad)
	return bad, err
}
