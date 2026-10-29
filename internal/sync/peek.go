package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"session-memory/internal/session"
	"session-memory/internal/transport"
)

// PeekRevisions lists the revisions that other devices' manifests announce and
// that this device has not imported yet, without importing anything: no
// segment object is downloaded, nothing is written to the store, no manifest
// is marked applied, nothing is quarantined. It reads the manifests exactly as
// Reconcile would (same order, same chain checks, applied ones skipped), and
// stops at a device's first manifest that cannot be read yet, as Reconcile
// does. Together with the revisions already in the store this is every
// revision this device knows the vault to hold, which is what claiming a
// native session needs to know (see engine.Claim).
func (s *Sync) PeekRevisions(ctx context.Context) ([]session.Revision, error) {
	ents, err := s.T.List(ctx, "manifests/")
	if err != nil {
		return nil, err
	}
	byDev := map[string][]transport.Entry{}
	for _, e := range ents {
		parts := strings.Split(e.Path, "/")
		if len(parts) != 3 || !session.ValidDeviceID(parts[1]) || !manifestName.MatchString(parts[2]) {
			continue
		}
		byDev[parts[1]] = append(byDev[parts[1]], e)
	}
	devs := make([]string, 0, len(byDev))
	for d := range byDev {
		if d != s.Info.DeviceID {
			devs = append(devs, d)
		}
	}
	sort.Strings(devs)
	var out []session.Revision
	for _, dev := range devs {
		list := byDev[dev]
		sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
		applied, prevSha := s.appliedState(dev)
		for _, e := range list {
			m := manifestName.FindStringSubmatch(filepath.Base(e.Path))
			seq, err := parseManifestSeq(m[1])
			if err != nil || seq <= applied {
				continue
			}
			if seq != applied+1 {
				break
			}
			mf, _, h, verdict, _, err := s.readManifest(ctx, dev, seq, m[2], e, prevSha, false)
			if err != nil {
				return nil, err
			}
			if verdict != manifestOK {
				break
			}
			for _, en := range mf.Entries {
				if en.Revision != nil {
					out = append(out, en.Revision.Revision)
				}
			}
			applied++
			prevSha = h
		}
	}
	return out, nil
}

// FetchSnapshot makes the snapshot object of revision r present in the store:
// a small object, not its segments, and on Weiyun not the other objects of the
// directory either.
func (s *Sync) FetchSnapshot(ctx context.Context, r session.Revision) error {
	return s.fetchSmall(ctx, r.NativeSnapshotRef)
}

// FetchRepoState does the same for the revision's repo-state object.
func (s *Sync) FetchRepoState(ctx context.Context, r session.Revision) error {
	if r.RepoStateRef == "" {
		return nil
	}
	return s.fetchSmall(ctx, r.RepoStateRef)
}

func (s *Sync) fetchSmall(ctx context.Context, ref string) error {
	if s.Store.HasObject(ref) {
		return nil
	}
	rc, err := transport.FetchOne(ctx, s.T, objectPath(ref))
	if err != nil {
		return fmt.Errorf("fetch %s: %w", ref[:12], err)
	}
	defer rc.Close()
	return s.Store.PutSealed(ctx, ref, rc)
}
