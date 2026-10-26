package session

import (
	"strings"
	"testing"
	"time"
)

func TestIDs(t *testing.T) {
	for _, c := range []struct {
		id string
		ok func(string) bool
		n  int
	}{
		{NewSessionID(), ValidSessionID, 3 + 26},
		{NewDeviceID(), ValidDeviceID, 4 + 16},
		{NewBindingID(), ValidBindingID, 5 + 16},
		{NewTxID(), ValidTxID, 3 + 16},
		{NewRequestID(), ValidRequestID, 4 + 16},
		{NewVaultID(), ValidVaultID, 4 + 26},
		{NewProjectRef(), ValidProjectRef, 4 + 16},
	} {
		if !c.ok(c.id) || len(c.id) != c.n {
			t.Errorf("generated id %q fails its own validator (len %d, want %d)", c.id, len(c.id), c.n)
		}
	}
	if ValidSessionID("ss_"+strings.Repeat("A", 26)) || ValidRevisionID("rev_abc") || ValidObjectRef("obj_"+strings.Repeat("g", 64)) {
		t.Error("validators accept malformed ids")
	}
}

func obj(c byte) string { return "obj_" + strings.Repeat(string(c), 64) }

func baseRevision(parent string, tag byte) Revision {
	r := Revision{
		SchemaVersion:     SchemaVersion,
		SessionID:         "ss_" + strings.Repeat("a", 26),
		ParentRevisionID:  parent,
		ProducerDeviceID:  "dev_" + strings.Repeat("b", 16),
		ProducerBindingID: "bind_" + strings.Repeat("c", 16),
		AgentProfile:      AgentProfile{Agent: "codex", Surface: "desktop", AdapterVersion: "0.1.0", OS: "darwin"},
		NativeSnapshotRef: obj(tag),
		CaptureState:      CaptureAuto,
		CreatedAt:         FormatTime(time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)),
	}
	if err := r.Seal(); err != nil {
		panic(err)
	}
	return r
}

func TestRevisionSealAndValidate(t *testing.T) {
	r := baseRevision("", '1')
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !ValidRevisionID(r.RevisionID) {
		t.Fatalf("bad id %q", r.RevisionID)
	}
	tampered := r
	tampered.CaptureState = CaptureCheckpoint
	if err := tampered.Validate(); err == nil {
		t.Error("a modified revision kept a valid id")
	}
	// the id does not depend on the id field itself
	again := r
	again.RevisionID = ""
	if id, _ := again.ComputeID(); id != r.RevisionID {
		t.Errorf("ComputeID not stable: %s vs %s", id, r.RevisionID)
	}
}

// fixed vector: any implementation must reproduce this id for this input
func TestRevisionIDVector(t *testing.T) {
	r := baseRevision("", '1')
	const want = "rev_cd4821ddbe469526fa3783bf23a2b0514e9d6ae0d6f6eb334df6130b565c34a1"
	if r.RevisionID != want {
		t.Errorf("revision id = %s, want %s", r.RevisionID, want)
	}
}

func graph(revs ...Revision) *Graph { return NewGraph(revs) }

func TestHeadsAndAncestry(t *testing.T) {
	r1 := baseRevision("", '1')
	r2 := baseRevision(r1.RevisionID, '2')
	r3 := baseRevision(r2.RevisionID, '3')
	g := graph(r1, r2, r3)
	if h := g.Heads(); len(h) != 1 || h[0] != r3.RevisionID {
		t.Errorf("heads = %v", h)
	}
	if !g.IsAncestorOrEqual(r1.RevisionID, r3.RevisionID) || g.IsAncestorOrEqual(r3.RevisionID, r1.RevisionID) {
		t.Error("ancestry wrong")
	}
	if !g.IsAncestorOrEqual(r2.RevisionID, r2.RevisionID) {
		t.Error("equal must count as ancestor-or-equal")
	}
}

func TestOrphansArePending(t *testing.T) {
	r1 := baseRevision("", '1')
	r2 := baseRevision(r1.RevisionID, '2')
	r3 := baseRevision(r2.RevisionID, '3')
	g := graph(r1, r3) // r2 has not arrived
	if len(g.Pending) != 1 || g.Pending[0] != r3.RevisionID {
		t.Errorf("pending = %v", g.Pending)
	}
	if h := g.Heads(); len(h) != 1 || h[0] != r1.RevisionID {
		t.Errorf("an orphan must not become a head: %v", h)
	}
}

