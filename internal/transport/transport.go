// Package transport moves immutable files to and from the shared folder. It
// knows nothing about sessions, encryption or manifests.
package transport

import (
	"context"
	"errors"
	"io"
	"time"
)

// Entry is one file in the shared folder.
type Entry struct {
	Path    string // slash-separated, relative to the transport root
	Size    int64
	ModTime time.Time
	// Suspect marks names that look like a sync client's conflict copy
	// ("name (1).ext", "name (conflicted copy).ext", "name-conflict-..."):
	// listed, but the sync engine verifies them by content (E7 refines this).
	Suspect bool
}

// ErrExists is returned by Put when the path exists with different content.
var ErrExists = errors.New("transport: path exists with different content")

// ErrNotStable means the file is still being written or downloaded; retry.
var ErrNotStable = errors.New("transport: file not stable yet")

// Transport is the whole contract.
type Transport interface {
	// Put publishes r at path, write-once: the bytes go to a sibling
	// "<name>.partial-<uuid>" file, are fsynced and closed, then renamed to
	// path. If path already exists with identical content, Put succeeds; with
	// different content it returns ErrExists. Partial files are never listed.
	Put(ctx context.Context, path string, r io.Reader) error
	// List returns files under prefix (recursive), excluding partial files.
	List(ctx context.Context, prefix string) ([]Entry, error)
	// Get opens path. ErrNotStable means a file of that name exists but cannot
	// be trusted yet.
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	// Stable reports whether e is safe to read: size and modification time were
	// unchanged across Settle and the file opens for reading.
	Stable(ctx context.Context, e Entry) (bool, error)
}

// Fetcher is an optional capability. A transport that mirrors a whole
// directory at a time (Weiyun) downloads every missing file of that directory
// when Get misses one; Fetch brings in only the file asked for. Callers that
// need a few small files of the big objects/ directory use FetchOne.
type Fetcher interface {
	Fetch(ctx context.Context, path string) (io.ReadCloser, error)
}

// FetchOne is Get, but without downloading the rest of the directory on
// transports that would.
func FetchOne(ctx context.Context, t Transport, path string) (io.ReadCloser, error) {
	if f, ok := t.(Fetcher); ok {
		return f.Fetch(ctx, path)
	}
	return t.Get(ctx, path)
}

// FolderOptions tunes FolderTransport.
type FolderOptions struct {
	Root   string
	Settle time.Duration // default 2s
	// Backoff for ErrNotStable: starts at 1s, doubles, caps at 60s, gives up
	// after 10 minutes (the entry stays pending and is retried by the next
	// reconcile).
}

// PartialSuffix marks unfinished files; importers must ignore any name
// containing it.
const PartialSuffix = ".partial-"
