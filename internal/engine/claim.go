package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"

	"session-memory/internal/agent"
	"session-memory/internal/errcode"
	"session-memory/internal/failpoint"
	"session-memory/internal/session"
	"session-memory/internal/store"
)

// ClaimIndex tells Claim about vault sessions that are not in the local store
// (yet): after a reset or a reinstall the store is empty although the vault
// holds this computer's sessions. Both fields are optional; without them Claim
// only knows what the store holds.
type ClaimIndex struct {
	// Load returns the revisions that other devices' manifests announce and the
	// store has not imported. It is called at most once, when the first native
	// session needs it, so a setup with nothing to take on reads nothing.
	Load func(context.Context) ([]session.Revision, error)
	// FetchSnapshot makes a revision's snapshot object present in the store (a
	// small object, not its segments); FetchState does the same for its repo
	// state, which is only needed for the revision that is finally linked.
	FetchSnapshot func(context.Context, session.Revision) error
	FetchState    func(context.Context, session.Revision) error

	loaded bool
	revs   []session.Revision
}

func (ix *ClaimIndex) load(ctx context.Context) ([]session.Revision, error) {
	if ix == nil || ix.Load == nil {
		return nil, nil
	}
	if !ix.loaded {
		revs, err := ix.Load(ctx)
		if err != nil {
			return nil, err
		}
		ix.revs, ix.loaded = revs, true
	}
	return ix.revs, nil
}

// bindingVerifier is what Claim needs of an adapter beyond agent.Adapter: a deep
// check of the native files against a captured state (both adapters have it).
type bindingVerifier interface {
	VerifyBinding(ctx context.Context, ref agent.NativeRef, arts map[string]session.ArtifactState, deep bool) ([]string, error)
}

// maxClaimProbes bounds how many revisions of one session are compared with a
// native session, newest first; each may cost one small snapshot download.
const maxClaimProbes = 12

// Claim links a native session that is already in the vault to its vault
// session instead of tracking it as a new one. It is for a computer whose local
// state was lost (reset, reinstall) while the vault kept the sessions it
// published, or installed on it from another computer.
//
// A candidate is a vault session of the same agent whose first revision records
// this native session id and that has no active binding here. It is claimed only
// when the native files hold EXACTLY the content of one of its revisions R: the
// same set of artifacts, the same sizes, and every segment's raw hash recomputed
// over the native bytes with R's own segment boundaries (segmentation is not
// deterministic, so the digests of a fresh capture mean nothing). The newest
// matching revision wins; R need not be the head, a later relay fast-forwards.
// The native id alone never claims anything, and native files are only read.
//
// On a match the binding is created at R exactly as an install leaves it
// (cursors from a capture of the files, base and installed revision R), no
// revision is made and nothing is uploaded except one "installed" receipt.
// claimed=false (and a nil error) means nothing matched: the caller tracks the
// session as a new one with Track, which still creates a second vault session
// when the native has progress the vault lacks.
func (e *Engine) Claim(ctx context.Context, ad agent.Adapter, ref agent.NativeRef, ix *ClaimIndex) (b session.Binding, rev session.Revision, claimed bool, err error) {
	verifier, ok := ad.(bindingVerifier)
	if !ok {
		return b, rev, false, nil
	}
	ns, found, err := ad.Locate(ctx, ref)
	if err != nil {
		return b, rev, false, err
	}
	if !found {
		return b, rev, false, errcode.New(errcode.NotFound, "native session not found")
	}
	all, err := e.Store.Bindings("")
	if err != nil {
		return b, rev, false, err
	}
	bound := map[string]bool{} // sessions with an active binding of this agent here
	for _, x := range all {
		if x.AgentProfile.Agent == ns.Profile.Agent && x.NativeSessionID == ns.Ref.NativeID && x.NativeHome == ref.Home {
			return b, rev, false, nil // already tracked: Track says so
		}
		if x.State == session.StateActive && x.AgentProfile.Agent == ad.Name() {
			bound[x.SessionID] = true
		}
	}
	known, err := e.Store.RevisionsOfAgent(ad.Name())
	if err != nil {
		return b, rev, false, err
	}
	extra, err := ix.load(ctx)
	if err != nil {
		return b, rev, false, err
	}
	cands := claimCandidates(append(known, extra...), ad.Name(), ns.Ref.NativeID, bound, e.DeviceID)
	if len(cands) == 0 {
		return b, rev, false, nil
	}

	// what the native files hold, read once and not stored
	probe, err := ad.Capture(ctx, ref, agent.Cursor{}, discardSink{})
	if err != nil {
		return b, rev, false, err
	}
	if !exactCapture(probe) {
		return b, rev, false, nil
	}

	for _, c := range cands {
		for i, r := range c.revs {
			if i >= maxClaimProbes {
				break
			}
			snap, ok := e.claimSnapshot(ctx, r, ix)
			if i == 0 && ok && c.singleHead && r.ReplacesRevisionID == "" && beyond(snap, probe.Cursor.Artifacts) {
				break // the files hold more than the newest revision of a plain history: none of its ancestors can match
			}
			if !ok || !sameShape(snap, probe.Cursor.Artifacts) {
				continue
			}
			bad, err := verifier.VerifyBinding(ctx, ref, statesOf(snap), true)
			if err != nil {
				return b, rev, false, err
			}
			if len(bad) > 0 {
				continue
			}
			b, err = e.bindClaimed(ctx, ad, ref, ns, c, r, snap, ix)
			if err != nil {
				return b, rev, false, err
			}
			if b.BindingID == "" {
				continue // the files changed while they were being linked
			}
			return b, r, true, nil
		}
	}
	return b, rev, false, nil
}

