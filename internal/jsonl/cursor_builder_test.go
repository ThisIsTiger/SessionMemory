package jsonl

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCursorBuilderMatchesFileCursor(t *testing.T) {
	for _, size := range []int{0, 1, hashWindow - 1, hashWindow, hashWindow + 1, 4*hashWindow + 3} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte((i*7 + i/257) % 251)
			}
			path := filepath.Join(t.TempDir(), "source")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			want, err := NewCursor(path, 3, int64(size))
			if err != nil {
				t.Fatal(err)
			}
			want.FileID = "" // a streamed cursor has no inode metadata
			for _, chunk := range []int{1, 97, hashWindow, 2*hashWindow + 1} {
				var builder CursorBuilder
				for pos := 0; pos < len(data); pos += chunk {
					part := data[pos:min(pos+chunk, len(data))]
					if n, err := builder.Write(part); err != nil || n != len(part) {
						t.Fatalf("write: %d %v", n, err)
					}
				}
				if got := builder.Cursor(3); got != want {
					t.Fatalf("chunk %d: got %+v want %+v", chunk, got, want)
				}
				if _, err := builder.Write(nil); err != nil || builder.Cursor(3) != want {
					t.Fatal("empty write changed the cursor")
				}
				if len(builder.head) > hashWindow || len(builder.tail) > hashWindow {
					t.Fatal("builder retained more than its edge windows")
				}
			}
		})
	}
}

func TestCursorBuilderDoesNotFollowLaterFileRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	var builder CursorBuilder
	data := []byte("{\"x\":1}\n")
	if _, err := builder.Write(data); err != nil {
		t.Fatal(err)
	}
	c := builder.Cursor(0)
	if err := os.WriteFile(path, bytes.ReplaceAll(data, []byte("1"), []byte("2")), 0600); err != nil {
		t.Fatal(err)
	}
	check, err := Verify(path, c)
	if err != nil || check.Status != StatusReplaced {
		t.Fatalf("stream fingerprint authenticated a later rewrite: %+v %v", check, err)
	}
}
