// Package codex is the Codex adapter: discovery of rollout files by thread id,
// safe probing, segment capture, and run-state detection (spec §8.3).
package codex

import (
	"regexp"
	"strings"
)

// AgentName identifies this adapter.
const AgentName = "codex"

// AdapterVersion is recorded in profiles and bindings.
const AdapterVersion = "0.1.0"

// rolloutName is Codex's file name for a rollout (spec §8.3).
var rolloutName = regexp.MustCompile(`^rollout-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(?:_([0-9a-fA-F-]{36}))?\.jsonl(\.zst)?$`)

// parseName returns the lower-case thread id and whether name is a rollout.
func parseName(name string) (thread string, zst bool, ok bool) {
	m := rolloutName.FindStringSubmatch(name)
	if m == nil {
		return "", false, false
	}
	return strings.ToLower(m[1]), m[3] != "", true
}

// logicalName drops the .zst suffix.
func logicalName(name string) string { return strings.TrimSuffix(name, ".zst") }
