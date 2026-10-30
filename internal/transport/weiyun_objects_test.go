package transport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// objects are content-addressed, so uploading one never lists the objects
// directory; what has been sent is remembered across processes.

func objectNames(f *fakeWeiyun, dir string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.files[dir] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func objectsDirKey(f *fakeWeiyun) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, d := range f.dirs {
		if d[0] == "objects" {
			return k
		}
	}
	return ""
}

func TestObjectUploadNeverListsTheObjectsDirectory(t *testing.T) {
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
	first := mk()
	for i := 0; i < 20; i++ {
		if err := first.Put(ctx, fmt.Sprintf("objects/%02x/%064x.obj", i, i), strings.NewReader("o")); err != nil {
			t.Fatal(err)
		}
	}
	dir := objectsDirKey(fake)
	if dir == "" {
		t.Fatal("no objects directory")
	}
	if n := fake.listed[dir]; n != 0 {
		t.Fatalf("uploading 20 objects listed the objects directory %d time(s)", n)
	}
	if got := len(objectNames(fake, dir)); got != 20 {
		t.Fatalf("%d objects in the cloud, want 20", got)
	}
	// a new process knows what the last one sent
	second := mk()
	fake.mu.Lock()
	before := fake.calls
	fake.mu.Unlock()
	if err := second.Put(ctx, fmt.Sprintf("objects/%02x/%064x.obj", 7, 7), strings.NewReader("o")); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	spent := fake.calls - before
	fake.mu.Unlock()
	if spent != 0 {
		t.Fatalf("an object that was already sent cost %d call(s)", spent)
	}
	// a different size under a remembered name is still a conflict
	third := mk()
	if err := third.Put(ctx, fmt.Sprintf("objects/%02x/%064x.obj", 7, 7), strings.NewReader("other bytes")); !errors.Is(err, ErrExists) {
		t.Fatalf("conflicting object = %v", err)
	}
}

func TestObjectSentAgainWithoutTheMemoIsStillOneCopy(t *testing.T) {
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
	path := fmt.Sprintf("objects/ab/%064x.obj", 1)
	if err := mk().Put(ctx, path, strings.NewReader("same bytes")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cache + pushedSuffix); err != nil { // the memo is lost
		t.Fatal(err)
	}
	if err := mk().Put(ctx, path, strings.NewReader("same bytes")); err != nil {
		t.Fatal(err)
	}
	if got := objectNames(fake, objectsDirKey(fake)); len(got) != 1 {
		t.Fatalf("the cloud holds %v, want a single copy", got)
	}
}

func TestObjectUploadWithoutAKeyCacheStillWorks(t *testing.T) {
	targets = map[string]string{}
	fake := newFake(t)
	ctx := context.Background()
	m := NewMirror(NewFolder(t.TempDir()), fake.client(), "SessionMemory")
	m.Local.Settle = 0
	for i := 0; i < 3; i++ {
		if err := m.Put(ctx, fmt.Sprintf("objects/ab/%064x.obj", i), strings.NewReader("o")); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(objectNames(fake, objectsDirKey(fake))); got != 3 {
		t.Fatalf("%d objects in the cloud, want 3", got)
	}
	// within the process the objects are not sent twice
	fake.mu.Lock()
	before := fake.uploads
	fake.mu.Unlock()
	if err := m.Put(ctx, fmt.Sprintf("objects/ab/%064x.obj", 1), strings.NewReader("o")); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.uploads != before {
		t.Fatal("an object sent a moment ago was sent again")
	}
}

// The vault folder was trashed and made again (another computer reset the
// cloud): the remembered keys lead nowhere, and what was sent before is not in
// the new folder, so it has to be sent again.
func TestObjectUploadRecoversWhenTheCloudFolderWasReplaced(t *testing.T) {
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
	old := fmt.Sprintf("objects/ab/%064x.obj", 1)
	if err := mk().Put(ctx, old, strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	// the folder goes away
	fake.mu.Lock()
	fake.dirs = map[string][2]string{}
	fake.files = map[string]map[string][]byte{"root": {}}
	fake.mu.Unlock()

	fresh := fmt.Sprintf("objects/ab/%064x.obj", 2)
	if err := mk().Put(ctx, fresh, strings.NewReader("new")); err != nil {
		t.Fatalf("upload into a vanished folder: %v", err)
	}
	dir := objectsDirKey(fake)
	if dir == "" || len(objectNames(fake, dir)) != 1 {
		t.Fatalf("after recovery the new objects directory holds %v", objectNames(fake, dir))
	}
	// the object sent to the old folder is not remembered for the new one
	if err := mk().Put(ctx, old, strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	if got := len(objectNames(fake, dir)); got != 2 {
		t.Fatalf("the new folder holds %d objects, want 2", got)
	}
}

func TestObjectUploadKeepsTheDailyQuotaError(t *testing.T) {
	targets = map[string]string{}
	fake := newFake(t)
	ctx := context.Background()
	cache := filepath.Join(t.TempDir(), "keys.json")
	m := NewMirror(NewFolder(t.TempDir()), fake.client(), "SessionMemory")
	m.Local.Settle = 0
	m.KeyCache = cache
	if err := m.Put(ctx, fmt.Sprintf("objects/ab/%064x.obj", 1), strings.NewReader("o")); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.quota = true
	fake.mu.Unlock()
	m2 := NewMirror(NewFolder(t.TempDir()), fake.client(), "SessionMemory")
	m2.Local.Settle = 0
	m2.KeyCache = cache
	err := m2.Put(ctx, fmt.Sprintf("objects/ab/%064x.obj", 2), strings.NewReader("p"))
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("put under quota = %v", err)
	}
	// the remembered keys survive a quota refusal: they were not wrong
	if _, statErr := os.Stat(cache); statErr != nil {
		t.Fatalf("the key cache was dropped on a quota error: %v", statErr)
	}
}
