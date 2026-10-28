package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func twoAccounts(t *testing.T) (*MultiMirror, *fakeWeiyun, *fakeWeiyun, *map[string]bool) {
	targets = map[string]string{}
	f1, f2 := newFake(t), newFake(t)
	local := NewFolder(t.TempDir())
	local.Settle = 0
	m1 := NewMirror(local, f1.client(), "SessionMemory")
	m2 := NewMirror(local, f2.client(), "SessionMemory")
	m1.TTL, m2.TTL = 0, 0
	mm := NewMultiMirror(local, Account{"primary", m1}, Account{"backup", m2})
	mm.StateFile = filepath.Join(t.TempDir(), "behind.json")
	skipped := map[string]bool{}
	mm.Skip = func(l string) bool { return skipped[l] }
	return mm, f1, f2, &skipped
}

func countFiles(f *fakeWeiyun) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, d := range f.files {
		n += len(d)
	}
	return n
}

// Every write reaches both accounts, so each one holds the whole vault.
func TestMultiMirrorWritesToBothAccounts(t *testing.T) {
	mm, f1, f2, _ := twoAccounts(t)
	ctx := context.Background()
	for p, d := range map[string]string{"public/vault.json": "{}", "manifests/dev_a/000000000001.mf": "m", "objects/ab/abcdef.obj": "o"} {
		if err := mm.Put(ctx, p, strings.NewReader(d)); err != nil {
			t.Fatal(err)
		}
	}
	if n1, n2 := countFiles(f1), countFiles(f2); n1 != 3 || n2 != 3 {
		t.Fatalf("files per account: %d and %d", n1, n2)
	}
}

// With the first account's quota gone, writes still succeed through the
// second; the first is brought level once it works again, and a reader that
// can only see the second account still gets everything.
func TestMultiMirrorSurvivesAnAccountRunningOutOfQuota(t *testing.T) {
	mm, f1, f2, skipped := twoAccounts(t)
	ctx := context.Background()
	if err := mm.Put(ctx, "manifests/dev_a/000000000001.mf", strings.NewReader("one")); err != nil {
		t.Fatal(err)
	}
	f1.quota = true // the server now refuses; the first call to fail is noticed as a failure
	if err := mm.Put(ctx, "manifests/dev_a/000000000002.mf", strings.NewReader("two")); err != nil {
		t.Fatalf("put with one account out of quota: %v", err)
	}
	if countFiles(f1) != 1 || countFiles(f2) != 2 {
		t.Fatalf("files: primary %d, backup %d", countFiles(f1), countFiles(f2))
	}
	if !mm.behind()["primary"] {
		t.Fatal("the missed write was not remembered")
	}
	// reading works while the first is out, and skipping it costs no call
	f1.mu.Lock()
	before := f1.calls
	f1.mu.Unlock()
	(*skipped)["primary"] = true
	if es, err := mm.List(ctx, "manifests/"); err != nil || len(es) != 2 {
		t.Fatalf("list: %v %d", err, len(es))
	}
	f1.mu.Lock()
	after := f1.calls
	f1.mu.Unlock()
	if after != before {
		t.Fatalf("a skipped account was asked %d time(s)", after-before)
	}
	// it comes back: caught up from the local copy
	f1.quota, (*skipped)["primary"] = false, false
	if err := mm.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if countFiles(f1) != 2 || mm.behind()["primary"] {
		t.Fatalf("not level: primary %d files, behind=%v", countFiles(f1), mm.behind())
	}
}

// A second computer that only reaches the second account sees what the first
// wrote into it, and what only reached the first account is found there.
func TestMultiMirrorReadsTheUnionOfAccounts(t *testing.T) {
	mm, f1, _, skipped := twoAccounts(t)
	ctx := context.Background()
	(*skipped)["backup"] = true
	mm.Put(ctx, "manifests/dev_a/000000000001.mf", strings.NewReader("only-in-primary"))
	(*skipped)["backup"] = false

	reader := func(f ...*fakeWeiyun) *MultiMirror {
		local := NewFolder(t.TempDir())
		local.Settle = 0
		var acc []Account
		for i, fk := range f {
			m := NewMirror(local, fk.client(), "SessionMemory")
			m.TTL = 0
			acc = append(acc, Account{string(rune('a' + i)), m})
		}
		return NewMultiMirror(local, acc...)
	}
	_, _, f2, _ := twoAccounts(t) // an unrelated empty account
	r := reader(f2, f1)           // the empty one first
	es, err := r.List(ctx, "manifests/")
	if err != nil || len(es) != 1 {
		t.Fatalf("union list: %v %v", err, es)
	}
	rc, err := r.Get(ctx, "manifests/dev_a/000000000001.mf")
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := io.ReadAll(rc); string(d) != "only-in-primary" {
		t.Fatalf("content %q", d)
	}
	rc.Close()
}

// When every account is out of quota, the error says so, and nothing is half-written.
func TestMultiMirrorAllAccountsOutOfQuota(t *testing.T) {
	mm, f1, f2, _ := twoAccounts(t)
	f1.quota, f2.quota = true, true
	ctx := context.Background()
	if _, err := mm.List(ctx, "manifests/"); !errors.Is(err, ErrQuota) {
		t.Fatalf("list: %v", err)
	}
	if err := mm.Put(ctx, "manifests/dev_a/1.mf", bytes.NewReader([]byte("x"))); !errors.Is(err, ErrQuota) {
		t.Fatalf("put: %v", err)
	}
}

// The key file must go from every account, and a failure to clear one is reported.
func TestMultiMirrorKeyFileRemovedEverywhere(t *testing.T) {
	mm, f1, f2, _ := twoAccounts(t)
	ctx := context.Background()
	if err := mm.PutKey(ctx, []byte("k")); err != nil {
		t.Fatal(err)
	}
	if countFiles(f1) != 1 || countFiles(f2) != 1 {
		t.Fatalf("key in %d and %d accounts", countFiles(f1), countFiles(f2))
	}
	f2.quota = true
	if err := mm.DeleteKey(ctx); err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("delete with one account unreachable: %v", err)
	}
	f2.quota = false
	if err := mm.DeleteKey(ctx); err != nil {
		t.Fatal(err)
	}
	if countFiles(f1) != 0 || countFiles(f2) != 0 {
		t.Fatalf("key left in %d and %d accounts", countFiles(f1), countFiles(f2))
	}
}
