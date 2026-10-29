package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/device"
	"session-memory/internal/vault"
)

// reauthCloud is a Weiyun that accepts only the token it was given and holds
// one vault folder whose public/vault.json carries vaultID. Synthetic tokens only.
func reauthCloud(t *testing.T, accepts, vaultID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "vault_id": vaultID})
			return
		}
		if r.Header.Get("WyHeader") != "mcp_token="+accepts {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		var req struct {
			Params struct {
				Name string         `json:"name"`
				Args map[string]any `json:"arguments"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		dir := func(k, n string) map[string]any { return map[string]any{"dir_key": k, "dir_name": n} }
		list := func(dirs []map[string]any, files []map[string]any) map[string]any {
			return map[string]any{"pdir_key": "root", "dir_list": dirs, "file_list": files, "finish_flag": true}
		}
		var out any
		switch key, _ := req.Params.Args["dir_key"].(string); {
		case req.Params.Name == "weiyun.download":
			out = map[string]any{"items": []map[string]any{{"https_download_url": "http://" + r.Host + "/dl/vault.json"}}}
		case req.Params.Name != "weiyun.list":
			out = map[string]any{"error": "not supported by this fake"}
		case key == "":
			out = list([]map[string]any{dir("kr", "SessionMemory")}, nil)
		case key == "kr":
			out = list([]map[string]any{dir("kp", "public")}, nil)
		case key == "kp":
			out = list(nil, []map[string]any{{"file_id": "id-vault.json", "filename": "vault.json", "file_size": 0}})
		default:
			out = list(nil, nil)
		}
		b, _ := json.Marshal(out)
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeSkillToken(t *testing.T, home, token string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"weiyun": map[string]any{"headers": map[string]string{"WyHeader": "mcp_token=" + token}}}})
	if err := os.MkdirAll(filepath.Join(home, ".mcporter"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mcporter", "mcporter.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// After the user authorises again in the Weiyun skill, the rejected saved
// token is replaced by the skill's new one, but only for the same vault.
func TestReauthorizedSkillTokenReplacesRejectedSavedToken(t *testing.T) {
	const stale, fresh = "SYNTHETIC_EXPIRED_TOKEN", "SYNTHETIC_REAUTHORIZED_TOKEN"
	for _, sameVault := range []bool{true, false} {
		root := t.TempDir()
		m := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
		os.MkdirAll(m.home, 0700)
		m.must("init")
		m.must("config", "set", "transport.weiyun", "SessionMemory")
		info, err := device.At(filepath.Join(m.home, "sm")).Load()
		if err != nil {
			t.Fatal(err)
		}
		id := info.VaultID
		if !sameVault {
			id = "some-other-vault"
		}
		srv := reauthCloud(t, fresh, id)
		t.Setenv("WEIYUN_MCP_URL", srv.URL)
		m.keys[weiyunTokenKey] = []byte(stale)
		writeSkillToken(t, m.home, fresh)

		code, _, errs := m.run("relay", "--client-closed")
		saved := string(m.keys[weiyunTokenKey])
		if sameVault {
			if code != 0 || saved != fresh {
				t.Fatalf("same vault: exit %d, saved token replaced=%v\n%s", code, saved == fresh, errs)
			}
			continue
		}
		if code == 0 || saved != stale {
			t.Fatalf("a token that opens a different vault must not be adopted: exit %d, saved replaced=%v", code, saved != stale)
		}
		if strings.Contains(errs, stale) || strings.Contains(errs, fresh) {
			t.Fatalf("a token was printed: %s", errs)
		}
	}
}

// A token given in the environment is the user's explicit choice: never swapped.
func TestEnvironmentTokenIsNeverReplacedBySkillToken(t *testing.T) {
	root := t.TempDir()
	m := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(m.home, 0700)
	m.must("init")
	m.must("config", "set", "transport.weiyun", "SessionMemory")
	info, _ := device.At(filepath.Join(m.home, "sm")).Load()
	srv := reauthCloud(t, "SYNTHETIC_REAUTHORIZED_TOKEN", info.VaultID)
	t.Setenv("WEIYUN_MCP_URL", srv.URL)
	t.Setenv(envWeiyunToken, "SYNTHETIC_EXPIRED_TOKEN")
	writeSkillToken(t, m.home, "SYNTHETIC_REAUTHORIZED_TOKEN")
	if code, _, _ := m.run("relay", "--client-closed"); code == 0 {
		t.Fatal("an explicit environment token that is rejected must fail, not be swapped")
	}
	if len(m.keys[weiyunTokenKey]) != 0 {
		t.Fatal("nothing should have been saved")
	}
}
