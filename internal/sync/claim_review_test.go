package sync

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/session"
	"session-memory/internal/vault"
)

// An adapter wrapper schedules a normal native-file rewrite immediately after
// the real adapter has completed its deep verification. Capture remains real.
type reviewAfterVerify struct {
	agent.Adapter
	after func()
}

func (a *reviewAfterVerify) VerifyBinding(ctx context.Context, ref agent.NativeRef, arts map[string]session.ArtifactState, deep bool) ([]string, error) {
	v := a.Adapter.(interface {
		VerifyBinding(context.Context, agent.NativeRef, map[string]session.ArtifactState, bool) ([]string, error)
	})
	bad, err := v.VerifyBinding(ctx, ref, arts, deep)
	if err == nil && len(bad) == 0 && a.after != nil {
		f := a.after
		a.after = nil
		f()
	}
	return bad, err
}

func TestRegressionClaimRechecksBytesAtBinding(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		name := "unchanged-control"
		if rewrite {
			name = "equal-length-rewrite"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			key, err := vault.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			v, err := vault.New(session.NewVaultID(), key)
			if err != nil {
				t.Fatal(err)
			}
			shared, home := t.TempDir(), t.TempDir()
			wbFixture().write(t, home)
			a := wbNode(t, v, shared, home)
			_, revision, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: home, NativeID: wbID}, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.sync.Publish(ctx); err != nil {
				t.Fatal(err)
			}
			fresh := wbNode(t, v, shared, home)
			realAdapter := fresh.ad
			if rewrite {
				fresh.ad = &reviewAfterVerify{Adapter: realAdapter, after: func() {
					p := filepath.Join(home, "projects/Users-me-work-app", wbID, "tool-results/out.txt")
					old, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					changed := bytes.Replace(old, []byte("line two"), []byte("line NEW"), 1)
					if bytes.Equal(changed, old) || len(changed) != len(old) {
						t.Fatal("invalid rewrite fixture")
					}
					if err := os.WriteFile(p, changed, 0600); err != nil {
						t.Fatal(err)
					}
				}}
			}
			binding, got, claimed, err := fresh.claim(fresh.index(), wbID)
			if !rewrite {
				if err != nil || !claimed || got.RevisionID != revision.RevisionID {
					t.Fatalf("control: claimed=%v err=%v", claimed, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !claimed {
				bindings, err := fresh.st.Bindings("")
				if err != nil || len(bindings) != 0 {
					t.Fatalf("refused claim left bindings: %d %v", len(bindings), err)
				}
				// The fallback still backs up the rewritten content as new work.
				if _, _, err := fresh.eng.Track(ctx, realAdapter, agent.NativeRef{Home: home, NativeID: wbID}, ""); err != nil {
					t.Fatal(err)
				}
				pending, err := fresh.st.UnpublishedRevisions(fresh.eng.DeviceID)
				if err != nil || len(pending) != 1 {
					t.Fatalf("changed content not queued: %d %v", len(pending), err)
				}
				return
			}
			_, changed, checkpointErr := fresh.eng.Checkpoint(ctx, realAdapter, binding.BindingID, "", session.CaptureAuto)
			pending, pendingErr := fresh.st.UnpublishedRevisions(fresh.eng.DeviceID)
			t.Fatalf("rewritten bytes claimed as old revision=%v; next checkpoint changed=%v err=%v; unpublished=%d err=%v", got.RevisionID == revision.RevisionID, changed, checkpointErr, len(pending), pendingErr)
		})
	}
}

func TestRegressionClaimReadFailureMustNotMakeDuplicate(t *testing.T) {
	for _, failure := range []string{"healthy-control", "snapshot-unavailable", "manifest-incomplete", "manifest-gap"} {
		t.Run(failure, func(t *testing.T) {
			a, _, v, shared := claimPair(t)
			ctx := context.Background()
			id, _ := a.put(91, 2)
			original, _, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: id}, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.sync.Publish(ctx); err != nil {
				t.Fatal(err)
			}
			fresh := nodeAt(t, v, shared, a.home)
			ix := fresh.index()
			fetches := 0
			restore := func() { ix.FetchSnapshot = fresh.sync.FetchSnapshot }
			readFailure := errors.New("synthetic temporary snapshot download failure")
			if failure == "snapshot-unavailable" {
				ix.FetchSnapshot = func(context.Context, session.Revision) error {
					fetches++
					return readFailure
				}
			}
			if failure == "manifest-incomplete" || failure == "manifest-gap" {
				paths, err := filepath.Glob(filepath.Join(shared, "manifests", "*", "*.manifest"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("manifests: %v %v", paths, err)
				}
				data, err := os.ReadFile(paths[0])
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths[0], data[:len(data)/2], 0600); err != nil {
					t.Fatal(err)
				}
				restore = func() {
					if err := os.WriteFile(paths[0], data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "manifest-gap" {
					missing := paths[0] + ".missing"
					later := filepath.Join(filepath.Dir(paths[0]), "000000000002-"+filepath.Base(paths[0])[13:])
					if err := os.Rename(paths[0], missing); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(later, data, 0600); err != nil {
						t.Fatal(err)
					}
					restore = func() {
						os.Remove(later)
						os.Remove(missing)
						if err := os.WriteFile(paths[0], data, 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			binding, _, claimed, err := fresh.claim(ix, id)
			if failure == "healthy-control" {
				if err != nil || !claimed || binding.SessionID != original.SessionID {
					t.Fatalf("control: claimed=%v err=%v", claimed, err)
				}
				return
			}
			if failure == "snapshot-unavailable" && fetches == 0 {
				t.Fatal("fixture did not reach snapshot fetch")
			}
			if err != nil {
				if failure == "snapshot-unavailable" && !errors.Is(err, readFailure) {
					t.Fatalf("lost the retryable cause: %v", err)
				}
				bindings, loadErr := fresh.st.Bindings("")
				if loadErr != nil || len(bindings) != 0 {
					t.Fatalf("read failure left a binding: %v %v", bindings, loadErr)
				}
				restore()
				binding, _, claimed, err = fresh.claim(ix, id)
				if err != nil || !claimed || binding.SessionID != original.SessionID {
					t.Fatalf("retry did not claim original: claimed=%v err=%v", claimed, err)
				}
				return
			} // retryable read failure prevents fallback
			if claimed {
				t.Fatal("claimed without complete evidence")
			}
			// This is the exact fallback taken by cmd/session-memory's trackOne.
			duplicate, _, err := fresh.eng.Track(ctx, fresh.ad, agent.NativeRef{Home: a.home, NativeID: id}, "")
			if err != nil {
				t.Fatal(err)
			}
			pending, err := fresh.st.UnpublishedRevisions(fresh.eng.DeviceID)
			if err != nil {
				t.Fatal(err)
			}
			t.Fatalf("cloud read failure became a successful no-match: new session differs=%v, queued revisions=%d", duplicate.SessionID != original.SessionID, len(pending))
		})
	}
}
