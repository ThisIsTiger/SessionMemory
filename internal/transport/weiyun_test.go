package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCalcUploadParamsMatchesReference(t *testing.T) {
	raw, err := os.ReadFile("testdata/weiyun_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vec map[string]struct {
		FileSHA   string   `json:"file_sha"`
		MD5       string   `json:"file_md5"`
		Blocks    []string `json:"block_sha_list"`
		CheckSHA  string   `json:"check_sha"`
		CheckData string   `json:"check_data"`
	}
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatal(err)
	}
	for size, want := range vec {
		var n int
		fmt.Sscan(size, &n)
		data := make([]byte, n)
		for i := range data {
			data[i] = byte((i*7 + 3) % 251)
		}
		got := calcUploadParams(data)
		if got.FileSHA != want.FileSHA || got.MD5 != want.MD5 || got.CheckSHA != want.CheckSHA || got.CheckData != want.CheckData || strings.Join(got.Blocks, ",") != strings.Join(want.Blocks, ",") {
			t.Fatalf("size %s: parameters differ from the reference implementation\n got %+v\nwant %+v", size, got, want)
		}
	}
}

// fakeWeiyun is an in-memory implementation of the tools the mirror uses.
type fakeWeiyun struct {
	mu      sync.Mutex
	dirs    map[string][2]string         // key -> {name, parent key}
	files   map[string]map[string][]byte // dir key -> name -> data
	pending map[string][]byte
	srv     *httptest.Server
	token   string
	calls   int
	listed  map[string]int // dir key -> weiyun.list calls
	quota   bool           // refuse every call as Weiyun does when the day's quota is used up
}

