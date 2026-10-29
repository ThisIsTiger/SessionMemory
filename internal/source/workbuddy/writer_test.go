package workbuddy

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
	"session-memory/internal/store"
	"session-memory/internal/vault"
)

const testNativeID = "0192f3a4-0000-7000-8000-000000000001"

func testRegistry(t *testing.T, home, user string) *sql.DB {
	t.Helper()
	schema, err := os.ReadFile("testdata/registry-5.8.3.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "workbuddy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO sessions(id,cwd,user_id,created_at,updated_at) VALUES('local-seed','/local',?,1,1)", user); err != nil {
		t.Fatal(err)
	}
	return db
}

type writerSnapshot struct{ data map[string][]byte }

func (s writerSnapshot) Artifacts() []session.Artifact {
	var out []session.Artifact
	for _, p := range sortedKeys(s.data) {
		out = append(out, session.Artifact{Path: p, Kind: session.KindStream, Form: session.FormPlain})
	}
	return out
}
func (s writerSnapshot) Open(_ context.Context, p string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data[p])), nil
}
func nativeFixture(cwd string) []byte {
	return []byte(line(map[string]any{"type": "message", "id": "u1", "sessionId": testNativeID, "timestamp": int64(1793000000000), "role": "user", "cwd": cwd, "content": "hello"}) +
		line(map[string]any{"type": "message", "id": "a1", "parentId": "u1", "sessionId": testNativeID, "timestamp": int64(1793000000001), "role": "assistant", "cwd": cwd, "content": "answer"}) +
		line(map[string]any{"type": "ai-title", "sessionId": testNativeID, "aiTitle": "Synthetic native sync", "cwd": cwd}))
}
func testWriter() *Writer {
	return &Writer{AllowRegistryWrite: true, ClientClosed: true, version: func(context.Context) error { return nil }, closed: func(context.Context, string) error { return nil }}
}
func writerInput(home string) agent.WriteInput {
	path := "projects/source/" + testNativeID + ".jsonl"
	return agent.WriteInput{SessionID: session.NewSessionID(), RevisionID: "test-revision", Mode: agent.ModeNative,
		Source:        writerSnapshot{map[string][]byte{path: nativeFixture("/source")}},
		SourceProfile: session.AgentProfile{Agent: AgentName, Format: "workbuddy-jsonl"}, Target: session.AgentProfile{Agent: AgentName},
		TargetState: agent.TargetState{Home: home}, Cwd: "/target", PathMap: agent.PathMap{Rules: []agent.PathRule{{From: "/source", To: "/target"}}}}
}
func writerStore(t *testing.T) *store.FS {
	t.Helper()
	key, _ := vault.GenerateKey()
	v, _ := vault.New(session.NewVaultID(), key)
	st, err := store.Open(t.TempDir(), v)
	if err != nil {
		t.Fatal(err)
	}
	st.InstallRowUndo = UndoRegistry
	t.Cleanup(func() { st.Close() })
	return st
}

func peerStore(t *testing.T, source *store.FS) *store.FS {
	t.Helper()
	st, err := store.Open(t.TempDir(), source.Sealer())
	if err != nil {
		t.Fatal(err)
	}
	st.InstallRowUndo = UndoRegistry
	t.Cleanup(func() { st.Close() })
	return st
}

