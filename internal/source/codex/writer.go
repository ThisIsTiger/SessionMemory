package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

// WriterVersion is recorded in import receipts.
const WriterVersion = "codex-writer/0.1-defaults"

// Writer installs a native revision into a Codex home using the default
// route 2 of spec §8.3 (session files only; Codex registers them itself).
// Its behaviour on a real client is UNVERIFIED until experiments E1a/E1b run;
// it never touches any database and refuses what spec §17 says to refuse.
type Writer struct {
	// ClientClosed is the user's explicit statement that the Codex client is not
	// running (needed while run detection is conservative, spec §8.3).
	ClientClosed bool
}

// WriterFor returns the Writer for this adapter.
func (a *Adapter) WriterWith(clientClosed bool) agent.Writer {
	return &Writer{ClientClosed: clientClosed}
}

func (a *Adapter) Writer() agent.Writer { return &Writer{} }

// nativeProvider is the provider the target client runs with; "" (leave the
// recorded one) when its configuration cannot be read.
func nativeProvider(home string) string {
	p, err := targetProvider(home)
	if err != nil {
		return ""
	}
	return p
}

// providerFor keeps the provider an already installed thread was written with
// (so later appends stay a byte-prefix extension even after the user switches
// provider) and otherwise uses the target client's current one.
func providerFor(home, rel string) string {
	if f, err := os.Open(filepath.Join(home, filepath.FromSlash(rel))); err == nil {
		defer f.Close()
		first, _ := bufio.NewReaderSize(f, 1<<16).Peek(1 << 16)
		if i := bytes.IndexByte(first, '\n'); i >= 0 {
			first = first[:i]
		}
		if m := providerRe.FindSubmatch(first); m != nil && string(m[1]) == "model_provider" {
			var v string
			if json.Unmarshal([]byte(`"`+string(m[2])+`"`), &v) == nil {
				return v
			}
		}
	}
	return nativeProvider(home)
}

var cwdRe = regexp.MustCompile(`"cwd":"((?:[^"\\]|\\.)*)"`)

// mapLine rewrites the structured working-directory fields of the records that
// carry them, at the byte level; every other byte is untouched. Besides
// session_meta and turn_context, Codex 0.164 repeats cwd and
// runtime_workspace_roots in event_msg thread_settings_applied and cwd in
// world_state, and derives the thread's registered cwd from them.
func mapLine(line []byte, pm agent.PathMap, provider string) ([]byte, bool) {
	if len(pm.Rules) == 0 && provider == "" {
		return line, false
	}
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &env) != nil {
		return line, false
	}
	switch env.Type {
	case "session_meta", "turn_context", "world_state":
	case "event_msg":
		if !bytes.Contains(line, []byte(`"thread_settings_applied"`)) {
			return line, false
		}
	default:
		return line, false
	}
	pi := bytes.Index(line, []byte(`"payload":`))
	if pi < 0 {
		return line, false
	}
	head, payload := line[:pi], line[pi:]
	mapped := cwdRe.ReplaceAllFunc(payload, func(m []byte) []byte {
		sub := cwdRe.FindSubmatch(m)
		var old string
		if json.Unmarshal([]byte(`"`+string(sub[1])+`"`), &old) != nil {
			return m
		}
		to, ok := pm.Apply(old)
		if !ok || to == old {
			return m
		}
		return append([]byte(`"cwd":`), jsonString(to)...)
	})
	mapped = mapRoots(mapped, pm)
	mapped = mapProvider(mapped, provider)
	if bytes.Equal(mapped, payload) {
		return line, false
	}
	out := make([]byte, 0, len(line)+len(mapped)-len(payload))
	out = append(out, head...)
	return append(out, mapped...), true
}

var providerRe = regexp.MustCompile(`"(model_provider|model_provider_id)":"((?:[^"\\]|\\.)*)"`)

// mapProvider points the thread's recorded provider at the target Codex's own:
// thread/list only lists threads whose provider equals the one the client runs
// with, so a thread from another machine's provider would stay invisible.
func mapProvider(payload []byte, provider string) []byte {
	if provider == "" {
		return payload
	}
	return providerRe.ReplaceAllFunc(payload, func(m []byte) []byte {
		key := providerRe.FindSubmatch(m)[1]
		out := append([]byte{'"'}, key...)
		out = append(out, '"', ':')
		return append(out, jsonString(provider)...)
	})
}

