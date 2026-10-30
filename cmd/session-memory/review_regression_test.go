package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/session"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
)

func TestRegressionResetRefusesBusyStore(t *testing.T) {
	root := t.TempDir()
	m := newComputer(t, root, "a", filepath.Join(root, "cloud"))
	m.must("init")
	state := filepath.Join(m.home, "sm")
	unlock, err := lockStore(state)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	oldWait := lockWait
	lockWait = 20 * time.Millisecond
	defer func() { lockWait = oldWait }()
	code, _, _ := m.run("reset", "--yes")
	_, statErr := os.Stat(filepath.Join(state, "device", "device.json"))
	// Check the directory itself as well: the holder's open lock must not be moved.
	_, dirErr := os.Stat(state)
	if code == 0 || os.IsNotExist(dirErr) {
		t.Fatalf("reset moved a store with an active lock: code=%d state=%v device=%v", code, dirErr, statErr)
	}
}

func TestRegressionWidenSetupWindow(t *testing.T) {
	for _, command := range []string{"setup", "setup-all", "project-track"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			m := newComputer(t, root, "a", filepath.Join(root, "cloud"))
			project := filepath.Join(m.home, "work", "app")
			os.MkdirAll(project, 0700)
			addCodexSession(m, 811, project)
			addCodexSession(m, 812, project)
			for _, p := range mustGlob(t, filepath.Join(m.home, "codex", "sessions", "*", "*", "*", "*.jsonl")) {
				if strings.Contains(p, codexgen.ThreadIDFor(811)) {
					old := time.Now().Add(-20 * 24 * time.Hour)
					if err := os.Chtimes(p, old, old); err != nil {
						t.Fatal(err)
					}
				}
			}
			selfVia(t, m, nil)
			m.must("setup", "--no-autosync", "--days", "3")
			if n := sessionLines(m); n != 1 {
				t.Fatalf("fixture count %d", n)
			}
			if strings.HasPrefix(command, "setup") {
				days := "30"
				if command == "setup-all" {
					days = "36500"
				}
				m.must("setup", "--no-autosync", "--days", days)
			} else {
				m.must("project", "track", "--agent", "codex", "--cwd", project)
			}
			if n := sessionLines(m); n != 2 {
				t.Fatalf("requested older history but still only %d session(s)", n)
			}
		})
	}
}

