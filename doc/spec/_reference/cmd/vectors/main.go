package main

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"session-memory/internal/canonjson"
	"session-memory/internal/session"
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func main() {
	seg := func(start, end int64, c byte) session.SegmentRef {
		return session.SegmentRef{Start: start, End: end, RawHash: strings.Repeat(string(c), 64), Object: "obj_" + strings.Repeat(string(c+1), 64)}
	}
	segs := []session.SegmentRef{seg(0, 100, '1'), seg(100, 250, '3')}
	fmt.Println("SEGMENTS_DIGEST", session.SegmentsDigest(segs))
	art := session.Artifact{Path: "sessions/2026/10/26/rollout-2026-10-26T10-00-00-0192f3a4-0000-7000-8000-000000000001.jsonl",
		Kind: session.KindStream, Form: session.FormZstd, Generation: 0, Mode: 0o600, Size: 250, Digest: session.SegmentsDigest(segs), Segments: segs}
	snap := session.Snapshot{SchemaVersion: 1, Artifacts: []session.Artifact{art}}
	fmt.Println("SNAPSHOT_JSON", string(must(canonjson.Marshal(snap))))

	r := session.Revision{
		SchemaVersion: 1, SessionID: "ss_" + strings.Repeat("a", 26), ProjectRef: "prj_" + strings.Repeat("d", 16),
		OriginNativeID:   "0192f3a4-0000-7000-8000-000000000001",
		ProducerDeviceID: "dev_" + strings.Repeat("b", 16), ProducerBindingID: "bind_" + strings.Repeat("c", 16),
		AgentProfile:      session.AgentProfile{Agent: "codex", Surface: "desktop", Version: "0.150.0", Format: "rollout-jsonl", AdapterVersion: "0.1.0", OS: "darwin"},
		NativeSnapshotRef: "obj_" + strings.Repeat("e", 64), CaptureState: session.CaptureCheckpoint,
		CreatedAt: session.FormatTime(time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)),
	}
	must(0, r.Seal())
	fmt.Println("REVISION_JSON", string(must(canonjson.Marshal(r))))
	fmt.Println("REVISION_ID", r.RevisionID)

	rev2 := r
	rev2.ParentRevisionID = r.RevisionID
	rev2.OriginNativeID = ""
	rev2.RevisionID = ""
	rev2.CaptureState = session.CaptureAuto
	rev2.CreatedAt = session.FormatTime(time.Date(2026, 10, 26, 10, 5, 0, 0, time.UTC))
	must(0, rev2.Seal())
	fmt.Println("REVISION2_ID", rev2.RevisionID)

	m := session.Manifest{SchemaVersion: 1, DeviceID: r.ProducerDeviceID, Sequence: 1, CreatedAt: "2026-10-26T10:00:01.000Z",
		Entries: []session.Entry{{Type: session.EntryRevision, Revision: &session.RevisionEntry{Revision: r, Objects: []string{"obj_" + strings.Repeat("1", 64), "obj_" + strings.Repeat("e", 64)}}}}}
	for _, e := range m.Entries {
		if err := e.Validate(); err != nil {
			panic(err)
		}
	}
	fmt.Println("MANIFEST_JSON", string(must(canonjson.Marshal(m))))
	fmt.Println("MANIFEST_NAME", session.ManifestFileName(1, strings.Repeat("ab", 32)))
	fmt.Println("HEX_EXAMPLE", hex.EncodeToString([]byte("ok")))
}
