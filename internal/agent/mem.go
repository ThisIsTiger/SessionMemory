package agent

import (
	"bytes"
	"context"
	"io"

	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// MemFile is one in-memory native file.
type MemFile struct {
	Path string // logical artifact path (relative to the agent home)
	Kind string // session.KindStream | session.KindBlob
	Mode uint32
	Data []byte
}

// MemSnapshot is a SnapshotReader over in-memory files. Conversion builds the
// target agent's native files in memory and hands them to that agent's native
// Writer as if they were a captured snapshot, so installation reuses the same
// plan, backup, journal and verification path as a native restore.
type MemSnapshot struct{ Files []MemFile }

func (m *MemSnapshot) Artifacts() []session.Artifact {
	out := make([]session.Artifact, 0, len(m.Files))
	for _, f := range m.Files {
		form := ""
		if f.Kind == session.KindStream {
			form = session.FormPlain
		}
		out = append(out, session.Artifact{Path: f.Path, Kind: f.Kind, Form: form, Mode: f.Mode, Size: int64(len(f.Data))})
	}
	return out
}

func (m *MemSnapshot) Open(_ context.Context, path string) (io.ReadCloser, error) {
	for _, f := range m.Files {
		if f.Path == path {
			return io.NopCloser(bytes.NewReader(f.Data)), nil
		}
	}
	return nil, errcode.New(errcode.NotFound, "artifact not in snapshot")
}
