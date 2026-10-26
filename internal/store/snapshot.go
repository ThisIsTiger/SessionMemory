package store

import (
	"context"
	"io"

	"session-memory/internal/agent"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
	"session-memory/internal/vault"
)

type snapReader struct {
	s    *FS
	snap session.Snapshot
}

// SnapshotReader returns read access to the files of a revision's snapshot.
func (s *FS) SnapshotReader(ctx context.Context, snapshotRef string) (agent.SnapshotReader, error) {
	snap, err := s.LoadSnapshot(ctx, snapshotRef)
	if err != nil {
		return nil, err
	}
	if err := snap.Validate(); err != nil {
		return nil, errcode.Wrap(errcode.UnsafeRecord, "invalid snapshot", err)
	}
	return &snapReader{s: s, snap: snap}, nil
}

func (r *snapReader) Artifacts() []session.Artifact { return r.snap.Artifacts }

func (r *snapReader) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	for _, a := range r.snap.Artifacts {
		if a.Path == path {
			return &segReader{ctx: ctx, s: r.s, segs: a.Segments}, nil
		}
	}
	return nil, errcode.New(errcode.NotFound, "artifact not in snapshot")
}

// segReader concatenates segment objects lazily.
type segReader struct {
	ctx  context.Context
	s    *FS
	segs []session.SegmentRef
	cur  io.ReadCloser
}

func (r *segReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			if len(r.segs) == 0 {
				return 0, io.EOF
			}
			rc, err := r.s.OpenObject(r.ctx, r.segs[0].Object, vault.TypeSegment, nil)
			if err != nil {
				return 0, err
			}
			r.cur, r.segs = rc, r.segs[1:]
		}
		n, err := r.cur.Read(p)
		if err == io.EOF {
			r.cur.Close()
			r.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (r *segReader) Close() error {
	if r.cur != nil {
		return r.cur.Close()
	}
	return nil
}
