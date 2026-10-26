package checkpoint

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	st := State{
		SessionID: "sess-123", RolloutPath: "/x/rollout-1.jsonl",
		FileSize: 100, FileModTime: time.Now().UTC().Truncate(time.Second),
		Offset: 50, Status: StatusReady, Classification: "included",
		Source: "vscode", Originator: "Codex Desktop",
		LastEventAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load("vscode", "sess-123")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SessionID != st.SessionID || got.Offset != 50 || got.SchemaVersion != 1 || got.Status != StatusReady {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestLoadMissing(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Load("vscode", "nope"); !os.IsNotExist(err) {
		t.Errorf("expected not-exist, got %v", err)
	}
}

func TestSafeSessionID(t *testing.T) {
	if SafeSessionID("Abc-123_X.y") != "Abc-123_X.y" {
		t.Error("safe id mangled")
	}
	weird := "session id with spaces/and:colons"
	got := SafeSessionID(weird)
	if got == weird || len(got) != 64 {
		t.Errorf("unsafe id not hashed: %q", got)
	}
	safeRe := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	if !safeRe.MatchString(got) {
		t.Errorf("hashed name still unsafe: %q", got)
	}
}

func TestCorruptFileQuarantined(t *testing.T) {
	s := newTestStore(t)
	dir := s.dirFor("vscode")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0600)

	_, err := s.Load("vscode", "bad")
	if _, ok := err.(CorruptError); !ok {
		t.Fatalf("expected CorruptError, got %v", err)
	}
	// quarantined away
	matches, _ := filepath.Glob(filepath.Join(dir, "bad.json.corrupt-*"))
	if len(matches) == 0 {
		t.Error("corrupt file not renamed to .corrupt-*")
	}
	// and original path no longer blocks a fresh Save
	if err := s.Save(State{SessionID: "bad", Source: "vscode", Status: "new"}); err != nil {
		t.Fatalf("Save after quarantine: %v", err)
	}
}

func TestListRetryable(t *testing.T) {
	s := newTestStore(t)
	states := []State{
		{SessionID: "a", Source: "codex", Status: StatusPending},
		{SessionID: "b", Source: "codex", Status: StatusFailed},
		{SessionID: "c", Source: "codex", Status: StatusReady},
		{SessionID: "d", Source: "codex", Status: StatusIgnoredSource},
	}
	for _, st := range states {
		if err := s.Save(st); err != nil {
			t.Fatalf("Save %s: %v", st.SessionID, err)
		}
	}
	got, err := s.ListRetryable("codex")
	if err != nil {
		t.Fatalf("ListRetryable: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("retryable = %d, want 2", len(got))
	}
	ids := []string{got[0].SessionID, got[1].SessionID}
	if ids[0] != "a" || ids[1] != "b" {
		t.Errorf("retryable order = %v", ids)
	}
}

func TestPermsArePrivate(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(State{SessionID: "p", Source: "codex"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(s.pathFor("codex", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Errorf("file perm = %o, want 600", info.Mode().Perm())
	}
	dir := s.dirFor("codex")
	dinfo, _ := os.Stat(dir)
	_ = dinfo
	_ = strings.TrimSpace
}

// A corrupt primary must recover from the mirror instead of forcing a
// rebuild-from-zero (which re-derives OperationIDs and double-bills).
func TestMirrorRecoversCorruptPrimary(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	st := State{SessionID: "abc", Source: "codex", Adapter: "codex", Status: StatusReady, Offset: 12345}
	if err := s.Save(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	primary := filepath.Join(dir, "codex", "sessions", "abc.json")
	if err := os.WriteFile(primary, []byte("{corrupt"), 0600); err != nil {
		t.Fatalf("corrupt primary: %v", err)
	}
	got, err := s.Load("codex", "abc")
	if err != nil {
		t.Fatalf("load after corrupt primary: %v", err)
	}
	if got.Offset != 12345 {
		t.Errorf("offset = %d, want 12345 (mirror recovery)", got.Offset)
	}
	// the next Save must repair the primary
	if err := s.Save(got); err != nil {
		t.Fatalf("resave: %v", err)
	}
	if _, err := s.Load("codex", "abc"); err != nil {
		t.Fatalf("load after repair: %v", err)
	}
}

// Both copies unreadable -> CorruptError so callers rebuild deliberately.
func TestBothCopiesCorruptStillReportsCorrupt(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(State{SessionID: "abc", Source: "codex", Adapter: "codex", Status: StatusReady}); err != nil {
		t.Fatalf("save: %v", err)
	}
	primary := filepath.Join(dir, "codex", "sessions", "abc.json")
	if err := os.WriteFile(primary, []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primary+".mirror", []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("codex", "abc"); err == nil {
		t.Fatal("want CorruptError when both copies are unreadable")
	}
}
