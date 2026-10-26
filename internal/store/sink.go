package store

import (
	"context"
	"io"

	"session-memory/internal/agent"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
	"session-memory/internal/vault"
)

// Sink returns the agent.SegmentSink backed by this store.
func (s *FS) Sink() agent.SegmentSink { return segSink{s} }

type segSink struct{ s *FS }

func (k segSink) PutSegment(ctx context.Context, key agent.ArtifactKey, start, end int64, r io.Reader) (session.SegmentRef, error) {
	info, err := k.s.PutObject(ctx, vault.TypeSegment, nil, r)
	if err != nil {
		return session.SegmentRef{}, err
	}
	if info.PlainSize != end-start {
		return session.SegmentRef{}, errcode.New(errcode.Internal, "segment length does not match its range")
	}
	return session.SegmentRef{Start: start, End: end, RawHash: info.PlainSHA256, Object: info.Ref}, nil
}

func (k segSink) PutBlob(ctx context.Context, key agent.ArtifactKey, size int64, r io.Reader) ([]session.SegmentRef, error) {
	var out []session.SegmentRef
	var off int64
	for {
		chunk, err := io.ReadAll(io.LimitReader(r, int64(agent.MaxSegmentBytes)))
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			break
		}
		ref, err := k.PutSegment(ctx, key, off, off+int64(len(chunk)), bytesReader(chunk))
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
		off += int64(len(chunk))
	}
	return out, nil
}
