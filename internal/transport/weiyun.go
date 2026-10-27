package transport

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// WeiyunURL is the MCP endpoint of the Tencent Weiyun cloud drive.
const WeiyunURL = "https://www.weiyun.com/api/v3/mcpserver"

const weiyunBlock = 524288

// WeiyunClient is the small part of Weiyun's MCP interface the mirror needs.
// The token is the user's own WEIYUN_MCP_TOKEN; it is sent only to URL.
type WeiyunClient struct {
	URL   string
	Token string
	HTTP  *http.Client
	id    int64
}

// NewWeiyunClient returns a client for the public endpoint.
func NewWeiyunClient(token string) *WeiyunClient {
	return &WeiyunClient{URL: WeiyunURL, Token: token, HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

type wyDir struct{ Key, Name string }
type wyFile struct {
	ID, Name, PDir string
	Size           int64
	MTime          time.Time
}

func (c *WeiyunClient) call(ctx context.Context, tool string, args map[string]any, out any) error {
	c.id++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("WyHeader", "mcp_token="+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("weiyun: %s: HTTP %d", tool, resp.StatusCode)
	}
	var env struct {
		Result struct {
			Content []struct{ Type, Text string } `json:"content"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("weiyun: %s: %w", tool, err)
	}
	if env.Error != nil {
		return fmt.Errorf("weiyun: %s: %s", tool, env.Error.Message)
	}
	for _, it := range env.Result.Content {
		if it.Type != "text" {
			continue
		}
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal([]byte(it.Text), &e)
		if e.Error != "" {
			return fmt.Errorf("weiyun: %s: %s", tool, e.Error)
		}
		if out != nil {
			return json.Unmarshal([]byte(it.Text), out)
		}
		return nil
	}
	return fmt.Errorf("weiyun: %s: empty response", tool)
}

// list returns every directory and file directly under dirKey ("" is the
// token's own root).
func (c *WeiyunClient) list(ctx context.Context, dirKey string) (dirs []wyDir, files []wyFile, rootKey string, err error) {
	for off := 0; ; {
		args := map[string]any{"limit": 50, "offset": off, "order_by": 1, "asc": true}
		if dirKey != "" {
			args["dir_key"] = dirKey
		}
		var r struct {
			PDirKey string `json:"pdir_key"`
			DirList []struct {
				Key  string `json:"dir_key"`
				Name string `json:"dir_name"`
			} `json:"dir_list"`
			FileList []struct {
				ID    string `json:"file_id"`
				Name  string `json:"filename"`
				Size  int64  `json:"file_size"`
				MTime int64  `json:"file_mtime"`
				PDir  string `json:"pdir_key"`
			} `json:"file_list"`
			Finish bool `json:"finish_flag"`
		}
		if err = c.call(ctx, "weiyun.list", args, &r); err != nil {
			return
		}
		rootKey = r.PDirKey
		// File items come back without their directory key (observed against
		// the live service); the directory we listed is that key.
		here := dirKey
		if here == "" {
			here = r.PDirKey
		}
		for _, d := range r.DirList {
			dirs = append(dirs, wyDir{d.Key, d.Name})
		}
		for _, f := range r.FileList {
			if f.PDir == "" {
				f.PDir = here
			}
			files = append(files, wyFile{ID: f.ID, Name: f.Name, PDir: f.PDir, Size: f.Size, MTime: time.UnixMilli(f.MTime)})
		}
		n := len(r.DirList) + len(r.FileList)
		if r.Finish || n == 0 {
			return
		}
		off += n
	}
}

// ensureDir returns the key of the named subdirectory of parent, creating it.
func (c *WeiyunClient) ensureDir(ctx context.Context, parent, name string) (string, error) {
	dirs, _, _, err := c.list(ctx, parent)
	if err != nil {
		return "", err
	}
	for _, d := range dirs {
		if d.Name == name {
			return d.Key, nil
		}
	}
	args := map[string]any{"dir_name": name}
	if parent != "" {
		args["pdir_key"] = parent
	}
	var r struct {
		Key  string `json:"dir_key"`
		Name string `json:"dir_name"`
	}
	if err := c.call(ctx, "weiyun.create_dir", args, &r); err != nil {
		return "", err
	}
	if r.Name != name {
		return "", fmt.Errorf("weiyun: directory %q was renamed to %q", name, r.Name)
	}
	return r.Key, nil
}

func (c *WeiyunClient) download(ctx context.Context, f wyFile) ([]byte, error) {
	var r struct {
		Items []struct {
			URL    string `json:"https_download_url"`
			Cookie string `json:"cookie"`
		} `json:"items"`
	}
	if err := c.call(ctx, "weiyun.download", map[string]any{"items": []map[string]string{{"file_id": f.ID, "pdir_key": f.PDir}}}, &r); err != nil {
		return nil, err
	}
	if len(r.Items) != 1 || r.Items[0].URL == "" {
		return nil, fmt.Errorf("weiyun: no download link for %s", f.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Items[0].URL, nil)
	if err != nil {
		return nil, err
	}
	if r.Items[0].Cookie != "" {
		req.Header.Set("Cookie", r.Items[0].Cookie)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("weiyun: download %s: HTTP %d", f.Name, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if f.Size > 0 && int64(len(data)) != f.Size {
		return nil, fmt.Errorf("weiyun: download %s: got %d bytes, want %d", f.Name, len(data), f.Size)
	}
	return data, nil
}

// sha1State returns h0..h4 of a SHA-1 that has consumed a multiple of 64
// bytes, each as a little-endian word, as Weiyun's upload protocol expects.
func sha1State(h encoding.BinaryMarshaler) string {
	b, _ := h.MarshalBinary() // "sha\x01" | h0..h4 big-endian | buffer | length
	var out [20]byte
	for i := 0; i < 5; i++ {
		binary.LittleEndian.PutUint32(out[i*4:], binary.BigEndian.Uint32(b[4+i*4:]))
	}
	return hex.EncodeToString(out[:])
}

type wyUploadParams struct {
	Size      int64
	FileSHA   string
	MD5       string
	Blocks    []string
	CheckSHA  string
	CheckData string
}

func calcUploadParams(data []byte) wyUploadParams {
	size := int64(len(data))
	last := size % weiyunBlock
	if last == 0 {
		last = weiyunBlock
	}
	check := last % 128
	if check == 0 {
		check = 128
	}
	before := size - last
	h := sha1.New()
	m := md5.New()
	p := wyUploadParams{Size: size}
	for off := int64(0); off < before; off += weiyunBlock {
		blk := data[off : off+weiyunBlock]
		h.Write(blk)
		m.Write(blk)
		p.Blocks = append(p.Blocks, sha1State(h.(encoding.BinaryMarshaler)))
	}
	mid := data[before : size-check]
	h.Write(mid)
	m.Write(mid)
	p.CheckSHA = sha1State(h.(encoding.BinaryMarshaler))
	tail := data[size-check:]
	h.Write(tail)
	m.Write(tail)
	p.FileSHA = hex.EncodeToString(h.Sum(nil))
	p.MD5 = hex.EncodeToString(m.Sum(nil))
	p.CheckData = base64.StdEncoding.EncodeToString(tail)
	p.Blocks = append(p.Blocks, p.FileSHA)
	return p
}

func (c *WeiyunClient) upload(ctx context.Context, pdirKey, name string, data []byte) error {
	if len(data) == 0 {
		return errors.New("weiyun: cannot upload an empty file")
	}
	p := calcUploadParams(data)
	pre := map[string]any{"filename": name, "file_size": p.Size, "file_sha": p.FileSHA, "file_md5": p.MD5,
		"block_sha_list": p.Blocks, "check_sha": p.CheckSHA, "check_data": p.CheckData, "pdir_key": pdirKey}
	type channel struct {
		ID     int64 `json:"id"`
		Offset int64 `json:"offset"`
		Len    int64 `json:"len"`
	}
	for round := 0; round < 200; round++ {
		var r struct {
			Exist    bool      `json:"file_exist"`
			Key      string    `json:"upload_key"`
			Ex       string    `json:"ex"`
			Channels []channel `json:"channel_list"`
			State    int       `json:"upload_state"`
		}
		if err := c.call(ctx, "weiyun.upload", pre, &r); err != nil {
			return err
		}
		if r.Exist {
			return nil
		}
		var ch *channel
		for i := range r.Channels {
			if r.Channels[i].Len > 0 {
				ch = &r.Channels[i]
				break
			}
		}
		if ch == nil {
			if r.State == 2 {
				return nil
			}
			return fmt.Errorf("weiyun: upload of %s has no channel (state %d)", name, r.State)
		}
		end := ch.Offset + ch.Len
		if end > p.Size {
			end = p.Size
		}
		list := make([]channel, len(r.Channels))
		copy(list, r.Channels)
		var up struct {
			State int `json:"upload_state"`
		}
		if err := c.call(ctx, "weiyun.upload", map[string]any{"filename": name, "file_size": p.Size, "file_sha": p.FileSHA,
			"block_sha_list": []string{}, "check_sha": p.CheckSHA, "upload_key": r.Key, "channel_list": list, "channel_id": ch.ID,
			"ex": r.Ex, "file_data": base64.StdEncoding.EncodeToString(data[ch.Offset:end])}, &up); err != nil {
			return err
		}
		if up.State == 2 {
			return nil
		}
	}
	return fmt.Errorf("weiyun: upload of %s did not finish", name)
}

// ---- Mirror ----

// Mirror is a Transport whose truth is a Weiyun folder and whose working copy
// is a local Folder. Published files are write-once, so replication is by
// name: List pulls files the local copy lacks, Put pushes after writing
// locally. Remote layout is flat per top-level directory: "a/b/c" is file
// "b~c" inside remote directory "a".
type Mirror struct {
	Local *Folder
	C     *WeiyunClient
	// Root is the Weiyun directory name that holds the vault.
	Root string

	mu      sync.Mutex
	rootKey string
	dirKeys map[string]string
	remote  map[string]wyFile // key: slash path
	fresh   time.Time
	TTL     time.Duration
}

// NewMirror returns a Mirror over local.
func NewMirror(local *Folder, c *WeiyunClient, root string) *Mirror {
	return &Mirror{Local: local, C: c, Root: root, TTL: 15 * time.Second}
}

const wySep = "~"

func splitRemote(path string) (dir, name string) {
	dir, rest, ok := strings.Cut(path, "/")
	if !ok {
		return "", path
	}
	return dir, strings.ReplaceAll(rest, "/", wySep)
}

func joinRemote(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + strings.ReplaceAll(name, wySep, "/")
}

func (m *Mirror) root(ctx context.Context) (string, error) {
	if m.rootKey != "" {
		return m.rootKey, nil
	}
	k, err := m.C.ensureDir(ctx, "", m.Root)
	if err != nil {
		return "", err
	}
	m.rootKey = k
	m.dirKeys = map[string]string{"": k}
	return k, nil
}

func (m *Mirror) refresh(ctx context.Context, force bool) error {
	if !force && m.remote != nil && time.Since(m.fresh) < m.TTL {
		return nil
	}
	rk, err := m.root(ctx)
	if err != nil {
		return err
	}
	dirs, files, _, err := m.C.list(ctx, rk)
	if err != nil {
		return err
	}
	remote := map[string]wyFile{}
	for _, f := range files {
		remote[f.Name] = f
	}
	for _, d := range dirs {
		m.dirKeys[d.Name] = d.Key
		_, fs, _, err := m.C.list(ctx, d.Key)
		if err != nil {
			return err
		}
		for _, f := range fs {
			remote[joinRemote(d.Name, f.Name)] = f
		}
	}
	m.remote, m.fresh = remote, time.Now()
	return nil
}

// Pull downloads every remote file the local copy lacks.
func (m *Mirror) Pull(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pullLocked(ctx, true)
}

func (m *Mirror) pullLocked(ctx context.Context, force bool) error {
	if err := m.refresh(ctx, force); err != nil {
		return err
	}
	have, err := m.Local.List(ctx, "")
	if err != nil {
		return err
	}
	local := map[string]bool{}
	for _, e := range have {
		local[e.Path] = true
	}
	paths := make([]string, 0, len(m.remote))
	for p := range m.remote {
		if !local[p] && !strings.Contains(p, PartialSuffix) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		data, err := m.C.download(ctx, m.remote[p])
		if err != nil {
			return err
		}
		if err := m.Local.Put(ctx, p, bytes.NewReader(data)); err != nil && !errors.Is(err, ErrExists) {
			return err
		}
	}
	return nil
}

// Push uploads every local file the remote lacks.
func (m *Mirror) Push(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refresh(ctx, true); err != nil {
		return err
	}
	have, err := m.Local.List(ctx, "")
	if err != nil {
		return err
	}
	for _, e := range have {
		if err := m.pushLocked(ctx, e.Path); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mirror) pushLocked(ctx context.Context, path string) error {
	if r, ok := m.remote[path]; ok {
		_ = r
		return nil
	}
	rc, err := m.Local.Get(ctx, path)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return err
	}
	rk, err := m.root(ctx)
	if err != nil {
		return err
	}
	dir, name := splitRemote(path)
	key, ok := m.dirKeys[dir]
	if !ok {
		if key, err = m.C.ensureDir(ctx, rk, dir); err != nil {
			return err
		}
		m.dirKeys[dir] = key
	}
	if dir == "" {
		key = rk
	}
	if err := m.C.upload(ctx, key, name, data); err != nil {
		return err
	}
	m.remote[path] = wyFile{Name: name, PDir: key, Size: int64(len(data))}
	return nil
}

// Put writes locally (write-once), then publishes to Weiyun.
func (m *Mirror) Put(ctx context.Context, path string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if err := m.Local.Put(ctx, path, bytes.NewReader(data)); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refresh(ctx, false); err != nil {
		return err
	}
	if r, ok := m.remote[path]; ok {
		if r.Size != int64(len(data)) {
			return ErrExists
		}
		return nil
	}
	return m.pushLocked(ctx, path)
}

// List pulls what the local copy lacks, then lists the local copy.
func (m *Mirror) List(ctx context.Context, prefix string) ([]Entry, error) {
	m.mu.Lock()
	err := m.pullLocked(ctx, false)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return m.Local.List(ctx, prefix)
}

// Get opens a file from the local copy, fetching it first if needed.
func (m *Mirror) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	rc, err := m.Local.Get(ctx, path)
	if err == nil {
		return rc, nil
	}
	m.mu.Lock()
	perr := m.pullLocked(ctx, true)
	m.mu.Unlock()
	if perr != nil {
		return nil, perr
	}
	return m.Local.Get(ctx, path)
}

// Stable reports on the local copy.
func (m *Mirror) Stable(ctx context.Context, e Entry) (bool, error) { return m.Local.Stable(ctx, e) }

// RemoveTestVault moves a throwaway remote vault directory to the recycle bin. It
// refuses any name that does not start with "SessionMemory-test-", so it can
// never touch a real vault.
func (m *Mirror) RemoveTestVault(ctx context.Context) error {
	if !strings.HasPrefix(m.Root, "SessionMemory-test-") {
		return fmt.Errorf("weiyun: refusing to delete %q", m.Root)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	dirs, _, parent, err := m.C.list(ctx, "")
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if d.Name == m.Root {
			return m.C.call(ctx, "weiyun.delete", map[string]any{"dir_list": []map[string]string{{"dir_key": d.Key, "pdir_key": parent, "dir_name": d.Name}}, "delete_completely": false}, nil)
		}
	}
	return nil
}
