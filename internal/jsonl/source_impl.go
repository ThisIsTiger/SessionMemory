package jsonl

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ZstdSuffix ends a compressed rollout file.
const ZstdSuffix = ".zst"

// OpenSource returns the Source for path: zstd when the name ends in .zst,
// plain otherwise.
func OpenSource(path string) (Source, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ZstdSuffix) {
		return &zstdSource{path: path}, nil
	}
	return &plainSource{path: path, size: st.Size()}, nil
}

type plainSource struct {
	path string
	size int64
}

func (s *plainSource) Form() Form  { return FormPlain }
func (s *plainSource) Size() int64 { return s.size }

// OpenAt seeks; the returned PrefixHasher is nil because plain files are
// verified with Verify/PrefixHash instead.
func (s *plainSource) OpenAt(off int64) (io.ReadCloser, PrefixHasher, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if st.Size() < off {
		f.Close()
		return nil, nil, fmt.Errorf("%w: size %d, offset %d", ErrTruncated, st.Size(), off)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, nil, nil
}

type zstdSource struct{ path string }

func (s *zstdSource) Form() Form  { return FormZstd }
func (s *zstdSource) Size() int64 { return -1 }

type sumHasher struct{ h hash.Hash }

func (s sumHasher) Write(p []byte) (int, error) { return s.h.Write(p) }
func (s sumHasher) Sum() []byte                 { return s.h.Sum(nil) }

type zstdReadCloser struct {
	d *zstd.Decoder
	f *os.File
}

func (z *zstdReadCloser) Read(p []byte) (int, error) { return z.d.Read(p) }
func (z *zstdReadCloser) Close() error {
	z.d.Close()
	return z.f.Close()
}

// OpenAt decompresses from the start, hashing and discarding off bytes. A
// stream that ends before off (or a truncated frame) is ErrTruncated.
func (s *zstdSource) OpenAt(off int64) (io.ReadCloser, PrefixHasher, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, nil, err
	}
	d, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	rc := &zstdReadCloser{d: d, f: f}
	h := sumHasher{sha256.New()}
	if off > 0 {
		n, err := io.Copy(h, io.LimitReader(d, off))
		if err != nil && err != io.EOF {
			rc.Close()
			return nil, nil, fmt.Errorf("%w: %v", ErrTruncated, err)
		}
		if n < off {
			rc.Close()
			return nil, nil, fmt.Errorf("%w: stream has %d bytes, offset %d", ErrTruncated, n, off)
		}
	}
	return rc, h, nil
}
