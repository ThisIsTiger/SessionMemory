//go:build failpoint

package workbuddy

import (
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
	"session-memory/internal/source/codex"
	"session-memory/internal/store"
	"session-memory/internal/testutil/codexgen"
)

// A cross-agent conversion is an ordinary install transaction, so it must obey
// the same crash rules: before `verified` everything is undone (no file, no
// registry row, no binding), from `verified` on it is rolled forward to a
// complete converted binding.
func TestConvertInstallCrashMatrix(t *testing.T) {
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
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			codexHome, wbHome, cwdA, cwdB := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			db := testRegistry(t, wbHome, "account-B")
			tid := codexgen.ThreadIDFor(55)
			p := filepath.Join(codexHome, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
			os.MkdirAll(filepath.Dir(p), 0700)
			os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 55, Turns: 2, ToolCalls: 1, Cwd: cwdA}), 0600)
			cfg := config.DefaultConfig()
			cfg.Agents.Codex.Home = codexHome
			cfg.Agents.WorkBuddy.Home = wbHome
			cx := codex.New(cfg)
			w := testWriter()
			wb := writerTestAdapter{New(cfg), w}
			st := writerStore(t)
			e := &engine.Engine{Store: st, DeviceID: session.NewDeviceID()}
			cb, rev, err := e.Track(ctx, cx, agent.NativeRef{Home: codexHome, NativeID: tid}, "")
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
				_, _, err = e.ConvertRestore(ctx, cx, wb, cb.SessionID, engine.RestoreOptions{Home: wbHome, Cwd: cwdB, ClientClosed: true})
			}()
			t.Setenv("SM_FAILPOINTS", "")
			if !panicked {
				t.Fatalf("failpoint not reached: %v", err)
			}
			sealer, root := st.Sealer(), st.Root
			st.Close()
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
			if journals, _ := filepath.Glob(filepath.Join(root, "journal", "*.json")); len(journals) != 0 {
				t.Fatal("unfinished journal after recovery")
			}
			files, _ := filepath.Glob(filepath.Join(wbHome, "projects", "*", "*.jsonl"))
			var rows int
			db.QueryRow("SELECT count(*) FROM sessions WHERE id<>'local-seed'").Scan(&rows)
			bs, _ := reopened.Bindings(cb.SessionID)
			forward := point == "install.after_verified" || strings.Contains(point, "binding_committed")
			if forward {
				var conv *session.Binding
				for i := range bs {
					if bs[i].InstallMode == session.InstallConverted {
						conv = &bs[i]
					}
				}
				if conv == nil || conv.State != session.StateActive || conv.InstalledRevisionID != rev.RevisionID || conv.ImportReceipt == nil || len(files) != 1 || rows != 1 {
					t.Fatalf("converted install not rolled forward: bindings=%+v files=%v rows=%d", bs, files, rows)
				}
				out, _ := os.ReadFile(filepath.Join(root, "device", "outbox.json"))
				if !strings.Contains(string(out), session.InstallConverted) {
					t.Fatalf("receipt of the converted install not queued with install_mode converted: %s", out)
				}
			} else if len(bs) != 1 || bs[0].AgentProfile.Agent != "codex" || len(files) != 0 || rows != 0 {
				t.Fatalf("not undone: bindings=%+v files=%v rows=%d", bs, files, rows)
			}
			// either way a fresh conversion attempt is consistent
			if !forward {
				e2 := &engine.Engine{Store: reopened, DeviceID: e.DeviceID}
				if _, _, err := e2.ConvertRestore(ctx, cx, wb, cb.SessionID, engine.RestoreOptions{Home: wbHome, Cwd: cwdB, ClientClosed: true}); err != nil {
					t.Fatalf("retry after recovery: %v", err)
				}
			}
		})
	}
}

// Converting back to the origin agent creates a new native session and retires
// the old binding. A crash must never leave two active bindings or none.
func TestConvertBackSupersedeCrashMatrix(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS native path format")
	}
	var points []string
	for _, state := range store.Flow[store.KindInstall] {
		if state.State != store.StDone {
			points = append(points, "install.before_"+string(state.State), "install.after_"+string(state.State))
		}
	}
	points = append(points, "install.mid_write")
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			codexHome, wbHome, cwdA, cwdB := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			testRegistry(t, wbHome, "account-B")
			tid := codexgen.ThreadIDFor(56)
			p := filepath.Join(codexHome, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
			os.MkdirAll(filepath.Dir(p), 0700)
			os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 56, Turns: 1, Cwd: cwdA}), 0600)
			cfg := config.DefaultConfig()
			cfg.Agents.Codex.Home = codexHome
			cfg.Agents.WorkBuddy.Home = wbHome
			cx := codex.New(cfg)
			w := testWriter()
			wb := writerTestAdapter{New(cfg), w}
			st := writerStore(t)
			e := &engine.Engine{Store: st, DeviceID: session.NewDeviceID()}
			cb, _, err := e.Track(ctx, cx, agent.NativeRef{Home: codexHome, NativeID: tid}, "")
			if err != nil {
				t.Fatal(err)
			}
			_, rec, err := e.ConvertRestore(ctx, cx, wb, cb.SessionID, engine.RestoreOptions{Home: wbHome, Cwd: cwdB, ClientClosed: true})
			if err != nil {
				t.Fatal(err)
			}
			wbFile := filepath.Join(wbHome, "projects", strings.Trim(strings.ReplaceAll(cwdB, "/", "-"), "-"), rec.NativeSessionID+".jsonl")
			f, _ := os.OpenFile(wbFile, os.O_APPEND|os.O_WRONLY, 0600)
			f.WriteString(line(map[string]any{"type": "message", "id": "c1", "role": "user", "sessionId": rec.NativeSessionID, "cwd": cwdB, "timestamp": int64(1893000000000),
				"content": []any{map[string]any{"type": "input_text", "text": "<user_query>\nmore\n</user_query>"}}}))
			f.Close()
			var wbb session.Binding
			for _, b := range mustBindings(t, e, cb.SessionID) {
				if b.AgentProfile.Agent == AgentName {
					wbb = b
				}
			}
			if _, changed, err := e.Checkpoint(ctx, wb, wbb.BindingID, "", session.CaptureCheckpoint); err != nil || !changed {
				t.Fatalf("continuation: %v", err)
			}

			t.Setenv("SM_FAILPOINTS", point+"=panic")
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				_, _, err = e.ConvertRestore(ctx, wb, cx, cb.SessionID, engine.RestoreOptions{Home: codexHome, Cwd: cwdA, ClientClosed: true})
			}()
			t.Setenv("SM_FAILPOINTS", "")
			if !panicked {
				t.Fatalf("failpoint not reached: %v", err)
			}
			sealer, root := st.Sealer(), st.Root
			st.Close()
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
			active, newer := 0, 0
			bs, _ := reopened.Bindings(cb.SessionID)
			for _, b := range bs {
				if b.AgentProfile.Agent != "codex" {
					continue
				}
				if b.State == session.StateActive {
					active++
				}
				if b.NativeSessionID != tid {
					newer++
				}
			}
			threads, _ := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*.jsonl"))
			if active != 1 {
				t.Fatalf("active Codex bindings after recovery = %d (%+v)", active, bs)
			}
			if forward && (newer != 1 || len(threads) != 2) || !forward && (newer != 0 || len(threads) != 1) {
				t.Fatalf("forward=%v newer=%d threads=%v", forward, newer, threads)
			}
		})
	}
}