func jsonString(s string) []byte {
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false)
	e.Encode(s)
	return bytes.TrimRight(enc.Bytes(), "\n")
}

var rootsRe = regexp.MustCompile(`"runtime_workspace_roots":\[((?:[^\]"\\]|"(?:[^"\\]|\\.)*")*)\]`)
var rootItemRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// mapRoots rewrites session_meta.payload.runtime_workspace_roots entries: Codex
// refuses to resume a thread whose roots are not valid absolute paths on the
// host (experiment E1a).
func mapRoots(line []byte, pm agent.PathMap) []byte {
	return rootsRe.ReplaceAllFunc(line, func(whole []byte) []byte {
		loc := rootsRe.FindSubmatchIndex(whole)
		inner := whole[loc[2]:loc[3]]
		mappedInner := rootItemRe.ReplaceAllFunc(inner, func(m []byte) []byte {
			var old string
			if json.Unmarshal(m, &old) != nil {
				return m
			}
			if mp, ok := pm.Apply(old); ok {
				return jsonString(mp)
			}
			return m
		})
		out := make([]byte, 0, len(whole)+len(mappedInner))
		out = append(out, whole[:loc[2]]...)
		out = append(out, mappedInner...)
		return append(out, whole[loc[3]:]...)
	})
}

// buildTarget streams the artifact through mapLine and returns the bytes.
func buildTarget(ctx context.Context, src agent.SnapshotReader, path string, pm agent.PathMap, provider string) ([]byte, bool, error) {
	rc, err := src.Open(ctx, path)
	if err != nil {
		return nil, false, err
	}
	defer rc.Close()
	r := jsonl.NewReader(rc, 0, jsonl.Options{MaxRecordBytes: jsonl.DefaultMaxRecordBytes})
	var out bytes.Buffer
	changed := false
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out.Bytes(), changed, nil
		}
		if err != nil {
			return nil, false, err
		}
		l, c := mapLine(rec.Line, pm, provider)
		changed = changed || c
		out.Write(l)
	}
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// existingState lists target files the plan may touch with their hashes.
func existingState(home string, paths []string) map[string]agent.FileStat {
	out := map[string]agent.FileStat{}
	for _, p := range paths {
		for _, cand := range []string{p, p + ".zst"} {
			if st, err := os.Stat(filepath.Join(home, filepath.FromSlash(cand))); err == nil {
				b, _ := os.ReadFile(filepath.Join(home, filepath.FromSlash(cand)))
				out[cand] = agent.FileStat{Path: cand, Size: st.Size(), ModTime: shaHex(b)}
			}
		}
	}
	return out
}

func baselineOf(m map[string]agent.FileStat) string {
	h, _ := canonjson.Hash(m)
	return h
}

