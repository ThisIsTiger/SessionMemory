// Package jsonl additions: Source hides whether a rollout is stored plain or
// zstd-compressed. Offsets, cursors and hashes are always over the
// DECOMPRESSED stream (design §8.2).
package jsonl

import "io"

// Form is the on-disk form of a stream.
type Form string

const (
	FormPlain Form = "plain"
	FormZstd  Form = "zstd"
)

// Source opens a stream at a decompressed byte offset.
type Source interface {
	Form() Form
	// OpenAt returns a reader positioned at decompressed offset off. For a
	// plain file this seeks; for zstd it decompresses from the start and
	// discards off bytes (zstd files have no random access), so callers that
	// verify the prefix hash the discarded bytes at the same time via the
	// returned prefix hasher.
	OpenAt(off int64) (io.ReadCloser, PrefixHasher, error)
	// Size is the decompressed size when known cheaply (plain: file size);
	// -1 for zstd until read to the end.
	Size() int64
}

// PrefixHasher is fed the bytes OpenAt skipped, so a caller can verify the
// consumed prefix while positioning.
type PrefixHasher interface {
	io.Writer
	Sum() []byte
}
