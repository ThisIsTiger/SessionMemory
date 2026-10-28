package workbuddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/canonjson"
	"session-memory/internal/errcode"
	"session-memory/internal/failpoint"
	"session-memory/internal/jsonl"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/store"
)

const WriterVersion = "workbuddy-writer/0.1-registry"

// Registry writes are opt-in, manual, and limited to the experimentally
// verified vendor version and registry schema.
type Writer struct {
	AllowRegistryWrite bool
	ClientClosed       bool
	version            func(context.Context) error
	closed             func(context.Context, string) error
}

func (a *Adapter) Writer() agent.Writer { return a.WriterWith(false) }
func (a *Adapter) WriterWith(closed bool) agent.Writer {
	return &Writer{AllowRegistryWrite: a.Cfg.Agents.WorkBuddy.AllowRegistryWrite, ClientClosed: closed, version: checkVersion, closed: checkClosed}
}
func (w *Writer) defaults() {
	if w.version == nil {
		w.version = checkVersion
	}
	if w.closed == nil {
		w.closed = checkClosed
	}
}
func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func sqliteURI(path, mode string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=" + mode}
	return u.String()
}

// Reject symlinks at every existing component, including the final file.
func safeTarget(home, rel string) (string, error) {
	if !filepath.IsAbs(home) || !session.ValidArtifactPath(rel) {
		return "", errcode.New(errcode.UnknownFormat, "invalid WorkBuddy target path")
	}
	home = filepath.Clean(home)
	st, err := os.Lstat(home)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", errcode.New(errcode.NeedsAction, "WorkBuddy home must be an existing real directory")
	}
	cur := home
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", errcode.New(errcode.NeedsAction, "WorkBuddy target contains a symlink")
		}
	}
	return cur, nil
}

var nativeIDPattern = regexp.MustCompile("^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$")
var supportedCwd = regexp.MustCompile("^/[A-Za-z0-9._/-]+$")

func mainArtifact(src agent.SnapshotReader) (session.Artifact, string, error) {
	var main session.Artifact
	id := ""
	for _, art := range src.Artifacts() {
		parts := strings.Split(art.Path, "/")
		if len(parts) == 3 && parts[0] == "projects" && strings.HasSuffix(parts[2], ".jsonl") {
			if id != "" {
				return main, "", errcode.New(errcode.UnknownFormat, "multiple WorkBuddy main streams")
			}
			main = art
			id = strings.TrimSuffix(parts[2], ".jsonl")
		}
	}
	if !nativeIDPattern.MatchString(id) {
		return main, "", errcode.New(errcode.UnknownFormat, "snapshot does not identify a WorkBuddy session")
	}
	return main, id, nil
}

// Patch only the known top-level cwd string, retaining every other source
// byte. This is necessary for native fast-forward after a path-remapped turn.
func mapCwd(line []byte, pm agent.PathMap) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errcode.New(errcode.UnknownFormat, "invalid WorkBuddy record")
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if key != "cwd" {
			continue
		}
		var cwd string
		if json.Unmarshal(raw, &cwd) != nil {
			return nil, errcode.New(errcode.UnknownFormat, "invalid WorkBuddy cwd")
		}
		mapped, ok := pm.Apply(cwd)
		if !ok || mapped == cwd {
			return line, nil
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		encoded, _ := json.Marshal(mapped)
		out := append([]byte{}, line[:start]...)
		out = append(out, encoded...)
		out = append(out, line[end:]...)
		return out, nil
	}
	return line, nil
}

type fileBaseline struct {
	Hash string
	Mode uint32
}
type nativeBuild struct {
	id            string
	files         map[string][]byte
	beforeFiles   map[string]fileBaseline
	before, after row
	baseline      string
}

// Bound per-artifact allocations while the writer is still manual/opt-in.
const maxWriteArtifact = 64 << 20

