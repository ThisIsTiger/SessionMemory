package transport

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingTransport counts the requests that go to the Weiyun API.
type countingTransport struct {
	n  atomic.Int64
	rt http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "mcpserver") {
		c.n.Add(1)
	}
	return c.rt.RoundTrip(r)
}

// TestLiveWeiyunDuplicateUploads shows what the real service does when a file
// with a name that is already in the directory is uploaded again. It needs
// SM_LIVE_WEIYUN=1 and WEIYUN_MCP_TOKEN, works in a throwaway directory and
// moves that directory to the recycle bin afterwards.
func TestLiveWeiyunDuplicateUploads(t *testing.T) {
	token := os.Getenv("WEIYUN_MCP_TOKEN")
	if os.Getenv("SM_LIVE_WEIYUN") != "1" || token == "" {
		t.Skip("set SM_LIVE_WEIYUN=1 and WEIYUN_MCP_TOKEN")
	}
	ctx := context.Background()
	c := NewWeiyunClient(token)
	name := fmt.Sprintf("SessionMemory-test-%d", time.Now().UnixNano())
	m := NewMirror(NewFolder(t.TempDir()), c, name)
	t.Cleanup(func() {
		if err := m.RemoveTestVault(ctx); err != nil {
			t.Errorf("cleanup of %s failed: %v", name, err)
		}
	})
	root, err := c.ensureDir(ctx, "", name)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := c.ensureDir(ctx, root, "objects")
	if err != nil {
		t.Fatal(err)
	}
	names := func() string {
		_, fs, _, err := c.list(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range fs {
			out = append(out, fmt.Sprintf("%s(%d)", f.Name, f.Size))
		}
		return strings.Join(out, " ")
	}
	one := []byte(strings.Repeat("first body ", 40))
	other := []byte(strings.Repeat("second body ", 40))
	longer := []byte(strings.Repeat("second body ", 50))
	step := func(label string, data []byte) {
		err := c.upload(ctx, dir, "probe-object", data)
		t.Logf("%-28s err=%v  dir now: %s", label, err, names())
	}
	step("first upload", one)
	step("same name, same bytes", one)
	step("same name, same size, other", other)
	step("same name, other size", longer)
}

// TestLiveWeiyunUploadCost counts the calls a reply's worth of uploading makes
// against the real service: an object and a manifest, from a new process that
// remembers the folder keys, with the objects directory holding other files.
func TestLiveWeiyunUploadCost(t *testing.T) {
	token := os.Getenv("WEIYUN_MCP_TOKEN")
	if os.Getenv("SM_LIVE_WEIYUN") != "1" || token == "" {
		t.Skip("set SM_LIVE_WEIYUN=1 and WEIYUN_MCP_TOKEN")
	}
	ctx := context.Background()
	name := fmt.Sprintf("SessionMemory-test-%d", time.Now().UnixNano())
	cache := filepath.Join(t.TempDir(), "keys.json")
	ct := &countingTransport{rt: http.DefaultTransport}
	mk := func() *Mirror {
		c := NewWeiyunClient(token)
		c.HTTP = &http.Client{Transport: ct, Timeout: 60 * time.Second}
		m := NewMirror(NewFolder(t.TempDir()), c, name)
		m.Local.Settle = 0
		m.KeyCache = cache
		return m
	}
	t.Cleanup(func() {
		if err := mk().RemoveTestVault(ctx); err != nil {
			t.Errorf("cleanup of %s failed: %v", name, err)
		}
	})
	seed := mk()
	for i := 0; i < 5; i++ {
		if err := seed.Put(ctx, fmt.Sprintf("objects/%02x/%064x.obj", i, i), strings.NewReader(fmt.Sprintf("object %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Put(ctx, "manifests/dev_a/000000000001-aa.manifest", strings.NewReader("m1")); err != nil {
		t.Fatal(err)
	}
	count := func(label string, f func(m *Mirror)) {
		before := ct.n.Load()
		f(mk())
		t.Logf("%-34s %d call(s)", label, ct.n.Load()-before)
	}
	count("poll (list manifests/)", func(m *Mirror) {
		if _, err := m.List(ctx, "manifests/"); err != nil {
			t.Fatal(err)
		}
	})
	count("upload one reply (object+manifest)", func(m *Mirror) {
		if _, err := m.List(ctx, "manifests/dev_a/"); err != nil {
			t.Fatal(err)
		}
		if err := m.Put(ctx, fmt.Sprintf("objects/ab/%064x.obj", 99), strings.NewReader("a new object")); err != nil {
			t.Fatal(err)
		}
		if err := m.Put(ctx, "manifests/dev_a/000000000002-bb.manifest", strings.NewReader("m2")); err != nil {
			t.Fatal(err)
		}
	})
	count("send the same object again", func(m *Mirror) {
		if err := m.Put(ctx, fmt.Sprintf("objects/ab/%064x.obj", 99), strings.NewReader("a new object")); err != nil {
			t.Fatal(err)
		}
	})
}
