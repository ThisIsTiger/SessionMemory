package engine

import (
	"testing"

	"session-memory/internal/canonjson"
	"session-memory/internal/session"
)

// A zero-byte file (a tool-result a client left empty) has no segments; the
// snapshot must still be hashable.
func TestSnapshotOfAnEmptyFileCanBeHashed(t *testing.T) {
	st := session.ArtifactState{Kind: session.KindBlob, Mode: 0644, Digest: session.SegmentsDigest(nil)}
	snap := session.Snapshot{Artifacts: []session.Artifact{snapshotArtifact("projects/x/tool-results/empty.txt", 0, st)}}
	if _, err := canonjson.Marshal(snap); err != nil {
		t.Fatalf("an empty file made the snapshot unhashable: %v", err)
	}
}
