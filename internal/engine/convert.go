package engine

import (
	"context"

	"session-memory/internal/agent"
	"session-memory/internal/canonical"
	"session-memory/internal/convert"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// ToolMaps lists, per TARGET agent, the origin tool names that may be written
// as native tool records (spec §9.2).
var ToolMaps = map[string]map[string]string{
	// Empty on purpose: a native tool call needs both the target's tool name and
	// its argument shape, and neither has been verified for a foreign origin
	// (experiments E3/E4). Until then every tool exchange is rendered as text,
	// which keeps the history readable to the target model.
	"codex":     {},
	"workbuddy": {},
}

// RevisionItems extracts the Canonical items of a revision on this device.
func (e *Engine) RevisionItems(ctx context.Context, ex agent.Extractor, rev session.Revision) ([]canonical.Item, error) {
	src, err := e.Store.SnapshotReader(ctx, rev.NativeSnapshotRef)
	if err != nil {
		return nil, err
	}
	var items []canonical.Item
	if err := ex.Extract(ctx, src, func(it canonical.Item) error { items = append(items, it); return nil }); err != nil {
		return nil, err
	}
	return items, nil
}

// ConversionReport converts a revision's canonical items for a target agent and
// returns the output and its fidelity report. Nothing is written anywhere.
func (e *Engine) ConversionReport(ctx context.Context, ex agent.Extractor, rev session.Revision, target string) (convert.Output, error) {
	if _, ok := ToolMaps[target]; !ok {
		return convert.Output{}, errcode.New(errcode.Unsupported, "no conversion rules for target "+target)
	}
	items, err := e.RevisionItems(ctx, ex, rev)
	if err != nil {
		return convert.Output{}, err
	}
	out, err := convert.Convert(items, convert.Options{ToolMap: ToolMaps[target]})
	if err != nil {
		return out, err
	}
	out.Report.DataEgress = true
	return out, nil
}
