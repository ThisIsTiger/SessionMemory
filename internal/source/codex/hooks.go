package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"session-memory/internal/hookfile"
)

// Codex runs the commands in <home>/hooks.json at the events of a session
// (https://developers.openai.com/codex/hooks): event name -> groups of
// handlers. session-memory adds one handler per event in HookEvents so a
// finished reply is uploaded at once instead of at the next background round.
// Everything else in the file is the user's and is kept as it is. The file
// handling is shared with WorkBuddy (package hookfile).

// HookEvents are the events session-memory listens to: a reply finishing (send
// what was done). A session starting is not one: what other computers sent
// cannot be put into a Codex that is already running anyway.
var HookEvents = []string{"Stop"}

// HooksPath is the hooks file of a Codex home.
func HooksPath(home string) string { return filepath.Join(home, "hooks.json") }

// HookCommand is the command line installed for an event.
func HookCommand(exe, smHome, event string) string {
	return hookfile.Command(exe, smHome, event, AgentName)
}

// IsOurHook reports whether a hook command is one session-memory installed.
func IsOurHook(command string) bool { return hookfile.IsOurs(command) }

// HooksState says what is installed in one Codex home.
type HooksState = hookfile.State

// ReadHooks reports which events carry a session-memory handler. commands maps
// an event to the command that would be installed now.
func ReadHooks(home string, commands map[string]string) (HooksState, error) {
	return hookfile.Read(HooksPath(home), commands)
}

// InstallHooks makes sure each event in commands has exactly one
// session-memory handler running that command, leaving the user's own hooks as
// they are. It reports whether the file changed.
func InstallHooks(home string, commands map[string]string) (bool, error) {
	return hookfile.Install(HooksPath(home), commands, true)
}

// RemoveHooks takes session-memory's handlers out of every event, leaving the
// user's own hooks. It returns how many it removed. A file that holds nothing
// else is removed with them.
func RemoveHooks(home string) (int, error) { return hookfile.Remove(HooksPath(home), true) }

// HookTrust is what Codex itself says about one installed hook.
type HookTrust struct {
	Event   string `json:"eventName"`
	Command string `json:"command"`
	Trust   string `json:"trustStatus"` // trusted | untrusted | modified
	Enabled bool   `json:"enabled"`
}

// SessionMemoryHookTrust asks a short-lived Codex app-server (hooks/list) how
// it sees the session-memory hooks of a home. Codex runs a hook only after the
// user has trusted it in /hooks; this is how that is told apart from "not
// installed". It never changes anything.
func SessionMemoryHookTrust(ctx context.Context, home, binary string) ([]HookTrust, error) {
	bin, err := findCodexBinary(binary)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
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
	if err := send(map[string]any{"id": 2, "method": "hooks/list", "params": map[string]any{"cwds": []string{home}}}); err != nil {
		return nil, err
	}
	raw, err := await(2)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data []struct {
			Hooks []HookTrust `json:"hooks"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return nil, errors.New("unreadable hooks/list result")
	}
	var out []HookTrust
	seen := map[string]bool{}
	for _, e := range res.Data {
		for _, h := range e.Hooks {
			if IsOurHook(h.Command) && !seen[h.Event+"\x00"+h.Command] {
				seen[h.Event+"\x00"+h.Command] = true
				out = append(out, h)
			}
		}
	}
	return out, nil
}
