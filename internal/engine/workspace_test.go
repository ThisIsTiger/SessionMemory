package engine

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/errcode"
	"session-memory/internal/project"
	"session-memory/internal/session"
	"session-memory/internal/store"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/testutil/gitrepo"
	"session-memory/internal/vault"
)

func copyRevision(t *testing.T, r *rig, rev session.Revision) *Engine {
	t.Helper()
	st, err := store.Open(t.TempDir(), r.vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	err = filepath.WalkDir(filepath.Join(r.root, "store", "objects"), func(path string, d fs.DirEntry, err error) error {
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
		return st.PutSealed(context.Background(), "obj_"+strings.TrimSuffix(d.Name(), ".obj"), f)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitRevision(nil, rev); err != nil {
		t.Fatal(err)
	}
	return &Engine{Store: st, DeviceID: session.NewDeviceID()}
}

func TestRepoStateCaptureAndRestore(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	source := gitrepo.New(t)
	tid, path := r.put(701, 2)
	os.WriteFile(path, codexgen.Rollout(codexgen.Options{Seed: 701, Turns: 2, Cwd: source}), 0600)
	b, rev, err := r.eng.Track(ctx, r.ad, agent.NativeRef{Home: r.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := r.st.OpenObject(ctx, rev.RepoStateRef, vault.TypeRepoState, nil)
	if err != nil {
		t.Fatal(err)
	}
	var recorded project.State
	err = json.NewDecoder(rc).Decode(&recorded)
	rc.Close()
	if err != nil || recorded.Head != gitrepo.Run(t, source, "rev-parse", "HEAD") {
		t.Fatalf("recorded state=%+v err=%v", recorded, err)
	}
	// Code-only changes must produce a child revision, even with no new chat bytes.
	gitrepo.Run(t, source, "commit", "--allow-empty", "-q", "-m", "code-only")
	rev2, changed, err := r.eng.Checkpoint(ctx, r.ad, b.BindingID, "", session.CaptureCheckpoint)
	if err != nil || !changed || rev2.ParentRevisionID != rev.RevisionID {
		t.Fatalf("code-only checkpoint: %+v %v %v", rev2, changed, err)
	}
	if _, changed, err := r.eng.Checkpoint(ctx, r.ad, b.BindingID, "", session.CaptureAuto); err != nil || changed {
		t.Fatalf("unchanged code checkpoint: %v %v", changed, err)
	}

	for _, scenario := range []string{"matching", "mismatch", "override", "missing-commit", "missing-directory", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			chosen := rev
			if scenario == "legacy" {
				chosen.RepoStateRef = ""
				chosen.Seal()
			}
			target := copyRevision(t, r, chosen)
			dir := t.TempDir()
			if scenario == "missing-commit" {
				gitrepo.Run(t, dir, "init", "-q")
				os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("unrelated repository"), 0600)
				gitrepo.Run(t, dir, "add", "unrelated.txt")
				gitrepo.Run(t, dir, "commit", "--allow-empty", "-q", "-m", "unrelated")
			} else {
				gitrepo.Run(t, dir, "clone", "-q", source, ".")
				gitrepo.Run(t, dir, "remote", "set-url", "origin", "https://example.invalid/team/project.git")
				if scenario == "matching" {
					gitrepo.Run(t, dir, "checkout", "-q", recorded.Head)
				}
			}
			if scenario == "missing-directory" {
				dir = filepath.Join(dir, "absent")
			}
			opt := RestoreOptions{Home: t.TempDir(), Cwd: dir, ClientClosed: true, AllowCodeMismatch: scenario == "override" || scenario == "missing-directory"}
			plan, _, err := target.Restore(ctx, r.ad, chosen.SessionID, RestoreOptions{Home: opt.Home, Cwd: dir, PlanOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "matching" && len(plan.Warnings) == 0 {
				t.Fatal("plan hid code-state gaps")
			}
			if scenario == "missing-commit" && !slices.Contains(plan.Warnings, "missing_commit") {
				t.Fatalf("missing commit not detected: %v", plan.Warnings)
			}
			_, receipt, err := target.Restore(ctx, r.ad, chosen.SessionID, opt)
			if scenario == "matching" || scenario == "override" {
				if err != nil || receipt == nil {
					t.Fatalf("restore: %v", err)
				}
				if scenario == "override" && !slices.Contains(receipt.Import.CodeStateOverride, "head_mismatch") {
					t.Fatalf("override was not recorded: %+v", receipt.Import)
				}
			} else {
				if errcode.CodeOf(err) != errcode.NeedsAction {
					t.Fatalf("expected code-state refusal: %v", err)
				}
				files, _ := filepath.Glob(filepath.Join(opt.Home, "sessions", "*", "*", "*", "*.jsonl"))
				if len(files) != 0 {
					t.Fatal("refusal wrote native files")
				}
			}
		})
	}
}
