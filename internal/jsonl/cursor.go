package jsonl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// hashWindow is how many bytes at each end of the consumed prefix are hashed.
const hashWindow = 4096

// Cursor records how far a file has been consumed and enough of its content to
// notice that the file underneath changed.
//
// Head/Tail hashes cover only the first and last hashWindow bytes of the
// consumed prefix, so a same-length rewrite strictly inside a long prefix is
// not caught by Verify; PrefixHash performs the full check for deep
// verification.
type Cursor struct {
	// Generation increases each time the source is found replaced or
	// rewritten; segments of different generations are never mixed.
	Generation int `json:"generation"`
	// Offset is the end of the last consumed complete record.
	Offset int64 `json:"offset"`
	// FileID is informational (device+inode, or the Windows file index). A
	// changed FileID with matching hashes is a move or atomic rewrite of the
	// same content, not a replacement.
	FileID   string `json:"file_id,omitempty"`
	HeadHash string `json:"head_hash,omitempty"`
	TailHash string `json:"tail_hash,omitempty"`
}

// Status is the outcome of checking a cursor against its file.
type Status int

const (
	// StatusOK means the consumed prefix still matches.
	StatusOK Status = iota
	// StatusTruncated means the file is now shorter than the cursor offset.
	StatusTruncated
	// StatusReplaced means the content at the head or tail of the consumed
	// prefix differs: the file was replaced or rewritten.
	StatusReplaced
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusTruncated:
		return "truncated"
	case StatusReplaced:
		return "replaced"
	}
	return fmt.Sprintf("status(%d)", int(s))
}

// Check is the result of Verify.
type Check struct {
	Status        Status
	Size          int64
	FileID        string
	FileIDChanged bool
}

// NewCursor builds a cursor for path at offset, which must be a record
// boundary at or before the end of the file.
func NewCursor(path string, generation int, offset int64) (Cursor, error) {
	f, err := os.Open(path)
	if err != nil {
		return Cursor{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Cursor{}, err
	}
	if offset < 0 || st.Size() < offset {
		return Cursor{}, fmt.Errorf("%w: size %d, offset %d", ErrTruncated, st.Size(), offset)
	}
	c := Cursor{Generation: generation, Offset: offset, FileID: fileID(f)}
	if offset == 0 {
		return c, nil
	}
	if c.HeadHash, c.TailHash, err = edgeHashes(f, offset); err != nil {
		return Cursor{}, err
	}
	return c, nil
}

// Verify checks that the consumed prefix described by c is still intact. A
// missing file is returned as an error (os.ErrNotExist) rather than a Status:
// whether that means "detached" or "deleted" is the caller's decision.
func Verify(path string, c Cursor) (Check, error) {
	f, err := os.Open(path)
	if err != nil {
		return Check{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Check{}, err
	}
	res := Check{Size: st.Size(), FileID: fileID(f)}
	res.FileIDChanged = c.FileID != "" && res.FileID != "" && c.FileID != res.FileID
	switch {
	case st.Size() < c.Offset:
		res.Status = StatusTruncated
	case c.Offset == 0:
		res.Status = StatusOK
	default:
		head, tail, err := edgeHashes(f, c.Offset)
		if err != nil {
			return Check{}, err
		}
		if head != c.HeadHash || tail != c.TailHash {
			res.Status = StatusReplaced
		}
	}
	return res, nil
}

// PrefixHash returns the hex SHA-256 of the first n bytes of path, for deep
// verification of the whole consumed prefix.
func PrefixHash(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	copied, err := io.Copy(h, io.LimitReader(f, n))
	if err != nil {
		return "", err
	}
	if copied < n {
		return "", fmt.Errorf("%w: wanted %d bytes, file has %d", ErrTruncated, n, copied)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// edgeHashes hashes the first and last hashWindow bytes of the prefix [0, offset).
func edgeHashes(f *os.File, offset int64) (head, tail string, err error) {
	window := int64(hashWindow)
	if offset < window {
		window = offset
	}
	if head, err = hashRange(f, 0, window); err != nil {
		return "", "", err
	}
	if tail, err = hashRange(f, offset-window, window); err != nil {
		return "", "", err
	}
	return head, tail, nil
}

func hashRange(f *os.File, off, n int64) (string, error) {
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}
