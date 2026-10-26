package jsonl

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zst(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf, zstd.WithEncoderCRC(false))
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func TestSourcePlainAndZstdAgree(t *testing.T) {
	data := bytes.Repeat([]byte("{\"a\":1}\n"), 5000)
	dir := t.TempDir()
	plain := filepath.Join(dir, "r.jsonl")
	comp := filepath.Join(dir, "r.jsonl.zst")
	os.WriteFile(plain, data, 0600)
	os.WriteFile(comp, zst(t, data), 0600)

	for _, off := range []int64{0, 8, 4096, int64(len(data))} {
		var got [2][]byte
		for i, p := range []string{plain, comp} {
			src, err := OpenSource(p)
			if err != nil {
				t.Fatal(err)
			}
			rc, h, err := src.OpenAt(off)
			if err != nil {
				t.Fatalf("%s OpenAt(%d): %v", p, off, err)
			}
			got[i], _ = io.ReadAll(rc)
			rc.Close()
			if src.Form() == FormZstd {
				want := sha256.Sum256(data[:off])
				if off > 0 && !bytes.Equal(h.Sum(), want[:]) {
					t.Errorf("prefix hash wrong at %d", off)
				}
			}
		}
		if !bytes.Equal(got[0], data[off:]) || !bytes.Equal(got[1], data[off:]) {
			t.Errorf("offset %d: plain/zstd content differs", off)
		}
	}
}

func TestZstdTruncated(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100000)
	c := zst(t, data)
	p := filepath.Join(t.TempDir(), "t.jsonl.zst")
	os.WriteFile(p, c[:len(c)/2], 0600)
	src, _ := OpenSource(p)
	if _, _, err := src.OpenAt(int64(len(data))); !errors.Is(err, ErrTruncated) {
		t.Errorf("truncated frame: got %v, want ErrTruncated", err)
	}
	full := filepath.Join(t.TempDir(), "f.jsonl.zst")
	os.WriteFile(full, c, 0600)
	src, _ = OpenSource(full)
	if _, _, err := src.OpenAt(int64(len(data)) + 1); !errors.Is(err, ErrTruncated) {
		t.Errorf("offset past end: got %v", err)
	}
}