func mirrorRevisions(t *testing.T, source, target *store.FS, sid string) {
	t.Helper()
	ctx := context.Background()
	err := filepath.WalkDir(filepath.Join(source.Root, "store", "objects"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return target.PutSealed(ctx, "obj_"+strings.TrimSuffix(d.Name(), ".obj"), f)
	})
	if err != nil {
		t.Fatal(err)
	}
	revs, err := source.Revisions(sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, rev := range revs {
		if err := target.CommitRevision(nil, rev); err != nil {
			t.Fatal(err)
		}
	}
}
func mustPlan(t *testing.T, w *Writer, in agent.WriteInput) agent.Plan {
	t.Helper()
	p, err := w.Plan(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNativeWriterLocalAccountAndRowUndo(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db := testRegistry(t, home, "target-account")
	st := writerStore(t)
	in := writerInput(home)
	w := testWriter()
	p := mustPlan(t, w, in)
	if p.Route != agent.RouteRegistry || len(p.Writes) != 2 {
		t.Fatalf("unexpected plan: %+v", p)
	}
	before, _ := os.ReadFile(filepath.Join(home, "workbuddy.db"))
	if len(before) == 0 {
		t.Fatal("fixture DB missing")
	}
	tx, err := st.Begin(ctx, store.KindInstall, in.SessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := w.Apply(ctx, in, p, tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Verify(ctx, in, rec); err != nil {
		t.Fatal(err)
	}
	r, err := readRow(ctx, db, testNativeID)
	if err != nil {
		t.Fatal(err)
	}
	if r["user_id"] != "target-account" || r["cwd"] != "/target" || r["model"] != nil || r["permission_mode"] != nil {
		t.Fatalf("copied source policy/account: %+v", r)
	}
	if _, err := db.Exec("UPDATE sessions SET title='unrelated change' WHERE id='local-seed'"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	r, _ = readRow(ctx, db, testNativeID)
	if r != nil {
		t.Fatal("new row survived rollback")
	}
	var title string
	if err := db.QueryRow("SELECT title FROM sessions WHERE id='local-seed'").Scan(&title); err != nil || title != "unrelated change" {
		t.Fatal("rollback replaced unrelated rows")
	}
	if _, err := os.Stat(filepath.Join(home, "projects", "target", testNativeID+".jsonl")); !os.IsNotExist(err) {
		t.Fatal("created file survived rollback")
	}
}

func TestWriterRejectsUnknownSchemaAndOptOut(t *testing.T) {
	home := t.TempDir()
	db := testRegistry(t, home, "target")
	in := writerInput(home)
	if _, err := (&Writer{}).Plan(context.Background(), in); errcode.CodeOf(err) != errcode.Unsupported {
		t.Fatalf("registry not opt-in: %v", err)
	}
	w := testWriter()
	w.ClientClosed = false
	if _, err := w.LockInstall(context.Background(), in); errcode.CodeOf(err) != errcode.ClientRunning {
		t.Fatalf("missing closed confirmation: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN vendor_change TEXT"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Plan(context.Background(), in); errcode.CodeOf(err) != errcode.UnknownFormat {
		t.Fatalf("unknown schema accepted: %v", err)
	}
}

func TestWriterBaselineAndRecoveryCAS(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db := testRegistry(t, home, "target")
	in := writerInput(home)
	w := testWriter()
	st := writerStore(t)
	p := mustPlan(t, w, in)
	if _, err := db.Exec("UPDATE sessions SET user_id='changed-account'"); err != nil {
		t.Fatal(err)
	}
	tx, _ := st.Begin(ctx, store.KindInstall, in.SessionID, "")
	if _, err := w.Apply(ctx, in, p, tx); errcode.CodeOf(err) != errcode.BaselineChanged {
		t.Fatalf("changed account accepted: %v", err)
	}
	tx.Finish()
	p = mustPlan(t, w, in)
	tx, _ = st.Begin(ctx, store.KindInstall, in.SessionID, "")
	if _, err := w.Apply(ctx, in, p, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET custom_title='user edited' WHERE id=?", testNativeID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); errcode.CodeOf(err) != errcode.BaselineChanged {
		t.Fatalf("advanced vendor row overwritten: %v", err)
	}
	r, _ := readRow(ctx, db, testNativeID)
	if r["custom_title"] != "user edited" {
		t.Fatal("recovery lost user row changes")
	}
}

func TestWriterClientStartsAndAdvancedFileIsPreserved(t *testing.T) {
	for _, advance := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean-rollback", true: "preserve-client-write"}[advance], func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			testRegistry(t, home, "target")
			st := writerStore(t)
			w := testWriter()
			in := writerInput(home)
			p := mustPlan(t, w, in)
			path := filepath.Join(home, "projects", "target", testNativeID+".jsonl")
			w.closed = func(context.Context, string) error {
				if b, err := os.ReadFile(path); err == nil {
					if advance {
						if err := os.WriteFile(path, append(b, []byte("client-new-bytes\n")...), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return errcode.New(errcode.ClientRunning, "client started")
				}
				return nil
			}
			tx, _ := st.Begin(ctx, store.KindInstall, in.SessionID, "")
			_, err := w.Apply(ctx, in, p, tx)
			if advance {
				if errcode.CodeOf(err) != errcode.RecoveryNeeded {
					t.Fatalf("expected protected recovery: %v", err)
				}
				b, _ := os.ReadFile(path)
				if !bytes.Contains(b, []byte("client-new-bytes")) {
					t.Fatal("lost concurrent client data")
				}
			} else {
				if errcode.CodeOf(err) != errcode.RecoveryNeeded {
					t.Fatalf("expected deferred rollback until quit: %v", err)
				}
				w.closed = func(context.Context, string) error { return nil }
				st.InstallGuard = func(ctx context.Context, d store.InstallData) (func(), error) { return w.lockHome(ctx, d.Home) }
				if _, err := st.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("file not rolled back")
				}
			}
		})
	}
}

func TestMapCwdPreservesHistoryAndFormatting(t *testing.T) {
	original := []byte("{ \"type\": \"message\", \"content\": \"history /source\", \"cwd\" : \"/source/sub\", \"timestamp\":1 }\n")
	out, err := mapCwd(original, agent.PathMap{Rules: []agent.PathRule{{From: "/source", To: "/target-long"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(original, []byte("\"/source/sub\""), []byte("\"/target-long/sub\""), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("rewrote unrelated bytes: %s", out)
	}
}

func TestWriterRejectsAmbiguousAccountAndSymlink(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db := testRegistry(t, home, "first")
	w := testWriter()
	in := writerInput(home)
	if _, err := db.Exec("INSERT INTO sessions(id,cwd,user_id,created_at,updated_at) VALUES('other-account','/local','second',1,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Plan(ctx, in); errcode.CodeOf(err) != errcode.NeedsAction {
		t.Fatalf("ambiguous account accepted: %v", err)
	}
	if _, err := db.Exec("DELETE FROM sessions WHERE id='other-account'"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "projects")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := w.Plan(ctx, in); errcode.CodeOf(err) != errcode.NeedsAction {
		t.Fatalf("symlink accepted: %v", err)
	}
}

func TestVerifyAndRollbackProtectUnchangedArtifacts(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	testRegistry(t, home, "target")
	w := testWriter()
	st := writerStore(t)
	in := writerInput(home)
	p := mustPlan(t, w, in)
	tx, _ := st.Begin(ctx, store.KindInstall, in.SessionID, "")
	if _, err := w.Apply(ctx, in, p, tx); err != nil {
		t.Fatal(err)
	}
	tx.Finish()
	in.BindingID = "existing-binding"
	snap := in.Source.(writerSnapshot)
	snap.data["projects/source/"+testNativeID+".file-rollback.ndjson"] = []byte("{\"type\":\"snapshot\"}\n")
	p = mustPlan(t, w, in)
	if len(p.Writes) != 1 {
		t.Fatalf("expected only the new side stream, got %+v", p.Writes)
	}
	tx, _ = st.Begin(ctx, store.KindInstall, in.SessionID, in.BindingID)
	rec, err := w.Apply(ctx, in, p, tx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "projects", "target", testNativeID+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{\"type\":\"client-new-record\"}\n")
	f.Close()
	if err := w.Verify(ctx, in, rec); errcode.CodeOf(err) != errcode.TargetRejected {
		t.Fatalf("unchanged artifact advanced unnoticed: %v", err)
	}
	if err := tx.Rollback(); errcode.CodeOf(err) != errcode.RecoveryNeeded {
		t.Fatalf("rollback accepted advanced baseline: %v", err)
	}
	content, _ := os.ReadFile(path)
	if !bytes.Contains(content, []byte("client-new-record")) {
		t.Fatal("client data overwritten")
	}
}

func TestPrewriteFailureDoesNotRequireRegistryRecovery(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	testRegistry(t, home, "target")
	w := testWriter()
	st := writerStore(t)
	in := writerInput(home)
	p := mustPlan(t, w, in)
	in.ValidateEnvironment = func(context.Context) error { return errcode.New(errcode.NeedsAction, "project changed") }
	tx, _ := st.Begin(ctx, store.KindInstall, in.SessionID, "")
	if _, err := w.Apply(ctx, in, p, tx); errcode.CodeOf(err) != errcode.NeedsAction {
		t.Fatalf("wrong prewrite failure: %v", err)
	}
	var data store.InstallData
	if err := tx.Data(&data); err != nil {
		t.Fatal(err)
	}
	if data.RegistryUndo != "" {
		t.Fatal("journal points to registry backup before it exists")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("failed to discard prewrite transaction: %v", err)
	}
}

type writerTestAdapter struct {
	*Adapter
	w *Writer
}

type lateStartAdapter struct {
	writerTestAdapter
	target string
}

func (a lateStartAdapter) Capture(ctx context.Context, ref agent.NativeRef, cursor agent.Cursor, sink agent.SegmentSink) (agent.CaptureResult, error) {
	res, err := a.Adapter.Capture(ctx, ref, cursor, sink)
	if ref.Home == a.target {
		a.w.closed = func(context.Context, string) error {
			return errcode.New(errcode.ClientRunning, "client started during adoption")
		}
	}
	return res, err
}

func TestClientStartsDuringAdoptionDefersRollback(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS native path format")
	}
	ctx := context.Background()
	homeA, homeB, project := t.TempDir(), t.TempDir(), t.TempDir()
	dbB := testRegistry(t, homeB, "target")
	path := filepath.Join(homeA, "projects", "source", testNativeID+".jsonl")
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, nativeFixture(project), 0600)
	stA := writerStore(t)
	stB := peerStore(t, stA)
	w := testWriter()
	cfg := config.DefaultConfig()
	cfg.Agents.WorkBuddy.Home = homeA
	ad := writerTestAdapter{New(cfg), w}
	a := &engine.Engine{Store: stA, DeviceID: session.NewDeviceID()}
	bnd, _, err := a.Track(ctx, ad, agent.NativeRef{Home: homeA, NativeID: testNativeID}, "")
	if err != nil {
		t.Fatal(err)
	}
	mirrorRevisions(t, stA, stB, bnd.SessionID)
	b := &engine.Engine{Store: stB, DeviceID: session.NewDeviceID()}
	_, _, err = b.Restore(ctx, lateStartAdapter{ad, homeB}, bnd.SessionID, engine.RestoreOptions{Home: homeB, Cwd: project, ClientClosed: true})
	if errcode.CodeOf(err) != errcode.RecoveryNeeded {
		t.Fatalf("unsafe adoption rollback: %v", err)
	}
	r, _ := readRow(ctx, dbB, testNativeID)
	if r == nil {
		t.Fatal("removed vendor row while client alive")
	}
	stB.InstallGuard = func(ctx context.Context, d store.InstallData) (func(), error) { return w.lockHome(ctx, d.Home) }
	if _, err := stB.Recover(ctx); errcode.CodeOf(err) != errcode.RecoveryNeeded {
		t.Fatalf("recovery ignored running client: %v", err)
	}
	w.closed = func(context.Context, string) error { return nil }
	if _, err := stB.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = readRow(ctx, dbB, testNativeID)
	if r != nil {
		t.Fatal("pending install not undone after quit")
	}
}

func (a writerTestAdapter) WriterWith(closed bool) agent.Writer {
	a.w.ClientClosed = closed
	return a.w
}

func TestNativeWorkBuddyEngineRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native writer path encoding currently supports macOS only")
	}
	ctx := context.Background()
	source, target := t.TempDir(), t.TempDir()
	pa, pb := t.TempDir(), t.TempDir()
	testRegistry(t, source, "account-A")
	dbB := testRegistry(t, target, "account-B")
	srcPath := filepath.Join(source, "projects", strings.Trim(strings.ReplaceAll(pa, "/", "-"), "-"), testNativeID+".jsonl")
	os.MkdirAll(filepath.Dir(srcPath), 0700)
	os.WriteFile(srcPath, nativeFixture(pa), 0600)
	st := writerStore(t)
	w := testWriter()
	cfg := config.DefaultConfig()
	cfg.Agents.WorkBuddy.Home = source
	ad := writerTestAdapter{New(cfg), w}
	e := &engine.Engine{Store: st, DeviceID: session.NewDeviceID()}
	deviceA := e.DeviceID
	bnd, _, err := e.Track(ctx, ad, agent.NativeRef{Home: source, NativeID: testNativeID}, "")
	if err != nil {
		t.Fatal(err)
	}
	peer := peerStore(t, st)
	mirrorRevisions(t, st, peer, bnd.SessionID)
	eB := &engine.Engine{Store: peer, DeviceID: session.NewDeviceID()}
	deviceB := eB.DeviceID
	_, rec, err := eB.Restore(ctx, ad, bnd.SessionID, engine.RestoreOptions{Home: target, Cwd: pb, ClientClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.NativeSessionID != testNativeID {
		t.Fatal("changed native identity")
	}
	targetPath := filepath.Join(target, "projects", strings.Trim(strings.ReplaceAll(pb, "/", "-"), "-"), testNativeID+".jsonl")
	f, err := os.OpenFile(targetPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(line(map[string]any{"type": "message", "id": "u2", "parentId": "a1", "role": "user", "content": "continued on B", "cwd": pb, "timestamp": int64(1793000000002), "sessionId": testNativeID}))
	f.Close()
	bs, _ := peer.Bindings(bnd.SessionID)
	var bindingB session.Binding
	for _, b := range bs {
		if b.DeviceID == deviceB {
			bindingB = b
		}
	}
	if _, changed, err := eB.Checkpoint(ctx, ad, bindingB.BindingID, "", session.CaptureCheckpoint); err != nil || !changed {
		t.Fatalf("B checkpoint: %v", err)
	}
	e.DeviceID = deviceA
	mirrorRevisions(t, peer, st, bnd.SessionID)
	if _, _, err := e.Restore(ctx, ad, bnd.SessionID, engine.RestoreOptions{Home: source, Cwd: pa, ClientClosed: true}); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(srcPath)
	if !bytes.HasPrefix(content, nativeFixture(pa)) || !bytes.Contains(content, []byte("continued on B")) {
		t.Fatal("native roundtrip lost prefix or continuation")
	}
	r, _ := readRow(ctx, dbB, testNativeID)
	if r["user_id"] != "account-B" {
		t.Fatal("source account migrated")
	}
}

func TestSupportedVersionIsTheFiveLine(t *testing.T) {
	for v, want := range map[string]bool{"5.8.3": true, "5.9.3": true, "5.10.0": true, "6.0.0": false, "4.9.9": false, "": false} {
		if got := supportedVersion(v); got != want {
			t.Errorf("%q: %v, want %v", v, got, want)
		}
	}
}

// A task (WorkBuddy's own ~/WorkBuddy/<timestamp> folder, shown under 任务) stays
// a task on the other computer; a session of a real project folder does not
// become one.
func TestWriterKeepsTasksAsTasks(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, from, to string
		task           bool
	}{
		{"task", "/Users/a/WorkBuddy/2026-10-09-09-25-53", "/Users/b/WorkBuddy/2026-10-09-09-25-53", true},
		{"project", "/Users/a/Documents/proj", "/Users/b/Documents/proj", false},
		{"task folder replaced by a project folder", "/Users/a/WorkBuddy/2026-10-09-09-25-53", "/Users/b/Documents/proj", false},
	} {
		home := t.TempDir()
		testRegistry(t, home, "target-account")
		path := "projects/source/" + testNativeID + ".jsonl"
		in := writerInput(home)
		in.Source = writerSnapshot{map[string][]byte{path: nativeFixture(c.from)}}
		in.Cwd = c.to
		in.PathMap = agent.PathMap{Rules: []agent.PathRule{{From: c.from, To: c.to}}}
		b, err := testWriter().build(ctx, in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := b.after["is_playground"]
		if c.task && (got != int64(1) || b.after["mode"] != "craft") {
			t.Errorf("%s: is_playground=%v mode=%v", c.name, got, b.after["mode"])
		}
		if !c.task && got != int64(0) {
			t.Errorf("%s: is_playground=%v, want the default 0", c.name, got)
		}
	}
}

func TestTaskWorkspaceFollowsTheUsersHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := &Adapter{}
	got, ok := a.TaskWorkspace("/Users/someone/WorkBuddy/2026-10-09-09-25-53")
	if !ok || got != filepath.Join(home, "WorkBuddy", "2026-10-09-09-25-53") {
		t.Fatalf("%q %v", got, ok)
	}
	for _, o := range []string{"/Users/someone/WorkBuddy/Claw", "/Users/someone/Documents/2026-10-09-09-25-53", "/Users/someone/WorkBuddy/2026-10-09-09-25-53/sub"} {
		if _, ok := a.TaskWorkspace(o); ok {
			t.Errorf("%s is not a task folder", o)
		}
	}
}

// metaRig is two devices that already share a WorkBuddy session: A tracks it,
// B restored it, and A's metadata file can be changed afterwards.
type metaRig struct {
	ctx        context.Context
	ad         writerTestAdapter
	a, b       *engine.Engine
	sid, bid   string
	srcMeta    string
	targetMeta string
	opt        engine.RestoreOptions
}

func newMetaRig(t *testing.T) *metaRig {
	t.Helper()
	ctx := context.Background()
	source, target, pa, pb := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	testRegistry(t, source, "account-A")
	testRegistry(t, target, "account-B")
	dir := filepath.Join(source, "projects", strings.Trim(strings.ReplaceAll(pa, "/", "-"), "-"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testNativeID+".jsonl"), nativeFixture(pa), 0600); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, testNativeID+".meta.json")
	if err := os.WriteFile(meta, []byte(`{"value":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Agents.WorkBuddy.Home = source
	r := &metaRig{ctx: ctx, ad: writerTestAdapter{New(cfg), testWriter()}, srcMeta: meta,
		opt: engine.RestoreOptions{Home: target, Cwd: pb, ClientClosed: true}}
	st := writerStore(t)
	r.a = &engine.Engine{Store: st, DeviceID: session.NewDeviceID()}
	bnd, _, err := r.a.Track(ctx, r.ad, agent.NativeRef{Home: source, NativeID: testNativeID}, "")
	if err != nil {
		t.Fatal(err)
	}
	r.sid, r.bid = bnd.SessionID, bnd.BindingID
	peer := peerStore(t, st)
	r.b = &engine.Engine{Store: peer, DeviceID: session.NewDeviceID()}
	r.sync(t, st, peer)
	if _, _, err := r.b.Restore(ctx, r.ad, r.sid, r.opt); err != nil {
		t.Fatalf("initial restore: %v", err)
	}
	r.targetMeta = filepath.Join(target, "projects", strings.Trim(strings.ReplaceAll(pb, "/", "-"), "-"), testNativeID+".meta.json")
	return r
}

func (r *metaRig) sync(t *testing.T, from, to *store.FS) { mirrorRevisions(t, from, to, r.sid) }

// A change to the native metadata file is a legitimate fast-forward for a
// device that has not edited that file; a local edit is still protected.
func TestManagedMetaUpdateFastForwards(t *testing.T) {
	r := newMetaRig(t)
	if err := os.WriteFile(r.srcMeta, []byte(`{"value":"new"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := r.a.Checkpoint(r.ctx, r.ad, r.bid, "", session.CaptureCheckpoint); err != nil || !changed {
		t.Fatalf("source metadata checkpoint: changed=%v err=%v", changed, err)
	}
	r.sync(t, r.a.Store, r.b.Store)
	if _, _, err := r.b.Restore(r.ctx, r.ad, r.sid, r.opt); err != nil {
		t.Fatalf("metadata-only fast-forward rejected on unchanged managed target: %v", err)
	}
	if got, err := os.ReadFile(r.targetMeta); err != nil || !bytes.Contains(got, []byte("new")) {
		t.Fatalf("target metadata not updated: %s %v", got, err)
	}
}

func TestManagedMetaUpdateKeepsLocalEditProtected(t *testing.T) {
	r := newMetaRig(t)
	if err := os.WriteFile(r.srcMeta, []byte(`{"value":"new"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.a.Checkpoint(r.ctx, r.ad, r.bid, "", session.CaptureCheckpoint); err != nil {
		t.Fatal(err)
	}
	r.sync(t, r.a.Store, r.b.Store)
	local := []byte(`{"value":"edited on B"}`)
	if err := os.WriteFile(r.targetMeta, local, 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.b.Restore(r.ctx, r.ad, r.sid, r.opt)
	if errcode.CodeOf(err) != errcode.NeedsAction {
		t.Fatalf("a locally edited metadata file must block the update: %v", err)
	}
	if got, _ := os.ReadFile(r.targetMeta); !bytes.Equal(got, local) {
		t.Fatalf("local edit was overwritten: %s", got)
	}
}