// claimCandidate is a vault session that may be the native session's: its
// revisions newest first, with its root.
type claimCandidate struct {
	sessionID  string
	root       session.Revision
	revs       []session.Revision
	singleHead bool // the history has one head, the newest revision
}

// claimCandidates groups the revisions by session and keeps the sessions of
// this agent whose root revision recorded nativeID, oldest session first (a
// duplicate made by an earlier setup is younger than the original). Revisions
// this device produced itself are left out: they are in the store with their
// manifest entries or not published, never "in the vault only".
func claimCandidates(revs []session.Revision, agentName, nativeID string, bound map[string]bool, self string) []claimCandidate {
	by := map[string]*claimCandidate{}
	seen := map[string]bool{}
	for _, r := range revs {
		if seen[r.RevisionID] || r.AgentProfile.Agent != agentName || r.ProducerDeviceID == self {
			continue
		}
		seen[r.RevisionID] = true
		c := by[r.SessionID]
		if c == nil {
			c = &claimCandidate{sessionID: r.SessionID}
			by[r.SessionID] = c
		}
		c.revs = append(c.revs, r)
		if r.ParentRevisionID == "" {
			c.root = r
		}
	}
	var out []claimCandidate
	for _, c := range by {
		if bound[c.sessionID] || c.root.RevisionID == "" || !strings.EqualFold(c.root.OriginNativeID, nativeID) {
			continue
		}
		sort.Slice(c.revs, func(i, j int) bool {
			if c.revs[i].CreatedAt != c.revs[j].CreatedAt {
				return c.revs[i].CreatedAt > c.revs[j].CreatedAt
			}
			return c.revs[i].RevisionID > c.revs[j].RevisionID
		})
		parents := map[string]bool{}
		for _, r := range c.revs {
			parents[r.ParentRevisionID] = true
		}
		heads := 0
		for _, r := range c.revs {
			if !parents[r.RevisionID] {
				heads++
			}
		}
		c.singleHead = heads == 1 && !parents[c.revs[0].RevisionID]
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].root.CreatedAt != out[j].root.CreatedAt {
			return out[i].root.CreatedAt < out[j].root.CreatedAt
		}
		return out[i].sessionID < out[j].sessionID
	})
	return out
}

// claimSnapshot loads the snapshot of r, fetching it first when the store lacks
// it and a fetcher is at hand.
func (e *Engine) claimSnapshot(ctx context.Context, r session.Revision, ix *ClaimIndex) (session.Snapshot, bool) {
	if !e.Store.HasObject(r.NativeSnapshotRef) {
		if ix == nil || ix.FetchSnapshot == nil || ix.FetchSnapshot(ctx, r) != nil {
			return session.Snapshot{}, false
		}
	}
	snap, err := e.Store.LoadSnapshot(ctx, r.NativeSnapshotRef)
	if err != nil || snap.Validate() != nil {
		return session.Snapshot{}, false
	}
	return snap, true
}

// exactCapture reports whether a capture of the native files saw every byte of
// them as complete records: a partial last line, a corrupt record or a file
// that vanished all rule out an exact match.
func exactCapture(res agent.CaptureResult) bool {
	return res.State == agent.CaptureOK && len(res.Diags) == 0 && len(res.Cursor.Artifacts) > 0
}

// beyond reports whether the native files hold more than the snapshot: a file it
// lacks, or a stream longer than its own.
func beyond(snap session.Snapshot, got map[string]session.ArtifactState) bool {
	have := map[string]session.Artifact{}
	for _, a := range snap.Artifacts {
		have[a.Path] = a
	}
	for path, st := range got {
		a, ok := have[path]
		if !ok || (st.Kind == session.KindStream && st.Size > a.Size) {
			return true
		}
	}
	return false
}