func TestSetupNarrowerWindowUpdatesAllWatchers(t *testing.T) {
	root := t.TempDir()
	m := newComputer(t, root, "a", filepath.Join(root, "cloud"))
	project := filepath.Join(m.home, "work", "app")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	addCodexSession(m, 821, project)
	selfVia(t, m, nil)
	m.must("setup", "--no-autosync", "--days", "30")
	a := &App{}
	a.Cfg.Home = filepath.Join(m.home, "sm")
	projects := loadProjects(a.Cfg.StateDir())
	if len(projects) != 1 {
		t.Fatalf("fixture projects: %d", len(projects))
	}
	alias := projects[0]
	alias.ProjectRef = session.NewProjectRef()
	if err := saveProject(a, alias); err != nil {
		t.Fatal(err)
	}
	m.must("setup", "--no-autosync", "--days", "3")
	// A previously unseen old session appears after the policy was narrowed.
	addCodexSession(m, 822, project)
	for _, p := range mustGlob(t, filepath.Join(m.home, "codex", "sessions", "*", "*", "*", "*.jsonl")) {
		if strings.Contains(p, codexgen.ThreadIDFor(822)) {
			old := time.Now().Add(-20 * 24 * time.Hour)
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	m.must("relay", "--client-closed")
	if n := sessionLines(m); n != 1 {
		t.Fatalf("a stale watcher bypassed the new window: %d sessions", n)
	}
	m.must("project", "track", "--agent", "codex", "--cwd", project)
	if n := sessionLines(m); n != 2 {
		t.Fatalf("full project tracking retained the window: %d sessions", n)
	}
}

// The same fully populated fake vault works with a fresh saved token. In the
// failing branch reset removes identity, then the skill receives a fresh token.
func TestRegressionReauthAfterReset(t *testing.T) {
	for _, testCase := range []string{"fresh-control", "reset-and-reauthorize", "wrong-key", "wrong-vault-id", "invalid-key-format", "damaged-identity"} {
		t.Run(testCase, func(t *testing.T) {
			stale := testCase != "fresh-control"
			reject := testCase != "fresh-control" && testCase != "reset-and-reauthorize"
			root := t.TempDir()
			m := newComputer(t, root, "a", filepath.Join(root, "unused"))
			m.must("init")
			m.must("reset", "--yes")
			m.must("config", "set", "transport.weiyun", "SessionMemory")
			m.must("config", "set", "agents.codex.home", filepath.Join(m.home, "codex"))
			if testCase == "damaged-identity" {
				p := filepath.Join(m.home, "sm", "device", "device.json")
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("{invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			key, err := vault.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			id := session.NewVaultID()
			v, _ := vault.New(id, key)
			info, _ := v.NewInfo(session.FormatTime(time.Now()))
			public, _ := json.Marshal(info)
			kf := keyFile{Format: keyFormat, VaultID: id, Key: base64.StdEncoding.EncodeToString(key)}
			switch testCase {
			case "wrong-key":
				other, err := vault.GenerateKey()
				if err != nil {
					t.Fatal(err)
				}
				kf.Key = base64.StdEncoding.EncodeToString(other)
			case "wrong-vault-id":
				kf.VaultID = session.NewVaultID()
			case "invalid-key-format":
				kf.Format = "unknown"
			}
			keyJSON, _ := json.Marshal(kf)
			files := map[string][]byte{"vault.json": public, "vault-key.json": keyJSON}
			const fresh = "SYNTHETIC_NEW_AUTH"
			saved := fresh
			if stale {
				saved = "SYNTHETIC_EXPIRED_AUTH"
			}
			m.keys[weiyunTokenKey] = []byte(saved)
			writeSkillToken(t, m.home, fresh)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/dl/") {
					w.Write(files[strings.TrimPrefix(r.URL.Path, "/dl/")])
					return
				}
				if r.Header.Get("WyHeader") != "mcp_token="+fresh {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				var req struct {
					Params struct {
						Name string         `json:"name"`
						Args map[string]any `json:"arguments"`
					}
				}
				json.NewDecoder(r.Body).Decode(&req)
				out := map[string]any{"finish_flag": true, "pdir_key": "root", "dir_list": []any{}, "file_list": []any{}}
				if req.Params.Name == "weiyun.download" {
					items := req.Params.Args["items"].([]any)
					f := items[0].(map[string]any)["file_id"].(string)
					out = map[string]any{"items": []map[string]string{{"https_download_url": "http://" + r.Host + "/dl/" + f}}}
				} else if req.Params.Name == "weiyun.list" {
					switch req.Params.Args["dir_key"] {
					case nil, "":
						out["dir_list"] = []map[string]string{{"dir_key": "vault", "dir_name": "SessionMemory"}}
					case "vault":
						out["dir_list"] = []map[string]string{{"dir_key": "public", "dir_name": "public"}}
					case "public":
						out["file_list"] = []map[string]any{{"file_id": "vault.json", "filename": "vault.json", "file_size": len(public)}, {"file_id": "vault-key.json", "filename": "vault-key.json", "file_size": len(keyJSON)}}
					}
				} else {
					out = map[string]any{"error": "unexpected fake operation"}
				}
				b, _ := json.Marshal(out)
				json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}}})
			}))
			defer srv.Close()
			t.Setenv("WEIYUN_MCP_URL", srv.URL)
			selfVia(t, m, nil)
			code, out, errs := m.run("setup", "--no-autosync", "--days", "3")
			if reject {
				if code == 0 || string(m.keys[weiyunTokenKey]) != saved {
					t.Fatalf("invalid onboarding replaced saved credentials: code=%d", code)
				}
				if strings.Contains(out+errs, fresh) || strings.Contains(out+errs, saved) {
					t.Fatal("authentication failure printed a synthetic credential")
				}
				return
			}
			if code != 0 || !strings.Contains(out, "done: JOINED") {
				t.Fatalf("reset + setup failed (stale=%v): code=%d %s", stale, code, errs)
			}
			if string(m.keys[weiyunTokenKey]) != fresh {
				t.Fatal("fresh credential was not saved")
			}
		})
	}
}