// Plan describes what Apply would do. It reads the target but writes nothing.
func (w *Writer) Plan(ctx context.Context, in agent.WriteInput) (agent.Plan, error) {
	if in.Mode != agent.ModeNative {
		return agent.Plan{}, errcode.New(errcode.Unsupported, "conversion into Codex is not implemented yet")
	}
	if in.SourceProfile.Agent != AgentName || in.SourceProfile.Format != "rollout-jsonl" || in.Target.Agent != AgentName {
		return agent.Plan{}, errcode.New(errcode.UnknownFormat, "Codex native restore only accepts Codex rollout snapshots")
	}
	home := in.TargetState.Home
	plan := agent.Plan{SchemaVersion: 1, SessionID: in.SessionID, RevisionID: in.RevisionID, Mode: in.Mode, Route: agent.RouteFiles}
	var paths []string
	for _, a := range in.Source.Artifacts() {
		if !session.ValidArtifactPath(a.Path) || nativeIDOf(in) == "" {
			return plan, errcode.New(errcode.UnknownFormat, "snapshot does not identify a Codex thread")
		}
		if a.Kind != session.KindStream {
			return plan, errcode.New(errcode.Unsupported, "non-stream artifacts are not supported by the Codex writer")
		}
		paths = append(paths, a.Path)
	}
	sort.Strings(paths)
	ex := existingState(home, paths)
	for _, a := range in.Source.Artifacts() {
		if _, hasZst := ex[a.Path+".zst"]; hasZst {
			return plan, errcode.New(errcode.NeedsAction, "the target thread exists only in compressed form")
		}
	}
	for _, p := range paths {
		content, lenChanged, err := buildTarget(ctx, in.Source, p, in.PathMap, providerFor(in.TargetState.Home, p))
		if err != nil {
			return plan, err
		}
		pw := agent.PlannedWrite{Path: p, Size: int64(len(content)), SHA256: shaHex(content)}
		if cur, ok := ex[p]; !ok {
			pw.Op = "create"
		} else {
			old, _ := os.ReadFile(filepath.Join(home, filepath.FromSlash(p)))
			if !bytes.HasPrefix(content, old) {
				return plan, errcode.New(errcode.NeedsAction, "the target thread has diverged from the revision")
			}
			if int64(len(old)) == int64(len(content)) && cur.ModTime == pw.SHA256 {
				continue // identical already
			}
			pw.Op = "append"
			if lenChanged {
				plan.Warnings = append(plan.Warnings, "path_length_changes")
			}
		}
		plan.Writes = append(plan.Writes, pw)
	}
	plan.BaselineHash = baselineOf(ex)
	d := plan
	d.Digest = ""
	plan.Digest, _ = canonjson.Hash(d)
	return plan, nil
}

