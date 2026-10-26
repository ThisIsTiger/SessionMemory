package codex

import (
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

var cwdRe = regexp.MustCompile(`"cwd":"((?:[^"\\]|\\.)*)"`)

// mapLine rewrites payload.cwd of session_meta and turn_context records at the
// byte level; every other byte is untouched.
func mapLine(line []byte, pm agent.PathMap) ([]byte, bool) {
	if len(pm.Rules) == 0 {
		return line, false
	}
	var env struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &env) != nil || (env.Type != "session_meta" && env.Type != "turn_context") {
		return line, false
	}
	pi := bytes.Index(line, []byte(`"payload":`))
	if pi < 0 {
		return line, false
	}
	loc := cwdRe.FindSubmatchIndex(line[pi:])
	if loc == nil {
		return line, false
	}
	var old string
	if json.Unmarshal([]byte(`"`+string(line[pi+loc[2]:pi+loc[3]])+`"`), &old) != nil {
		return line, false
	}
	mapped, ok := pm.Apply(old)
	if !ok || mapped == old {
		return line, false
	}
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false)
	e.Encode(mapped)
	val := bytes.TrimRight(enc.Bytes(), "\n")
	out := make([]byte, 0, len(line)+len(val))
	out = append(out, line[:pi+loc[2]-1]...)
	out = append(out, val...)
	out = append(out, line[pi+loc[3]+1:]...)
	return out, true
}

// buildTarget streams the artifact through mapLine and returns the bytes.
func buildTarget(ctx context.Context, src agent.SnapshotReader, path string, pm agent.PathMap) ([]byte, bool, error) {
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
		l, c := mapLine(rec.Line, pm)
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
	home := in.TargetState.Home
	plan := agent.Plan{SchemaVersion: 1, SessionID: in.SessionID, RevisionID: in.RevisionID, Mode: in.Mode, Route: agent.RouteFiles}
	var paths []string
	for _, a := range in.Source.Artifacts() {
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
		content, lenChanged, err := buildTarget(ctx, in.Source, p, in.PathMap)
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
	}
	return rel, false, nil
}

// Apply executes the plan under the journal: backup, write, verify.
func (w *Writer) Apply(ctx context.Context, in agent.WriteInput, p agent.Plan, tx agent.Tx) (agent.Receipt, error) {
	home := in.TargetState.Home
	t, ok := tx.(store.Tx)
	if !ok {
		return agent.Receipt{}, errcode.New(errcode.Internal, "writer needs a store transaction")
	}
	// run state, before and after
	nativeID := nativeIDOf(in)
	rs, _, _ := (runtimeDetector{}).Running(ctx, home, nativeID)
	if rs == agent.Running || (rs == agent.Unknown && !w.ClientClosed) {
		return agent.Receipt{}, errcode.New(errcode.ClientRunning, "the Codex client may be running; close it and confirm with --client-closed")
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
	d := store.InstallData{PlanDigest: p.Digest, Mode: p.Mode, Route: p.Route, Home: home, BackupDir: t.BackupDir()}
	contents := map[string][]byte{}
	for _, pw := range p.Writes {
		c, _, err := buildTarget(ctx, in.Source, pw.Path, in.PathMap)
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

	release, held, err := threadLocks(home, nativeID)
	if err != nil {
		return agent.Receipt{}, err
	}
	if held {
		return agent.Receipt{}, errcode.New(errcode.ClientRunning, "a Codex client holds this thread")
	}
	defer release()
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
	// client must not have started meanwhile
	if rs2, _, _ := (runtimeDetector{}).Running(ctx, home, nativeID); rs2 == agent.Running {
		return agent.Receipt{}, errcode.New(errcode.ClientRunning, "the client started during the install")
	}
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