// sameShape compares the artifact set and sizes of a snapshot with a capture.
func sameShape(snap session.Snapshot, got map[string]session.ArtifactState) bool {
	if len(snap.Artifacts) != len(got) {
		return false
	}
	for _, a := range snap.Artifacts {
		st, ok := got[a.Path]
		if !ok || st.Kind != a.Kind || st.Size != a.Size {
			return false
		}
	}
	return true
}

// statesOf turns a snapshot into the captured state VerifyBinding checks the
// files against: R's own segments, hence R's own boundaries.
func statesOf(snap session.Snapshot) map[string]session.ArtifactState {
	out := map[string]session.ArtifactState{}
	for _, a := range snap.Artifacts {
		out[a.Path] = session.ArtifactState{Kind: a.Kind, Form: a.Form, Mode: a.Mode, Segments: a.Segments, Size: a.Size, Digest: a.Digest}
	}
	return out
}

// bindClaimed makes the binding for a verified match: the files are captured
// for the cursors (into the local store only; nothing is published), the
// revision is recorded in the store when it came from a manifest not imported
// yet, and the binding is saved in one atomic file write. A zero binding means
// the files changed since the check, so the caller must not link them.
func (e *Engine) bindClaimed(ctx context.Context, ad agent.Adapter, ref agent.NativeRef, ns agent.NativeSession, c claimCandidate, r session.Revision, snap session.Snapshot, ix *ClaimIndex) (session.Binding, error) {
	tx, err := e.Store.Begin(ctx, store.KindCapture, c.sessionID, "")
	if err != nil {
		return session.Binding{}, err
	}
	adopted, err := ad.Capture(ctx, ref, agent.Cursor{}, e.Store.Sink())
	if err != nil {
		tx.Finish()
		return session.Binding{}, err
	}
	if !exactCapture(adopted) || !sameShape(snap, adopted.Cursor.Artifacts) {
		return session.Binding{}, tx.Finish()
	}
	if _, err := e.Store.LoadRevision(r.RevisionID); err != nil {
		if ix != nil && ix.FetchState != nil {
			if err := ix.FetchState(ctx, r); err != nil {
				tx.Finish()
				return session.Binding{}, err
			}
		}
		adopt := []session.Revision{r}
		if c.root.RevisionID != r.RevisionID {
			adopt = []session.Revision{c.root, r} // the session's row is made from the first
		}
		if err := e.Store.AdoptRevisions(adopt...); err != nil {
			tx.Finish()
			return session.Binding{}, err
		}
	}
	if err := tx.SetState(store.StStaged, nil); err != nil {
		return session.Binding{}, err
	}
	b := session.Binding{
		BindingID: session.NewBindingID(), SessionID: c.sessionID, DeviceID: e.DeviceID,
		AgentProfile: ns.Profile, NativeSessionID: ns.Ref.NativeID, NativeHome: ref.Home,
		InstallMode: session.InstallNative, State: session.StateActive, Artifacts: adopted.Cursor.Artifacts,
		LocalCwd: ns.Cwd, BaseRevisionID: r.RevisionID, InstalledRevisionID: r.RevisionID,
	}
	if err := e.Store.SaveBinding(tx, b); err != nil { // one atomic file write: no half-made binding
		tx.Finish()
		return session.Binding{}, err
	}
	if err := failpoint.Hit("claim.after_binding"); err != nil {
		return b, err
	}
	if err := (&deviceOutbox{root: e.Store.Root}).add(
		session.Entry{Type: session.EntryReceipt, Receipt: &session.ReceiptEntry{SessionID: c.sessionID, RevisionID: r.RevisionID, DeviceID: e.DeviceID,
			Status: session.ReceiptInstalled, InstallMode: session.InstallNative, BindingID: b.BindingID, At: session.FormatTime(e.now())}},
	); err != nil {
		return b, err
	}
	return b, tx.Finish()
}

// discardSink hashes what a capture delivers and keeps nothing: Claim reads the
// native files this way before it knows whether it will link them.
type discardSink struct{}

func (discardSink) PutSegment(_ context.Context, _ agent.ArtifactKey, start, end int64, r io.Reader) (session.SegmentRef, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return session.SegmentRef{}, err
	}
	if n != end-start {
		return session.SegmentRef{}, errcode.New(errcode.Internal, "segment length does not match its range")
	}
	return session.SegmentRef{Start: start, End: end, RawHash: hex.EncodeToString(h.Sum(nil))}, nil
}

func (s discardSink) PutBlob(ctx context.Context, k agent.ArtifactKey, size int64, r io.Reader) ([]session.SegmentRef, error) {
	var out []session.SegmentRef
	var off int64
	for {
		chunk, err := io.ReadAll(io.LimitReader(r, int64(agent.MaxSegmentBytes)))
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			return out, nil
		}
		ref, err := s.PutSegment(ctx, k, off, off+int64(len(chunk)), bytes.NewReader(chunk))
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
		off += int64(len(chunk))
	}
}
