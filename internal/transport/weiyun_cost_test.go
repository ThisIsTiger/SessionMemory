package transport

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// What a round costs Weiyun, in calls, in a vault that already holds a few
// hundred objects (a new process each time, as in life). A poll is a couple of
// calls whatever the vault holds, and so is an upload: the objects directory,
// by far the largest, is never listed to see whether a name is taken (an object
// name is the hash of its bytes, and the service keeps one copy of identical
// content), so what a reply costs does not grow with the vault.
func TestMirrorCallCosts(t *testing.T) {
	targets = map[string]string{}
	fake := newFake(t)
	ctx := context.Background()
	cache := filepath.Join(t.TempDir(), "keys.json")
	mk := func() *Mirror {
		m := NewMirror(NewFolder(t.TempDir()), fake.client(), "SessionMemory")
		m.Local.Settle = 0
		m.TTL = 0
		m.KeyCache = cache
		return m
	}
	seed := mk()
	for i := 0; i < 300; i++ {
		p := fmt.Sprintf("objects/%02x/%064x.obj", i%256, i)
		if err := seed.Put(ctx, p, strings.NewReader("o")); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Put(ctx, "manifests/dev_a/000000000001-aa.manifest", strings.NewReader("m")); err != nil {
		t.Fatal(err)
	}
	measure := func(name string, f func(m *Mirror)) int {
		fake.mu.Lock()
		before := fake.calls
		fake.mu.Unlock()
		f(mk())
		fake.mu.Lock()
		defer fake.mu.Unlock()
		t.Logf("%-28s %d call(s)", name, fake.calls-before)
		return fake.calls - before
	}
	poll := measure("poll (list manifests/)", func(m *Mirror) {
		if _, err := m.List(ctx, "manifests/"); err != nil {
			t.Fatal(err)
		}
	})
	push := measure("upload one reply (2 files)", func(m *Mirror) {
		if _, err := m.List(ctx, "manifests/dev_a/"); err != nil { // what Publish does first
			t.Fatal(err)
		}
		if err := m.Put(ctx, fmt.Sprintf("objects/ab/%064x.obj", 999), strings.NewReader("new")); err != nil {
			t.Fatal(err)
		}
		if err := m.Put(ctx, "manifests/dev_a/000000000002-bb.manifest", strings.NewReader("m2")); err != nil {
			t.Fatal(err)
		}
	})
	again := measure("send the same object again", func(m *Mirror) {
		if err := m.Put(ctx, fmt.Sprintf("objects/ab/%064x.obj", 999), strings.NewReader("new")); err != nil {
			t.Fatal(err)
		}
	})
	if poll > 2 {
		t.Errorf("a poll is %d calls", poll)
	}
	// list the device's manifests, upload an object, list again, upload the manifest
	if push > 8 {
		t.Errorf("uploading one reply took %d calls; it must not depend on the %d objects already there", push, 300)
	}
	if again != 0 {
		t.Errorf("sending an object this computer already sent took %d calls", again)
	}
}