func readArtifact(ctx context.Context, src agent.SnapshotReader, art session.Artifact, pm agent.PathMap) ([]byte, error) {
	rc, err := src.Open(ctx, art.Path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxWriteArtifact+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxWriteArtifact {
		return nil, errcode.New(errcode.Unsupported, "WorkBuddy writer currently limits each artifact to 64 MiB")
	}
	if art.Kind == session.KindStream {
		if art.Form != session.FormPlain {
			return nil, errcode.New(errcode.Unsupported, "WorkBuddy writer requires uncompressed streams")
		}
		r := jsonl.NewReader(io.NopCloser(bytes.NewReader(b)), 0, jsonl.Options{RequireJSON: true, MaxRecordBytes: jsonl.DefaultMaxRecordBytes})
		defer r.Close()
		var out bytes.Buffer
		for {
			rec, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if rec.Blank() {
				out.Write(rec.Line)
				continue
			}
			mapped, err := mapCwd(rec.Line, pm)
			if err != nil {
				return nil, err
			}
			out.Write(mapped)
		}
		return out.Bytes(), nil
	}
	if strings.HasSuffix(art.Path, ".meta.json") {
		return mapCwd(b, pm)
	}
	return b, nil
}

// WorkBuddy keeps a "task" (a session that belongs to no project folder) in a
// folder it makes itself: ~/WorkBuddy/<yyyy-mm-dd-hh-mm-ss>. Its registry row
// carries is_playground=1, which is what puts it under 任务 instead of 空间.
var taskDirPattern = regexp.MustCompile(`^(/.+)/WorkBuddy/(\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2})$`)

func isTaskDir(dir string) bool { return taskDirPattern.MatchString(filepath.Clean(dir)) }

// TaskWorkspace maps a task folder recorded on another computer to the same
// task folder under this user's home. Other folders are not task folders.
func (a *Adapter) TaskWorkspace(origin string) (string, bool) {
	m := taskDirPattern.FindStringSubmatch(filepath.Clean(origin))
	if m == nil {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, "WorkBuddy", m[2]), true
}

func (a *Adapter) Workspace(ctx context.Context, src agent.SnapshotReader) (string, error) {
	main, _, err := mainArtifact(src)
	if err != nil {
		return "", err
	}
	rc, err := src.Open(ctx, main.Path)
	if err != nil {
		return "", err
	}
	r := jsonl.NewReader(rc, 0, jsonl.Options{MaxRecordBytes: jsonl.DefaultMaxRecordBytes})
	defer r.Close()
	for i := 0; i < 400; i++ {
		rec, err := r.Next()
		if err != nil {
			break
		}
		var head struct{ Cwd string }
		if json.Unmarshal(rec.Line, &head) == nil && head.Cwd != "" {
			return head.Cwd, nil
		}
	}
	return "", errcode.New(errcode.NeedsAction, "WorkBuddy snapshot has no workspace; select --cwd")
}

func (a *Adapter) SnapshotIdentity(_ context.Context, src agent.SnapshotReader) (string, error) {
	_, id, err := mainArtifact(src)
	return id, err
}

func (w *Writer) build(ctx context.Context, in agent.WriteInput) (nativeBuild, error) {
	b := nativeBuild{files: map[string][]byte{}, beforeFiles: map[string]fileBaseline{}}
	if !w.AllowRegistryWrite {
		return b, errcode.New(errcode.Unsupported, "enable agents.workbuddy.allow_registry_write to use the version-limited native writer")
	}
	w.defaults()
	if err := w.version(ctx); err != nil {
		return b, err
	}
	if in.Mode != agent.ModeNative || in.SourceProfile.Agent != AgentName || in.SourceProfile.Format != "workbuddy-jsonl" || in.Target.Agent != AgentName {
		return b, errcode.New(errcode.Unsupported, "WorkBuddy writer currently accepts native WorkBuddy snapshots only")
	}
	main, id, err := mainArtifact(in.Source)
	if err != nil {
		return b, err
	}
	b.id = id
	if !supportedCwd.MatchString(in.Cwd) || strings.Contains(in.Cwd, "//") {
		return b, errcode.New(errcode.Unsupported, "WorkBuddy path encoding is currently verified for absolute ASCII paths without spaces only")
	}
	dir := strings.Trim(strings.ReplaceAll(in.Cwd, "/", "-"), "-")
	var first, last int64
	var title, custom string
	total := 0
	for _, art := range in.Source.Artifacts() {
		if !session.ValidArtifactPath(art.Path) {
			return b, errcode.New(errcode.UnknownFormat, "invalid WorkBuddy artifact path")
		}
		parts := strings.SplitN(art.Path, "/", 3)
		if len(parts) != 3 || parts[0] != "projects" {
			return b, errcode.New(errcode.Unsupported, "unknown WorkBuddy artifact location")
		}
		tail := parts[2]
		known := tail == id+".jsonl" || tail == id+".meta.json" || tail == id+".file-rollback.ndjson" ||
			strings.HasPrefix(tail, id+"/subagents/") || strings.HasPrefix(tail, id+"/tool-results/")
		if !known {
			return b, errcode.New(errcode.Unsupported, "unknown WorkBuddy artifact")
		}
		rel := "projects/" + dir + "/" + tail
		if _, exists := b.files[rel]; exists {
			return b, errcode.New(errcode.UnknownFormat, "duplicate mapped artifact")
		}
		content, err := readArtifact(ctx, in.Source, art, in.PathMap)
		if err != nil {
			return b, err
		}
		total += len(content)
		if total > 128<<20 {
			return b, errcode.New(errcode.Unsupported, "WorkBuddy writer currently limits one install to 128 MiB")
		}
		b.files[rel] = content
		path, err := safeTarget(in.TargetState.Home, rel)
		if err != nil {
			return b, err
		}
		if current, err := os.ReadFile(path); err == nil {
			st, err := os.Stat(path)
			if err != nil {
				return b, err
			}
			b.beforeFiles[rel] = fileBaseline{hashBytes(current), uint32(st.Mode().Perm())}
			if in.BindingID == "" || !bytes.HasPrefix(content, current) || (art.Kind == session.KindBlob && !bytes.Equal(content, current)) {
				return b, errcode.New(errcode.NeedsAction, "WorkBuddy target has local or unmanaged content")
			}
		} else if !os.IsNotExist(err) {
			return b, err
		}
		if art.Path == main.Path {
			for _, line := range bytes.Split(content, []byte("\n")) {
				var head struct {
					Type, SessionId, AITitle, CustomTitle string
					Timestamp                             int64
				}
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				if json.Unmarshal(line, &head) != nil {
					return b, errcode.New(errcode.UnknownFormat, "invalid WorkBuddy main record")
				}
				if head.SessionId != "" && head.SessionId != id {
					return b, errcode.New(errcode.UnknownFormat, "WorkBuddy session id mismatch")
				}
				if head.Timestamp > 0 {
					if first == 0 || head.Timestamp < first {
						first = head.Timestamp
					}
					if head.Timestamp > last {
						last = head.Timestamp
					}
				}
				if head.Type == "ai-title" {
					title = head.AITitle
				}
				if head.Type == "custom-title" {
					custom = head.CustomTitle
				}
			}
		}
	}
	if first == 0 {
		return b, errcode.New(errcode.UnknownFormat, "WorkBuddy snapshot has no record timestamps")
	}
	if found := scan(in.TargetState.Home)[id]; found != nil {
		for _, file := range found.files {
			if _, ok := b.files[file.rel]; !ok {
				return b, errcode.New(errcode.NeedsAction, "WorkBuddy target has additional native artifacts")
			}
		}
	}
	db, err := openRegistry(in.TargetState.Home, false)
	if err != nil {
		return b, err
	}
	defer db.Close()
	defaults, err := registryDefaults(ctx, db)
	if err != nil {
		return b, err
	}
	user, err := localUser(ctx, db)
	if err != nil {
		return b, err
	}
	b.before, err = readRow(ctx, db, id)
	if err != nil {
		return b, err
	}
	b.after = defaults
	if b.before != nil {
		if in.BindingID == "" || b.before["deleted_at"] != nil || b.before["user_id"] != user {
			return b, errcode.New(errcode.NeedsAction, "WorkBuddy registry row is unmanaged, deleted or belongs to another account")
		}
		b.after = row{}
		for k, v := range b.before {
			b.after[k] = v
		}
	} else {
		b.after["id"] = id
		b.after["user_id"] = user
		b.after["created_at"] = first
		if origin, err := (&Adapter{}).Workspace(ctx, in.Source); err == nil && isTaskDir(origin) && isTaskDir(in.Cwd) {
			b.after["is_playground"] = int64(1) // a task stays a task here, not a project
			b.after["mode"] = "craft"
			b.after["source_mode"] = "craft"
		}
		// Model, permissions and account-private settings are target defaults.
	}
	if custom != "" {
		title = custom
	}
	if title == "" {
		title = "Synced session"
	}
	if b.after["custom_title"] == nil {
		b.after["title"] = title
	}
	b.after["cwd"] = in.Cwd
	b.after["updated_at"] = last
	b.after["last_activity_at"] = last
	b.after["status"] = "completed"
	baseline, err := json.Marshal(struct {
		Files  map[string]fileBaseline
		Row    row
		User   string
		Schema string
	}{b.beforeFiles, b.before, user, registrySchema})
	b.baseline = hashBytes(baseline)
	return b, err
}

func (w *Writer) Plan(ctx context.Context, in agent.WriteInput) (agent.Plan, error) {
	p := agent.Plan{SchemaVersion: 1, SessionID: in.SessionID, RevisionID: in.RevisionID, Mode: in.Mode, Route: agent.RouteRegistry, Writes: []agent.PlannedWrite{}, Warnings: []string{"workbuddy_manual_registry_write"}}
	b, err := w.build(ctx, in)
	if err != nil {
		return p, err
	}
	p.BaselineHash = b.baseline
	for _, rel := range sortedKeys(b.files) {
		content := b.files[rel]
		op := "create"
		if before, ok := b.beforeFiles[rel]; ok {
			if before.Hash == hashBytes(content) {
				continue
			}
			op = "append"
		}
		p.Writes = append(p.Writes, agent.PlannedWrite{Path: rel, Op: op, Size: int64(len(content)), SHA256: hashBytes(content)})
	}
	if rowHash(b.before) != rowHash(b.after) {
		op := "row_insert"
		if b.before != nil {
			op = "row_update"
		}
		p.Writes = append(p.Writes, agent.PlannedWrite{Path: "sessions#" + b.id, Op: op, SHA256: rowHash(b.after)})
	}
	p.Digest, _ = canonjson.Hash(p)
	return p, nil
}

func (w *Writer) Apply(ctx context.Context, in agent.WriteInput, p agent.Plan, tx agent.Tx) (receipt agent.Receipt, applyErr error) {
	w.defaults()
	check := p
	check.Digest = ""
	digest, err := canonjson.Hash(check)
	if err != nil || digest != p.Digest {
		return receipt, errcode.New(errcode.BaselineChanged, "WorkBuddy plan digest mismatch")
	}
	t, ok := tx.(store.Tx)
	if !ok {
		return receipt, errcode.New(errcode.Internal, "writer needs a store transaction")
	}
	if !in.VendorLocked {
		release, err := w.LockInstall(ctx, in)
		if err != nil {
			return receipt, err
		}
		defer release()
	}
	b, err := w.build(ctx, in)
	if err != nil {
		return receipt, err
	}
	fresh, err := w.Plan(ctx, in)
	if err != nil {
		return receipt, err
	}
	aHash, _ := canonjson.Hash(fresh.Writes)
	pHash, _ := canonjson.Hash(p.Writes)
	if b.baseline != p.BaselineHash || aHash != pHash {
		return receipt, errcode.New(errcode.BaselineChanged, "WorkBuddy source or target changed after planning")
	}
	d := store.InstallData{Agent: AgentName, NativeSessionID: b.id, ClientClosed: w.ClientClosed, PlanDigest: p.Digest, Mode: p.Mode, Route: p.Route, Home: in.TargetState.Home, BackupDir: t.BackupDir()}
	undo := registryBackup{SchemaVersion: 1, NativeID: b.id, Before: b.before, After: b.after, Files: map[string]string{}, BeforeFiles: b.beforeFiles}
	for rel, content := range b.files {
		undo.Files[rel] = hashBytes(content)
	}
	for _, pw := range p.Writes {
		if strings.HasPrefix(pw.Path, "sessions#") {
			continue
		}
		if err := platform.AtomicWrite(filepath.Join(t.StagingDir(), filepath.FromSlash(pw.Path)), b.files[pw.Path], 0600); err != nil {
			return receipt, err
		}
		undo.Files[pw.Path] = pw.SHA256
	}
	if err := t.SetState(store.StStaged, d); err != nil {
		return receipt, err
	}
	defer func() {
		if applyErr != nil && (t.State() == store.StBackedUp || t.State() == store.StWritten) {
			if err := w.CanRollback(context.Background(), in); err != nil {
				applyErr = errcode.Wrap(errcode.RecoveryNeeded, "WorkBuddy started during install; quit it before recovery", err)
				return
			}
			if err := t.Rollback(); err != nil {
				applyErr = errcode.Wrap(errcode.RecoveryNeeded, "WorkBuddy install rollback requires recovery", err)
			}
		}
	}()
	if err := w.closed(ctx, d.Home); err != nil {
		return receipt, err
	}
	if in.ValidateEnvironment != nil {
		if err := in.ValidateEnvironment(ctx); err != nil {
			return receipt, err
		}
	}
	for _, pw := range p.Writes {
		if strings.HasPrefix(pw.Path, "sessions#") {
			continue
		}
		if pw.Op == "create" {
			d.Created = append(d.Created, pw.Path)
			continue
		}
		path, err := safeTarget(d.Home, pw.Path)
		if err != nil {
			return receipt, err
		}
		old, err := os.ReadFile(path)
		if err != nil {
			return receipt, err
		}
		if hashBytes(old) != b.beforeFiles[pw.Path].Hash {
			return receipt, errcode.New(errcode.BaselineChanged, "WorkBuddy file changed before backup")
		}
		if err := platform.AtomicWrite(filepath.Join(d.BackupDir, filepath.FromSlash(pw.Path)), old, os.FileMode(b.beforeFiles[pw.Path].Mode)); err != nil {
			return receipt, err
		}
		d.Backed = append(d.Backed, pw.Path)
	}
	data, err := json.Marshal(undo)
	if err != nil {
		return receipt, err
	}
	if err := platform.AtomicWrite(filepath.Join(d.BackupDir, registryBackupName), data, 0600); err != nil {
		return receipt, err
	}
	d.RegistryUndo = registryBackupName
	if err := t.SetState(store.StBackedUp, d); err != nil {
		return receipt, err
	}
	for i, pw := range p.Writes {
		if err := w.closed(ctx, d.Home); err != nil {
			return receipt, err
		}
		if i == len(p.Writes)/2 {
			if err := failpoint.Hit("install.mid_write"); err != nil {
				return receipt, err
			}
		}
		if strings.HasPrefix(pw.Path, "sessions#") {
			if err := swapRow(ctx, d.Home, b.id, b.before, b.after); err != nil {
				return receipt, err
			}
			if err := failpoint.Hit("install.after_registry"); err != nil {
				return receipt, err
			}
		} else {
			path, err := safeTarget(d.Home, pw.Path)
			if err != nil {
				return receipt, err
			}
			old, err := os.ReadFile(path)
			expected, existed := b.beforeFiles[pw.Path]
			if (existed && (err != nil || hashBytes(old) != expected.Hash)) || (!existed && !os.IsNotExist(err)) {
				return receipt, errcode.New(errcode.BaselineChanged, "WorkBuddy target changed before write")
			}
			mode := os.FileMode(0600)
			if existed {
				mode = os.FileMode(expected.Mode)
			}
			if err := platform.AtomicWrite(path, b.files[pw.Path], mode); err != nil {
				return receipt, err
			}
		}
		d.Written = append(d.Written, pw)
	}
	if err := t.SetState(store.StWritten, d); err != nil {
		return receipt, err
	}
	receipt = agent.Receipt{SessionID: in.SessionID, RevisionID: in.RevisionID, NativeSessionID: b.id, InstallMode: session.InstallNative, Written: p.Writes, At: session.FormatTime(time.Now())}
	expected := map[string]fileBaseline{}
	for rel, content := range b.files {
		mode := uint32(0600)
		if previous, ok := b.beforeFiles[rel]; ok {
			mode = previous.Mode
		}
		expected[rel] = fileBaseline{hashBytes(content), mode}
	}
	receipt.TargetHash = postconditionHash(expected, b.after)
	if err := w.Verify(ctx, in, receipt); err != nil {
		return receipt, err
	}
	var count, boundary int64
	for _, content := range b.files {
		count += int64(bytes.Count(content, []byte("\n")))
		boundary += int64(len(content))
	}
	receipt.Import = &session.ImportReceipt{WriterVersion: WriterVersion, SourceRevision: in.RevisionID, WrittenCount: count, BoundaryOffset: boundary, At: receipt.At}
	return receipt, nil
}

func (w *Writer) Verify(ctx context.Context, in agent.WriteInput, r agent.Receipt) error {
	w.defaults()
	if err := w.closed(ctx, in.TargetState.Home); err != nil {
		return err
	}
	files := map[string]fileBaseline{}
	native := scan(in.TargetState.Home)[r.NativeSessionID]
	if native == nil {
		return errcode.New(errcode.TargetRejected, "WorkBuddy native session disappeared")
	}
	for _, file := range native.files {
		path, err := safeTarget(in.TargetState.Home, file.rel)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		files[file.rel] = fileBaseline{hashBytes(content), uint32(st.Mode().Perm())}
	}
	db, err := openRegistry(in.TargetState.Home, false)
	if err != nil {
		return err
	}
	current, err := readRow(ctx, db, r.NativeSessionID)
	db.Close()
	if err != nil {
		return err
	}
	if r.TargetHash == "" || postconditionHash(files, current) != r.TargetHash {
		return errcode.New(errcode.TargetRejected, "WorkBuddy native state changed before binding commit")
	}
	for _, pw := range r.Written {
		if strings.HasPrefix(pw.Path, "sessions#") {
			db, err := openRegistry(in.TargetState.Home, false)
			if err != nil {
				return err
			}
			current, err := readRow(ctx, db, r.NativeSessionID)
			db.Close()
			if err != nil {
				return err
			}
			if rowHash(current) != pw.SHA256 {
				return errcode.New(errcode.TargetRejected, "WorkBuddy registry verification failed")
			}
		} else {
			path, err := safeTarget(in.TargetState.Home, pw.Path)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if hashBytes(b) != pw.SHA256 {
				return errcode.New(errcode.TargetRejected, "WorkBuddy file verification failed")
			}
		}
	}
	return nil
}

func postconditionHash(files map[string]fileBaseline, r row) string {
	b, _ := json.Marshal(struct {
		Files map[string]fileBaseline
		Row   row
	}{files, r})
	return hashBytes(b)
}

func (w *Writer) Rollback(ctx context.Context, home string, tx agent.Tx) error {
	w.defaults()
	if err := w.closed(ctx, home); err != nil {
		return errcode.Wrap(errcode.RecoveryNeeded, "quit WorkBuddy before rollback", err)
	}
	t, ok := tx.(store.Tx)
	if !ok {
		return errcode.New(errcode.Internal, "writer needs a store transaction")
	}
	return t.Rollback()
}

func (w *Writer) CanRollback(ctx context.Context, in agent.WriteInput) error {
	w.defaults()
	return w.closed(ctx, in.TargetState.Home)
}
