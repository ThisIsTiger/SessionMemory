//go:build failpoint

package workbuddy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/engine"
	"session-memory/internal/session"
	"session-memory/internal/store"
)

func TestWorkBuddyInstallCrashMatrix(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS native path format")
	}
	var points []string
	for _, state := range store.Flow[store.KindInstall] {
		if state.State != store.StDone {
			points = append(points, "install.before_"+string(state.State), "install.after_"+string(state.State))
		}
	}
	points = append(points, "install.mid_write", "install.after_registry")
	for _, update := range []bool{false, true} {
		mode := "new"
		if update {
			mode = "fast-forward"
		}
		for _, point := range points {
			t.Run(mode+"/"+point, func(t *testing.T) {
				ctx := context.Background()
				homeA, homeB, pa, pb := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
				testRegistry(t, homeA, "account-A")
				testRegistry(t, homeB, "account-B")
				srcPath := filepath.Join(homeA, "projects", strings.Trim(strings.ReplaceAll(pa, "/", "-"), "-"), testNativeID+".jsonl")
				os.MkdirAll(filepath.Dir(srcPath), 0700)
				os.WriteFile(srcPath, nativeFixture(pa), 0600)
				stA := writerStore(t)
				stB := peerStore(t, stA)
				cfg := config.DefaultConfig()
				cfg.Agents.WorkBuddy.Home = homeA
				w := testWriter()
				ad := writerTestAdapter{New(cfg), w}
				eA := &engine.Engine{Store: stA, DeviceID: session.NewDeviceID()}
				eB := &engine.Engine{Store: stB, DeviceID: session.NewDeviceID()}
				bnd, rev, err := eA.Track(ctx, ad, agent.NativeRef{Home: homeA, NativeID: testNativeID}, "")
				if err != nil {
					t.Fatal(err)
				}
				mirrorRevisions(t, stA, stB, bnd.SessionID)
				if update {
					if _, _, err := eB.Restore(ctx, ad, bnd.SessionID, engine.RestoreOptions{Home: homeB, Cwd: pb, ClientClosed: true}); err != nil {
						t.Fatal(err)
					}
					f, err := os.OpenFile(srcPath, os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					f.WriteString(line(map[string]any{"type": "message", "id": "u2", "parentId": "a1", "role": "user", "content": "next revision", "cwd": pa, "timestamp": int64(1793000000002), "sessionId": testNativeID}))
					f.Close()
					rev, _, err = eA.Checkpoint(ctx, ad, bnd.BindingID, "", session.CaptureCheckpoint)
					if err != nil {
						t.Fatal(err)
					}
					mirrorRevisions(t, stA, stB, bnd.SessionID)
				}
				targetPath := filepath.Join(homeB, "projects", strings.Trim(strings.ReplaceAll(pb, "/", "-"), "-"), testNativeID+".jsonl")
				oldFile, _ := os.ReadFile(targetPath)
				db, err := openRegistry(homeB, false)
				if err != nil {
					t.Fatal(err)
				}
				oldRow, err := readRow(ctx, db, testNativeID)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("SM_FAILPOINTS", point+"=panic")
				panicked := false
				func() {
					defer func() {
						if recover() != nil {
							panicked = true
						}
					}()
					_, _, err = eB.Restore(ctx, ad, bnd.SessionID, engine.RestoreOptions{Home: homeB, Cwd: pb, ClientClosed: true})
				}()
				t.Setenv("SM_FAILPOINTS", "")
				if !panicked {
					t.Fatalf("failpoint not reached: %v", err)
				}
				sealer, root := stB.Sealer(), stB.Root
				stB.Close()
				reopened, err := store.Open(root, sealer)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				reopened.InstallRowUndo = UndoRegistry
				reopened.InstallGuard = func(ctx context.Context, d store.InstallData) (func(), error) { return w.lockHome(ctx, d.Home) }
				if _, err := reopened.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				forward := point == "install.after_verified" || strings.Contains(point, "binding_committed")
				db, err = openRegistry(homeB, false)
				if err != nil {
					t.Fatal(err)
				}
				finalRow, err := readRow(ctx, db, testNativeID)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
				file, _ := os.ReadFile(targetPath)
				if forward {
					bs, err := reopened.Bindings(bnd.SessionID)
					if err != nil {
						t.Fatal(err)
					}
					if len(bs) != 1 || bs[0].InstalledRevisionID != rev.RevisionID || finalRow == nil {
						t.Fatal("verified install not rolled forward")
					}
					if update && !bytes.Contains(file, []byte("next revision")) {
						t.Fatal("lost fast-forward")
					}
				} else if rowHash(finalRow) != rowHash(oldRow) || !bytes.Equal(file, oldFile) {
					t.Fatal("rollback changed baseline row or files")
				}
				if journals, _ := filepath.Glob(filepath.Join(root, "journal", "*.json")); len(journals) != 0 {
					t.Fatal("unfinished journal after recovery")
				}
			})
		}
	}
}