// A5: two devices continue the same revision
func TestConflictAndChoiceScenarios(t *testing.T) {
	r1 := baseRevision("", '1')
	r2 := baseRevision(r1.RevisionID, '2')
	a := baseRevision(r2.RevisionID, 'a') // company
	b := baseRevision(r2.RevisionID, 'b') // home

	g := graph(r1, r2, a, b)
	if res := Resolve(g, nil); !res.Conflict || len(res.Heads) != 2 {
		t.Fatalf("A5: want conflict, got %+v", res)
	}

	choose := func(chosen string, seen ...string) Choice {
		return Choice{SessionID: r1.SessionID, SeenHeads: seen, Chosen: chosen, DeviceID: a.ProducerDeviceID, At: "t"}
	}
	c1 := choose(a.RevisionID, a.RevisionID, b.RevisionID)
	if res := Resolve(g, []Choice{c1}); res.Conflict || res.Effective != a.RevisionID {
		t.Fatalf("choose a: %+v", res)
	}

	// the chosen branch continues: still resolved
	a2 := baseRevision(a.RevisionID, 'x')
	g2 := graph(r1, r2, a, b, a2)
	if res := Resolve(g2, []Choice{c1}); res.Conflict || res.Effective != a2.RevisionID {
		t.Fatalf("chosen branch continued: %+v", res)
	}

	// A6: the rejected branch grows after the choice: conflict again
	b2 := baseRevision(b.RevisionID, 'y')
	g3 := graph(r1, r2, a, b, b2)
	if res := Resolve(g3, []Choice{c1}); !res.Conflict {
		t.Fatalf("A6: a new descendant of the rejected head must reopen the conflict: %+v", res)
	}
	// choosing again over the new heads resolves it
	c2 := choose(a.RevisionID, a.RevisionID, b2.RevisionID)
	if res := Resolve(g3, []Choice{c1, c2}); res.Conflict || res.Effective != a.RevisionID {
		t.Fatalf("A6: second choice: %+v", res)
	}

	// two devices choose different branches: inconsistent
	cb := choose(b.RevisionID, a.RevisionID, b.RevisionID)
	if res := Resolve(g, []Choice{c1, cb}); !res.Conflict {
		t.Fatalf("A6: inconsistent choices must conflict: %+v", res)
	}

	// a choice naming a revision we do not have yet is ignored
	unknown := choose(a.RevisionID, a.RevisionID, "rev_"+strings.Repeat("0", 64))
	if res := Resolve(g, []Choice{unknown}); !res.Conflict {
		t.Fatalf("a choice with unknown revisions must not resolve: %+v", res)
	}

	// the chosen branch forks again: new conflict even with the old choice
	a3 := baseRevision(a.RevisionID, 'p')
	a4 := baseRevision(a.RevisionID, 'q')
	g4 := graph(r1, r2, a, b, a3, a4)
	if res := Resolve(g4, []Choice{c1}); !res.Conflict {
		t.Fatalf("fork after choice: %+v", res)
	}
}

// A13: landing after a choice
func TestLanding(t *testing.T) {
	r1 := baseRevision("", '1')
	r2 := baseRevision(r1.RevisionID, '2')
	a := baseRevision(r2.RevisionID, 'a')
	b := baseRevision(r2.RevisionID, 'b')
	g := graph(r1, r2, a, b)
	for _, c := range []struct {
		installed, effective string
		want                 Landing
	}{
		{a.RevisionID, a.RevisionID, LandUpToDate},
		{r2.RevisionID, a.RevisionID, LandFastForward},
		{r1.RevisionID, a.RevisionID, LandFastForward},
		{b.RevisionID, a.RevisionID, LandNeedsAction}, // home installed the rejected branch
		{"", a.RevisionID, LandFastForward},
		{"rev_" + strings.Repeat("9", 64), a.RevisionID, LandNeedsAction},
	} {
		if got := LandingFor(g, c.installed, c.effective); got != c.want {
			t.Errorf("installed %.12s effective %.12s: got %v want %v", c.installed, c.effective, got, c.want)
		}
	}
}

// The example in spec.md §2.3: any implementation must reproduce these ids.
func TestSpecExampleVectors(t *testing.T) {
	r := Revision{
		SchemaVersion: 1, SessionID: "ss_" + strings.Repeat("a", 26), ProjectRef: "prj_" + strings.Repeat("d", 16),
		OriginNativeID:    "0192f3a4-0000-7000-8000-000000000001",
		ProducerDeviceID:  "dev_" + strings.Repeat("b", 16),
		ProducerBindingID: "bind_" + strings.Repeat("c", 16),
		AgentProfile:      AgentProfile{Agent: "codex", Surface: "desktop", Version: "0.150.0", Format: "rollout-jsonl", AdapterVersion: "0.1.0", OS: "darwin"},
		NativeSnapshotRef: "obj_" + strings.Repeat("e", 64), CaptureState: CaptureCheckpoint,
		CreatedAt: FormatTime(time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)),
	}
	if err := r.Seal(); err != nil {
		t.Fatal(err)
	}
	if r.RevisionID != "rev_bf5fc65f428edc2e61f0ac445f942553580481f1c0702f63906382b6d571b932" {
		t.Errorf("revision 1 id = %s", r.RevisionID)
	}
	r2 := r
	r2.ParentRevisionID, r2.OriginNativeID, r2.RevisionID = r.RevisionID, "", ""
	r2.CaptureState = CaptureAuto
	r2.CreatedAt = FormatTime(time.Date(2026, 10, 26, 10, 5, 0, 0, time.UTC))
	if err := r2.Seal(); err != nil {
		t.Fatal(err)
	}
	if r2.RevisionID != "rev_aa573d28dff0d2a6e8204a85e59033d4e8028c4b3cfb5f579a79c377c57d79a5" {
		t.Errorf("revision 2 id = %s", r2.RevisionID)
	}
	segs := []SegmentRef{
		{Start: 0, End: 100, RawHash: strings.Repeat("1", 64)},
		{Start: 100, End: 250, RawHash: strings.Repeat("3", 64)},
	}
	if d := SegmentsDigest(segs); d != "709d39a7ffa1ba4ab5e4176518c8b809d838815b55e5305b6fb58f4e2a37fb19" {
		t.Errorf("segments digest = %s", d)
	}
	if got := ManifestFileName(1, strings.Repeat("ab", 32)); got != "000000000001-abababababab.manifest" {
		t.Errorf("manifest name = %s", got)
	}
}