func newFake(t *testing.T) *fakeWeiyun {
	f := &fakeWeiyun{dirs: map[string][2]string{}, files: map[string]map[string][]byte{"root": {}}, pending: map[string][]byte{}, token: "tok"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWeiyun) client() *WeiyunClient {
	c := NewWeiyunClient(f.token)
	c.URL = f.srv.URL + "/mcp"
	return c
}

func (f *fakeWeiyun) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/dl/") {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/dl/"), "/", 2)
		if r.Header.Get("Cookie") != "dl=1" {
			http.Error(w, "cookie", 403)
			return
		}
		w.Write(f.files[parts[0]][parts[1]])
		return
	}
	if r.Header.Get("WyHeader") != "mcp_token="+f.token {
		http.Error(w, "auth", 401)
		return
	}
	f.calls++
	if f.quota {
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "MCP error -32603: tool execution failed (tool: weiyun.list): type:business, code:117401, msg:rate limit exceeded, daily quota used up. Please try again tomorrow"}})
		return
	}
	var req struct {
		Params struct {
			Name string         `json:"name"`
			Args map[string]any `json:"arguments"`
		} `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	a := req.Params.Args
	str := func(k string) string { s, _ := a[k].(string); return s }
	var out any
	switch req.Params.Name {
	case "weiyun.list":
		key := str("dir_key")
		if key == "" {
			key = "root"
		}
		if f.listed == nil {
			f.listed = map[string]int{}
		}
		f.listed[key]++
		if _, known := f.dirs[key]; key != "root" && !known {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "dir not found"}})
			return
		}
		var dl []map[string]any
		var fl []map[string]any
		for k, n := range f.dirs {
			if n[1] == key {
				dl = append(dl, map[string]any{"dir_key": k, "dir_name": n[0]})
			}
		}
		for n, d := range f.files[key] {
			fl = append(fl, map[string]any{"file_id": "id-" + n, "filename": n, "file_size": len(d)})
		}
		off := int(a["offset"].(float64))
		all := append(append([]map[string]any{}, dl...), fl...)
		end := off + 3 // tiny pages exercise pagination
		fin := end >= len(all)
		if fin {
			end = len(all)
		}
		page := all[min(off, len(all)):end]
		var pd, pf []map[string]any
		for _, it := range page {
			if _, ok := it["dir_key"]; ok {
				pd = append(pd, it)
			} else {
				pf = append(pf, it)
			}
		}
		out = map[string]any{"pdir_key": "root", "dir_list": pd, "file_list": pf, "finish_flag": fin}
	case "weiyun.create_dir":
		k := fmt.Sprintf("d%d", len(f.dirs)+1)
		parent := str("pdir_key")
		if parent == "" {
			parent = "root"
		}
		f.dirs[k] = [2]string{str("dir_name"), parent}
		f.files[k] = map[string][]byte{}
		out = map[string]any{"dir_key": k, "dir_name": str("dir_name")}
	case "weiyun.download":
		it := a["items"].([]any)[0].(map[string]any)
		name := strings.TrimPrefix(it["file_id"].(string), "id-")
		out = map[string]any{"items": []map[string]any{{"https_download_url": f.srv.URL + "/dl/" + it["pdir_key"].(string) + "/" + name, "cookie": "dl=1"}}}
	case "weiyun.upload":
		out = f.upload(a)
	case "weiyun.delete":
		if a["delete_completely"] != false {
			out = map[string]any{"error": "permanent delete refused"}
			break
		}
		for _, it := range a["file_list"].([]any) {
			m := it.(map[string]any)
			delete(f.files[m["pdir_key"].(string)], strings.TrimPrefix(m["file_id"].(string), "id-"))
		}
		out = map[string]any{"error": ""}
	default:
		out = map[string]any{"error": "unknown tool " + req.Params.Name}
	}
	b, _ := json.Marshal(out)
	json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}})
}

func (f *fakeWeiyun) upload(a map[string]any) any {
	name := a["filename"].(string)
	size := int(a["file_size"].(float64))
	key := "u-" + name
	if d, ok := a["file_data"].(string); ok {
		raw, _ := base64.StdEncoding.DecodeString(d)
		f.pending[key] = append(f.pending[key], raw...)
		if len(f.pending[key]) >= size {
			dir := a["pdir_key"]
			_ = dir
			f.files[f.target(name)][name] = f.pending[key]
			delete(f.pending, key)
			return map[string]any{"upload_state": 2}
		}
		return map[string]any{"upload_state": 1}
	}
	pdir, _ := a["pdir_key"].(string)
	f.pdirOf(name, pdir)
	off := len(f.pending[key])
	n := min(size-off, weiyunBlock)
	return map[string]any{"file_exist": false, "upload_key": key, "ex": "x", "upload_state": 1,
		"channel_list": []map[string]any{{"id": 1, "offset": off, "len": n}}}
}

var targets = map[string]string{}

func (f *fakeWeiyun) pdirOf(name, pdir string)  { targets[name] = pdir }
func (f *fakeWeiyun) target(name string) string { return targets[name] }

func TestMirrorReplicatesBetweenDevices(t *testing.T) {
	targets = map[string]string{}
	fake := newFake(t)
	ctx := context.Background()
	mk := func() (*Mirror, string) {
		dir := t.TempDir()
		m := NewMirror(NewFolder(dir), fake.client(), "SessionMemory")
		m.Local.Settle = 0
		m.TTL = 0
		return m, dir
	}
	a, adir := mk()
	b, bdir := mk()

	big := bytes.Repeat([]byte("0123456789abcdef"), 100000) // 1.6 MB: several chunks
	for path, data := range map[string][]byte{
		"public/vault.json":               {'{', '}'},
		"manifests/dev_a/000000000001.mf": []byte("manifest"),
		"objects/ab/abcdef0123.obj":       big,
		"objects/cd/cdef.obj":             []byte("x"),
	} {
		if err := a.Put(ctx, path, bytes.NewReader(data)); err != nil {
			t.Fatalf("put %s: %v", path, err)
		}
	}
	// idempotent, and a different size is a conflict
	if err := a.Put(ctx, "objects/cd/cdef.obj", strings.NewReader("x")); err != nil {
		t.Fatalf("repeat put: %v", err)
	}
	if err := a.Put(ctx, "objects/cd/cdef.obj", strings.NewReader("different")); !errors.Is(err, ErrExists) {
		t.Fatalf("conflicting put = %v", err)
	}

	es, err := b.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 4 {
		t.Fatalf("B sees %d files: %+v", len(es), es)
	}
	got, err := os.ReadFile(filepath.Join(bdir, "objects", "ab", "abcdef0123.obj"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("large file did not replicate intact: %v", err)
	}
	rc, err := b.Get(ctx, "manifests/dev_a/000000000001.mf")
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := io.ReadAll(rc); string(d) != "manifest" {
		t.Fatalf("manifest = %q", d)
	}
	rc.Close()

	// B publishes; A pulls it; nothing uploads twice
	if err := b.Put(ctx, "manifests/dev_b/000000000001.mf", strings.NewReader("from b")); err != nil {
		t.Fatal(err)
	}
	if es, _ = a.List(ctx, "manifests"); len(es) != 2 {
		t.Fatalf("A sees %d manifests", len(es))
	}
	// files written locally outside Put are published by Push (vault.json at attach time)
	os.WriteFile(filepath.Join(adir, "public", "extra.json"), []byte("e"), 0600)
	if err := a.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bdir, "public", "extra.json")); err != nil {
		t.Fatal("Push/Pull did not carry a locally written file")
	}
}

func TestMirrorBadTokenFails(t *testing.T) {
	fake := newFake(t)
	c := fake.client()
	c.Token = "wrong"
	m := NewMirror(NewFolder(t.TempDir()), c, "SessionMemory")
	if _, err := m.List(context.Background(), ""); err == nil {
		t.Fatal("a rejected token must surface as an error")
	}
}

// The key file is kept apart from normal sync: only the key methods move it,
// so removing it from the cloud is not undone by another device's Push.
func TestMirrorKeyFileIsSeparateAndDeletable(t *testing.T) {
	targets = map[string]string{}
	fake := newFake(t)
	ctx := context.Background()
	mk := func() (*Mirror, string) {
		dir := t.TempDir()
		m := NewMirror(NewFolder(dir), fake.client(), "SessionMemory")
		m.Local.Settle = 0
		m.TTL = 0
		return m, dir
	}
	a, _ := mk()
	b, bdir := mk()
	if _, ok, err := a.GetKey(ctx); err != nil || ok {
		t.Fatalf("empty cloud folder: %v %v", ok, err)
	}
	if err := a.PutKey(ctx, []byte(`{"key":"k1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := a.PutKey(ctx, []byte(`{"key":"other"}`)); err != nil {
		t.Fatal(err) // an existing key file is left alone
	}
	got, ok, err := b.GetKey(ctx)
	if err != nil || !ok || string(got) != `{"key":"k1"}` {
		t.Fatalf("B reads the key: %q %v %v", got, ok, err)
	}
	// ordinary sync does not carry it to B's local copy, nor push it back up
	if err := b.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bdir, "public", "vault-key.json")); err == nil {
		t.Fatal("Pull copied the key file into the local mirror")
	}
	if err := b.DeleteKey(ctx); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(bdir, "public"), 0700)
	os.WriteFile(filepath.Join(bdir, "public", "vault-key.json"), []byte("stale"), 0600)
	if err := b.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.GetKey(ctx); ok {
		t.Fatal("the deleted key file came back")
	}
}

