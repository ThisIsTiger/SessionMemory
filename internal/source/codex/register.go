package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// appServerCandidates are the places a Codex binary that shares the desktop
// app's backend is looked for when none is configured.
func appServerCandidates() []string {
	return []string{
		"/Applications/ChatGPT.app/Contents/Resources/codex",
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex",
		"/Applications/Codex.app/Contents/Resources/codex",
	}
}

func findCodexBinary(configured string) (string, error) {
	cands := appServerCandidates()
	if configured != "" {
		cands = []string{configured}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	if configured == "" {
		if p, err := exec.LookPath("codex"); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Codex binary found")
}

// Register makes Codex itself register thread ids that were installed as plain
// rollout files: the desktop client only reads its own registry at start-up,
// while thread/list scans the sessions directory and repairs the registry
// (experiments E1a/E2b and the two-machine run). It talks to a short-lived
// stdio app-server of the given binary (default: the one bundled with the
// desktop app) and never touches Codex's files itself. It returns the ids that
// the listing reported.
func Register(ctx context.Context, home, binary string, ids []string) ([]string, error) {
	bin, err := findCodexBinary(binary)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "app-server")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		stdin.Close()
		cancel()
		cmd.Wait()
	}()
	rd := bufio.NewReaderSize(stdout, 1<<20)
	send := func(v map[string]any) error {
		b, _ := json.Marshal(v)
		_, err := stdin.Write(append(b, '\n'))
		return err
	}
	await := func(id int) (json.RawMessage, error) {
		for {
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return nil, fmt.Errorf("app-server closed: %w", err)
			}
			var m struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(line, &m) != nil || m.ID == nil || *m.ID != id {
				continue
			}
			if len(m.Error) > 0 {
				return nil, fmt.Errorf("app-server: %s", strings.TrimSpace(string(m.Error)))
			}
			return m.Result, nil
		}
	}
	if err := send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "session-memory", "title": "SessionMemory", "version": "0"}}}); err != nil {
		return nil, err
	}
	if _, err := await(1); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"method": "initialized"}); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[strings.ToLower(id)] = true
	}
	found := []string{}
	cursor := any(nil)
	for page := 0; page < 20 && len(found) < len(want); page++ {
		params := map[string]any{"limit": 100}
		if cursor != nil {
			params["cursor"] = cursor
		}
		if err := send(map[string]any{"id": 2 + page, "method": "thread/list", "params": params}); err != nil {
			return found, err
		}
		raw, err := await(2 + page)
		if err != nil {
			return found, err
		}
		var res struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &res) != nil {
			return found, errors.New("unreadable thread/list result")
		}
		for _, t := range res.Data {
			if want[strings.ToLower(t.ID)] {
				found = append(found, t.ID)
			}
		}
		if res.NextCursor == nil || *res.NextCursor == "" {
			break
		}
		cursor = *res.NextCursor
	}
	return found, nil
}
