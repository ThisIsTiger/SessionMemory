//go:build failpoint

package sync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/source/codex"
	"session-memory/internal/store"
)

// crashInstall runs a restore that panics right after the files were written
// (journal at "written"), then closes the store as a killed process would.
func crashInstall(t *testing.T, b *node, sid string) {
	t.Helper()
	t.Setenv("SM_FAILPOINTS", "install.after_written=panic")
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		if _, _, err := b.restore(t, sid, engine.RestoreOptions{ClientClosed: true}); err != nil {
			t.Fatal(err)
		}
	}()
	t.Setenv("SM_FAILPOINTS", "")
	if !panicked {
		t.Fatal("failpoint not reached")
	}
	if err := b.st.Close(); err != nil {
		t.Fatal(err)
	}
}

func reopenWithGuard(t *testing.T, b *node) *store.FS {
	t.Helper()
	s, err := store.Open(b.root, b.sync.V)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.InstallGuard = codex.GuardRecovery
	return s
}

// Recovery must not roll back over a conversation that kept growing after the
// install crashed: it refuses, and keeps the files, backups and journal.
func TestRecoverKeepsContinuationAfterInstallCrash(t *testing.T) {
	for _, existing := range []bool{false, true} {
		label := "create"
		if existing {
			label = "append"
		}
		t.Run(label, func(t *testing.T) {
			_, b, sid, target := updatedPair(t, existing)
			crashInstall(t, b, sid)
			f, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.WriteString("{\"type\":\"event_msg\",\"payload\":{\"message\":\"AFTER_CRASH\"}}\n"); err != nil {
				t.Fatal(err)
			}
			f.Close()
			want, _ := os.ReadFile(target)
			reopened := reopenWithGuard(t, b)
			_, err = reopened.Recover(context.Background())
			got, readErr := os.ReadFile(target)
			if readErr != nil || !bytes.Equal(got, want) {
				t.Fatalf("recovery erased post-crash continuation: err=%v read=%v", err, readErr)
			}
			if errcode.CodeOf(err) != errcode.RecoveryNeeded {
				t.Fatalf("changed files must preserve journal and require attention: %v", err)
			}
			journals, _ := filepath.Glob(filepath.Join(b.root, "journal", "*.json"))
			if len(journals) != 1 {
				t.Fatalf("the journal must stay: %v", journals)
			}
			if !strings.Contains(err.Error(), journals[0]) {
				t.Fatalf("the error does not name the journal %s: %v", journals[0], err)
			}
			// still refused on the next start, nothing was consumed
			if _, err = reopened.Recover(context.Background()); errcode.CodeOf(err) != errcode.RecoveryNeeded {
				t.Fatalf("second recovery: %v", err)
			}
		})
	}
}

// With nothing written after the crash, recovery still rolls the install back.
func TestRecoverRollsBackUntouchedInstallCrash(t *testing.T) {
	for _, existing := range []bool{false, true} {
		label := "create"
		if existing {
			label = "append"
		}
		t.Run(label, func(t *testing.T) {
			_, b, sid, target := updatedPair(t, existing)
			var before []byte
			if existing {
				before, _ = os.ReadFile(target)
			}
			crashInstall(t, b, sid)
			if got, _ := os.ReadFile(target); existing && bytes.Equal(got, before) {
				t.Fatal("setup: the crash happened before the files were written")
			}
			reopened := reopenWithGuard(t, b)
			if _, err := reopened.Recover(context.Background()); err != nil {
				t.Fatalf("recover: %v", err)
			}
			got, err := os.ReadFile(target)
			switch {
			case existing && (err != nil || !bytes.Equal(got, before)):
				t.Fatalf("existing file not restored: %v", err)
			case !existing && err == nil:
				t.Fatal("created file not removed")
			}
			if journals, _ := filepath.Glob(filepath.Join(b.root, "journal", "*.json")); len(journals) != 0 {
				t.Fatalf("journal left behind: %v", journals)
			}
		})
	}
}