// threadLocks takes Codex's coordination lock and the thread's own lock, when
// Codex's lock directory exists (spec §4.4 step 8). held means a Codex has the
// thread.
func threadLocks(home, nativeID string) (release func(), held bool, err error) {
	dir := filepath.Join(home, "thread-writer-locks")
	if st, serr := os.Stat(dir); serr != nil || !st.IsDir() {
		return func() {}, false, nil
	}
	co, err := os.OpenFile(filepath.Join(dir, ".coordination.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if lerr := platform.LockExclusiveNonBlocking(co); lerr == nil {
			break
		} else if !errors.Is(lerr, platform.ErrLockHeld) || time.Now().After(deadline) {
			co.Close()
			return nil, true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	rel := func() { platform.Unlock(co); co.Close() }
	if tf, terr := os.OpenFile(filepath.Join(dir, strings.ToLower(nativeID)+".lock"), os.O_RDWR, 0); terr == nil {
		if lerr := platform.LockExclusiveNonBlocking(tf); lerr != nil {
			tf.Close()
			rel()
			return nil, true, nil
		}
		return func() { platform.Unlock(tf); tf.Close(); rel() }, false, nil
	} else if !os.IsNotExist(terr) {
		rel()
		return nil, false, terr
	}
	return rel, false, nil
}

func (w *Writer) LockInstall(ctx context.Context, in agent.WriteInput) (func(), error) {
	nativeID := nativeIDOf(in)
	return w.lockThread(ctx, in.TargetState.Home, nativeID)
}

func (w *Writer) lockThread(ctx context.Context, home, nativeID string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if nativeID == "" {
		return nil, errcode.New(errcode.UnknownFormat, "snapshot does not identify a Codex thread")
	}
	rs, _, err := (runtimeDetector{}).Running(ctx, home, nativeID)
	if err != nil {
		return nil, err
	}
	if rs == agent.Running || (rs == agent.Unknown && !w.ClientClosed) {
		return nil, errcode.New(errcode.ClientRunning, "the Codex client may be running; close it and confirm with --client-closed")
	}
	release, held, err := threadLocks(home, nativeID)
	if err != nil {
		return nil, err
	}
	if held {
		return nil, errcode.New(errcode.ClientRunning, "a Codex client holds this thread")
	}
	return release, nil
}

// GuardRecovery refuses to undo vendor files while Codex could own them.
func GuardRecovery(ctx context.Context, d store.InstallData) (func(), error) {
	id := d.NativeSessionID
	if id == "" {
		for _, p := range append(append([]string{}, d.Backed...), d.Created...) {
			if parsed, _, ok := parseName(filepath.Base(p)); ok {
				id = parsed
				break
			}
		}
	}
	return (&Writer{ClientClosed: d.ClientClosed}).lockThread(ctx, d.Home, id)
}

// Apply executes the plan under the journal: backup, write, verify.
func (w *Writer) Apply(ctx context.Context, in agent.WriteInput, p agent.Plan, tx agent.Tx) (receipt agent.Receipt, applyErr error) {
	check := p
	check.Digest = ""
	digest, err := canonjson.Hash(check)
	if err != nil {
		return agent.Receipt{}, err
	}
	if digest != p.Digest {
		return agent.Receipt{}, errcode.New(errcode.BaselineChanged, "the restore plan digest does not match")
	}
	home := in.TargetState.Home
	t, ok := tx.(store.Tx)
	if !ok {
		return agent.Receipt{}, errcode.New(errcode.Internal, "writer needs a store transaction")
	}
	nativeID := nativeIDOf(in)
	if !in.VendorLocked {
		// The standalone Writer path must also hold locks through staging,
		// revalidation, backup and any rollback.
		release, err := w.LockInstall(ctx, in)
		if err != nil {
			return agent.Receipt{}, err
		}
		defer release()
	}
	// plan must match the target now
	paths := make([]string, 0, len(p.Writes))
	var all []string
	for _, a := range in.Source.Artifacts() {
		all = append(all, a.Path)
	}
	sort.Strings(all)
	if baselineOf(existingState(home, all)) != p.BaselineHash {
		return agent.Receipt{}, errcode.New(errcode.BaselineChanged, "the target changed after the plan was made")
	}
	d := store.InstallData{NativeSessionID: nativeID, ClientClosed: w.ClientClosed, PlanDigest: p.Digest, Mode: p.Mode, Route: p.Route, Home: home, BackupDir: t.BackupDir()}
	contents := map[string][]byte{}
	for _, pw := range p.Writes {
		c, _, err := buildTarget(ctx, in.Source, pw.Path, in.PathMap, providerFor(in.TargetState.Home, pw.Path))
		if err != nil {
			return agent.Receipt{}, err
		}
		if shaHex(c) != pw.SHA256 {
			return agent.Receipt{}, errcode.New(errcode.BaselineChanged, "the source no longer matches the plan")
		}
		if err := platform.AtomicWrite(filepath.Join(t.StagingDir(), filepath.FromSlash(pw.Path)), c, 0600); err != nil {
			return agent.Receipt{}, err
		}
		contents[pw.Path] = c
		paths = append(paths, pw.Path)
	}
	if err := t.SetState(store.StStaged, d); err != nil {
		return agent.Receipt{}, err
	}

	// Undo under the same vendor locks; a later caller must not overwrite
	// records produced after this writer released its critical section.
	defer func() {
		if applyErr != nil && (t.State() == store.StBackedUp || t.State() == store.StWritten) {
			if err := t.Rollback(); err != nil {
				applyErr = errcode.Wrap(errcode.RecoveryNeeded, "install rollback failed", err)
			}
		}
	}()
	if baselineOf(existingState(home, all)) != p.BaselineHash {
		return agent.Receipt{}, errcode.New(errcode.BaselineChanged, "the target changed before the writer acquired its locks")
	}
	if in.ValidateEnvironment != nil {
		if err := in.ValidateEnvironment(ctx); err != nil {
			return agent.Receipt{}, err
		}
	}

	for _, pw := range p.Writes {
		target := filepath.Join(home, filepath.FromSlash(pw.Path))
		if pw.Op == "create" {
			d.Created = append(d.Created, pw.Path)
			continue
		}
		old, err := os.ReadFile(target)
		if err != nil {
			return agent.Receipt{}, err
		}
		st, _ := os.Stat(target)
		if err := platform.AtomicWrite(filepath.Join(d.BackupDir, filepath.FromSlash(pw.Path)), old, st.Mode().Perm()); err != nil {
			return agent.Receipt{}, err
		}
		d.Backed = append(d.Backed, pw.Path)
	}
	if err := t.SetState(store.StBackedUp, d); err != nil {
		return agent.Receipt{}, err
	}
	for i, pw := range p.Writes {
		if i == len(p.Writes)/2 {
			if err := failpoint.Hit("install.mid_write"); err != nil {
				return agent.Receipt{}, err
			}
		}
		target := filepath.Join(home, filepath.FromSlash(pw.Path))
		mode := os.FileMode(0600)
		if st, err := os.Stat(target); err == nil {
			mode = st.Mode().Perm()
		}
		if err := platform.AtomicWrite(target, contents[pw.Path], mode); err != nil {
			return agent.Receipt{}, err
		}
		d.Written = append(d.Written, pw)
	}
	if err := t.SetState(store.StWritten, d); err != nil {
		return agent.Receipt{}, err
	}
	// The coordination/thread locks prevent a cooperating Codex writer from
	// starting here. Probing the thread lock again would detect our own lock.
	rec := agent.Receipt{SessionID: in.SessionID, RevisionID: in.RevisionID, NativeSessionID: nativeID, InstallMode: session.InstallNative,
		Written: p.Writes, At: session.FormatTime(time.Now())}
	if err := w.Verify(ctx, in, rec); err != nil {
		return rec, errcode.Wrap(errcode.TargetRejected, "the written files did not verify", err)
	}
	var count, boundary int64
	var last []byte
	for _, pw := range p.Writes {
		_ = pw
	}
	for _, c := range contents {
		count += int64(bytes.Count(c, []byte("\n")))
		boundary += int64(len(c))
		last = c
	}
	_ = last
	rec.Import = &session.ImportReceipt{WriterVersion: WriterVersion, SourceRevision: in.RevisionID, WrittenCount: count,
		BoundaryOffset: boundary, At: rec.At}
	if len(contents) == 1 {
		for _, c := range contents {
			rec.Import.BoundaryHash = shaHex(c)
		}
	}
	return rec, nil
}

func nativeIDOf(in agent.WriteInput) string {
	for _, a := range in.Source.Artifacts() {
		if t, _, ok := parseName(filepath.Base(a.Path)); ok {
			return t
		}
	}
	return ""
}

// Verify re-reads the written files and checks hash and JSONL validity.
func (w *Writer) Verify(ctx context.Context, in agent.WriteInput, r agent.Receipt) error {
	for _, pw := range r.Written {
		target := filepath.Join(in.TargetState.Home, filepath.FromSlash(pw.Path))
		b, err := os.ReadFile(target)
		if err != nil {
			return err
		}
		if shaHex(b) != pw.SHA256 {
			return errors.New("written file does not match the plan")
		}
		rd := jsonl.NewReader(io.NopCloser(bytes.NewReader(b)), 0, jsonl.Options{RequireJSON: true})
		for {
			_, err := rd.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Rollback restores everything the transaction recorded (used by recovery).
func (w *Writer) Rollback(ctx context.Context, home string, tx agent.Tx) error {
	return errors.New("rollback is performed by store recovery from the journal")
}

// ReplaceUnderLocks atomically replaces (or creates) path with data while
// holding Codex's coordination and thread locks when they exist (the same
// protocol Apply uses). Used by the P0 experiment tooling.
func ReplaceUnderLocks(home, nativeID, path string, data []byte) error {
	release, held, err := threadLocks(home, nativeID)
	if err != nil {
		return err
	}
	if held {
		return errcode.New(errcode.ClientRunning, "a Codex client holds this thread")
	}
	defer release()
	mode := os.FileMode(0600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return platform.AtomicWrite(path, data, mode)
}

// LockStatus reports, for every lock file under thread-writer-locks, whether
// it is currently held by another process (probe only; locks are released at
// once).
func LockStatus(home string) (map[string]bool, error) {
	dir := filepath.Join(home, "thread-writer-locks")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		f, err := os.OpenFile(filepath.Join(dir, e.Name()), os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if lerr := platform.LockExclusiveNonBlocking(f); errors.Is(lerr, platform.ErrLockHeld) {
			out[e.Name()] = true
		} else if lerr == nil {
			platform.Unlock(f)
			out[e.Name()] = false
		}
		f.Close()
	}
	return out, nil
}
