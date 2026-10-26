package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTransport(t *testing.T) *Folder {
	f := NewFolder(t.TempDir())
	f.Settle = 10 * time.Millisecond
	f.sleep = func(time.Duration) {}
	return f
}

func TestPutIsWriteOnceAndIdempotent(t *testing.T) {
	f, ctx := newTransport(t), context.Background()
	if err := f.Put(ctx, "objects/ab/x.obj", bytes.NewReader([]byte("one"))); err != nil {
		t.Fatal(err)
	}
	if err := f.Put(ctx, "objects/ab/x.obj", bytes.NewReader([]byte("one"))); err != nil {
		t.Errorf("identical re-put failed: %v", err)
	}
	if err := f.Put(ctx, "objects/ab/x.obj", bytes.NewReader([]byte("two"))); !errors.Is(err, ErrExists) {
		t.Errorf("different content: %v", err)
	}
	if err := f.Put(ctx, "../escape", bytes.NewReader(nil)); err == nil {
		t.Error("path escape accepted")
	}
	ents, _ := os.ReadDir(filepath.Join(f.Root, "objects", "ab"))
	if len(ents) != 1 {
		t.Errorf("leftover files: %v", ents)
	}
}

func TestListIgnoresPartialAndFlagsConflictCopies(t *testing.T) {
	f, ctx := newTransport(t), context.Background()
	f.Put(ctx, "manifests/dev/000000000001-aaaaaaaaaaaa.manifest", bytes.NewReader([]byte("m")))
	os.WriteFile(filepath.Join(f.Root, "manifests", "dev", "000000000002-bbbbbbbbbbbb.manifest"+PartialSuffix+"123"), []byte("p"), 0600)
	os.WriteFile(filepath.Join(f.Root, "manifests", "dev", "000000000001-aaaaaaaaaaaa (1).manifest"), []byte("m"), 0600)
	ents, err := f.List(ctx, "manifests/")
	if err != nil || len(ents) != 2 {
		t.Fatalf("list = %v %v", ents, err)
	}
	suspect := 0
	for _, e := range ents {
		if e.Suspect {
			suspect++
		}
	}
	if suspect != 1 {
		t.Errorf("suspect = %d", suspect)
	}
	if none, _ := f.List(ctx, "nothing/"); len(none) != 0 {
		t.Error("missing prefix must list empty")
	}
}

func TestGetAndStable(t *testing.T) {
	f, ctx := newTransport(t), context.Background()
	f.Put(ctx, "a/b", bytes.NewReader([]byte("data")))
	rc, err := f.Get(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "data" {
		t.Errorf("got %q", b)
	}
	if _, err := f.Get(ctx, "missing"); err == nil {
		t.Error("missing file opened")
	}
	// a file still being modified is not stable
	p := filepath.Join(f.Root, "a", "b")
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				os.WriteFile(p, bytes.Repeat([]byte("x"), 10+i), 0600)
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	f.Settle = 30 * time.Millisecond
	st, _ := os.Stat(p)
	ok, _ := f.Stable(ctx, Entry{Path: "a/b", Size: st.Size(), ModTime: st.ModTime()})
	close(stop)
	if ok {
		t.Error("a file being rewritten was reported stable")
	}
}
