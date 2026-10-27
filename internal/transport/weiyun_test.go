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