// Polling for new manifests lists the vault folder and the manifests
// directory only: the objects directory, the largest by far, is listed only
// when an object is actually missing.
func TestMirrorManifestPollSkipsTheObjectsDirectory(t *testing.T) {
	targets = map[string]string{}
	fake := newFake(t)
	ctx := context.Background()
	mk := func() *Mirror {
		m := NewMirror(NewFolder(t.TempDir()), fake.client(), "SessionMemory")
		m.Local.Settle = 0
		m.TTL = 0
		return m
	}
	a, b := mk(), mk()
	for path, data := range map[string][]byte{
		"public/vault.json":               []byte("{}"),
		"manifests/dev_a/000000000001.mf": []byte("manifest"),
		"objects/ab/abcdef0123.obj":       []byte("object"),
		"objects/cd/cdef.obj":             []byte("x"),
	} {
		if err := a.Put(ctx, path, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	objectsDir := ""
	for k, n := range fake.dirs {
		if n[0] == "objects" {
			objectsDir = k
		}
	}
	fake.mu.Lock()
	fake.listed = map[string]int{}
	fake.mu.Unlock()
	es, err := b.List(ctx, "manifests/")
	if err != nil || len(es) != 1 {
		t.Fatalf("manifest poll: %v %+v", err, es)
	}
	fake.mu.Lock()
	n := fake.listed[objectsDir]
	fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("a manifest poll listed the objects directory %d time(s)", n)
	}
	if _, err := os.Stat(filepath.Join(b.Local.Root, "objects", "ab", "abcdef0123.obj")); err == nil {
		t.Fatal("a manifest poll downloaded objects")
	}
	// the object is fetched when something asks for it
	rc, err := b.Get(ctx, "objects/ab/abcdef0123.obj")
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if d, _ := io.ReadAll(rc); string(d) != "object" {
		t.Fatalf("object = %q", d)
	}
	rc.Close()
}

func TestWeiyunQuotaIsRecognisedAndReported(t *testing.T) {
	fake := newFake(t)
	fake.quota = true
	c := fake.client()
	var quotaCalls, okCalls int
	c.OnQuota = func(error) { quotaCalls++ }
	c.OnOK = func() { okCalls++ }
	m := NewMirror(NewFolder(t.TempDir()), c, "SessionMemory")
	_, err := m.List(context.Background(), "manifests/")
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("quota refusal = %v", err)
	}
	if quotaCalls != 1 || okCalls != 0 {
		t.Fatalf("hooks: quota=%d ok=%d", quotaCalls, okCalls)
	}
	// a normal failure is not mistaken for it
	fake.quota = false
	c.Token = "wrong"
	if _, err := m.List(context.Background(), "manifests/"); err == nil || errors.Is(err, ErrQuota) {
		t.Fatalf("auth failure = %v", err)
	}
	// once calls go through again, OnOK fires, once
	c.Token = fake.token
	if _, err := m.List(context.Background(), "manifests/"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.List(context.Background(), "manifests/"); err != nil {
		t.Fatal(err)
	}
	if okCalls != 1 {
		t.Fatalf("OnOK fired %d times", okCalls)
	}
}

// With the directory keys remembered, a poll for manifests is one list call:
// the account root and the vault folder are not listed again. Keys that no
// longer exist are noticed and learned again.
func TestMirrorRemembersDirectoryKeys(t *testing.T) {
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
	a := mk()
	for p, d := range map[string]string{"manifests/dev_a/000000000001.mf": "m", "objects/ab/abcdef.obj": "o"} {
		if err := a.Put(ctx, p, strings.NewReader(d)); err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	fake.listed = map[string]int{}
	fake.mu.Unlock()
	b := mk() // a new process: nothing in memory, the cache file remembers
	if es, err := b.List(ctx, "manifests/"); err != nil || len(es) != 1 {
		t.Fatalf("poll: %v %v", err, es)
	}
	fake.mu.Lock()
	n := 0
	for _, c := range fake.listed {
		n += c
	}
	fake.mu.Unlock()
	if n != 1 {
		t.Fatalf("a poll with remembered keys made %d list calls, want 1 (the manifests directory)", n)
	}
	// the remembered keys go stale (folder recreated elsewhere): learned again
	bad, _ := json.Marshal(keyCacheFile{Root: "SessionMemory", Key: "root-gone", Dirs: map[string]string{"manifests": "gone"}})
	os.WriteFile(cache, bad, 0o600)
	c := mk()
	if es, err := c.List(ctx, "manifests/"); err != nil || len(es) != 1 {
		t.Fatalf("poll with stale keys: %v %v", err, es)
	}
	var back keyCacheFile
	raw, _ := os.ReadFile(cache)
	json.Unmarshal(raw, &back)
	if back.Key == "root-gone" || back.Dirs["manifests"] == "gone" || back.Dirs["manifests"] == "" {
		t.Fatalf("cache not repaired: %+v", back)
	}
}
