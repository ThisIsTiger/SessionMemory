package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"session-memory/internal/platform"
)

// Codex runs the commands in <home>/hooks.json at the events of a session
// (https://developers.openai.com/codex/hooks): event name -> groups of
// handlers. session-memory adds one handler per event in HookEvents so a
// finished reply is handed over at once instead of at the next background
// round. Everything else in the file is the user's and is kept as it is.

// HookEvents are the events session-memory listens to: a session starting (take
// what the other computer sent) and a reply finishing (send what was done).
var HookEvents = []string{"SessionStart", "Stop"}

// hookTimeout is the seconds Codex waits for the command; it only starts a
// background job and returns, so this is a ceiling, not an expectation.
const hookTimeout = 10

// HooksPath is the hooks file of a Codex home.
func HooksPath(home string) string { return filepath.Join(home, "hooks.json") }

// hookMark in front of the command says whose it is, whatever the program is
// called.
const hookMark = "SM_HOOK=1"

// HookCommand is the command line installed for an event. smHome (SM_HOME at
// install time, if any) is carried along because Codex starts the command with
// its own environment.
func HookCommand(exe, smHome, event string) string {
	var b strings.Builder
	b.WriteString(hookMark + " ")
	if smHome != "" {
		b.WriteString("SM_HOME=" + shellQuote(smHome) + " ")
	}
	b.WriteString(shellQuote(exe) + " hook --event " + event)
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellFields splits a command line the way a shell would for the simple
// shapes HookCommand writes (quotes, no expansions).
func shellFields(s string) []string {
	var out []string
	var cur strings.Builder
	in, quote := false, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			in = true
		case c == ' ' || c == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// IsOurHook reports whether a hook command is one session-memory installed:
// the program called with "hook" after the mark (or, for a command typed by
// hand, the session-memory program itself).
func IsOurHook(command string) bool {
	f := shellFields(command)
	marked := false
	for len(f) > 0 && isAssignment(f[0]) {
		marked = marked || f[0] == hookMark
		f = f[1:]
	}
	if len(f) < 2 || f[1] != "hook" {
		return false
	}
	if marked {
		return true
	}
	base := f[0]
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	return base == "session-memory" || base == "session-memory.exe"
}

// isAssignment is a NAME=value word in front of a command.
func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := w[j]
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || j > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// hooksDoc is a hooks file with everything it holds kept verbatim.
type hooksDoc struct {
	top    map[string]json.RawMessage
	events map[string][]json.RawMessage // event -> groups
}

func readHooksDoc(path string) (*hooksDoc, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &hooksDoc{top: map[string]json.RawMessage{}, events: map[string][]json.RawMessage{}}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	d := &hooksDoc{top: map[string]json.RawMessage{}, events: map[string][]json.RawMessage{}}
	if len(bytes.TrimSpace(b)) == 0 {
		return d, true, nil
	}
	if err := json.Unmarshal(b, &d.top); err != nil {
		return nil, true, fmt.Errorf("%s is not a JSON object (%v); it was not touched", path, err)
	}
	raw, ok := d.top["hooks"]
	if !ok {
		return d, true, nil
	}
	var evs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &evs); err != nil {
		return nil, true, fmt.Errorf(`"hooks" in %s is not an object; it was not touched`, path)
	}
	for ev, r := range evs {
		var groups []json.RawMessage
		if err := json.Unmarshal(r, &groups); err != nil {
			return nil, true, fmt.Errorf("the %s entry in %s is not a list; it was not touched", ev, path)
		}
		d.events[ev] = groups
	}
	return d, true, nil
}

func (d *hooksDoc) marshal() ([]byte, error) {
	top := map[string]json.RawMessage{}
	for k, v := range d.top {
		top[k] = v
	}
	evs := map[string][]json.RawMessage{}
	for ev, g := range d.events {
		if len(g) > 0 {
			evs[ev] = g
		}
	}
	if len(evs) > 0 {
		raw, err := json.Marshal(evs)
		if err != nil {
			return nil, err
		}
		top["hooks"] = raw
	} else {
		delete(top, "hooks")
	}
	if len(top) == 0 {
		return nil, nil
	}
	return json.MarshalIndent(top, "", "  ")
}

type handler struct {
	Command string `json:"command"`
}

// split separates a group into the handlers that are session-memory's and
// the rest; rest is nil when nothing of the user's remains in the group.
func splitGroup(g json.RawMessage) (ours []handler, rest json.RawMessage, err error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(g, &m); err != nil {
		return nil, g, nil // not a shape we understand: leave it alone
	}
	var hs []json.RawMessage
	if raw, ok := m["hooks"]; !ok || json.Unmarshal(raw, &hs) != nil {
		return nil, g, nil
	}
	var keep []json.RawMessage
	for _, h := range hs {
		var hd handler
		if json.Unmarshal(h, &hd) == nil && IsOurHook(hd.Command) {
			ours = append(ours, hd)
			continue
		}
		keep = append(keep, h)
	}
	if len(ours) == 0 {
		return nil, g, nil
	}
	if len(keep) == 0 {
		return ours, nil, nil
	}
	m["hooks"], err = json.Marshal(keep)
	if err != nil {
		return nil, nil, err
	}
	rest, err = json.Marshal(m)
	return ours, rest, err
}

// HooksState says what is installed in one Codex home.
type HooksState struct {
	Path      string
	Exists    bool
	Installed map[string]bool // event -> a session-memory handler is there
	Current   map[string]bool // event -> and it is the command that would be installed now
}

// ReadHooks reports which of HookEvents carry a session-memory handler.
// commands maps an event to the command that would be installed now.
func ReadHooks(home string, commands map[string]string) (HooksState, error) {
	st := HooksState{Path: HooksPath(home), Installed: map[string]bool{}, Current: map[string]bool{}}
	d, exists, err := readHooksDoc(st.Path)
	st.Exists = exists
	if err != nil {
		return st, err
	}
	for ev, groups := range d.events {
		for _, g := range groups {
			ours, _, err := splitGroup(g)
			if err != nil {
				return st, err
			}
			for _, h := range ours {
				st.Installed[ev] = true
				if h.Command == commands[ev] {
					st.Current[ev] = true
				}
			}
		}
	}
	return st, nil
}

func writeHooks(path string, existed bool, d *hooksDoc) error {
	out, err := d.marshal()
	if err != nil {
		return err
	}
	if out == nil { // nothing is left in the file
		if existed {
			return os.Remove(path)
		}
		return nil
	}
	if existed {
		// the file as it was stays next to it, once, in case the change is not wanted
		if bak := path + ".sm-bak"; !fileExists(bak) {
			if prev, err := os.ReadFile(path); err == nil {
				if err := platform.AtomicWrite(bak, prev, 0600); err != nil {
					return err
				}
			}
		}
	}
	perm := os.FileMode(0644)
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	return platform.AtomicWrite(path, append(out, '\n'), perm)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// InstallHooks makes sure each event in commands has exactly one
// session-memory handler running that command, leaving the user's own hooks
// as they are. It reports whether the file changed. A file that cannot be read
// as hooks is left alone and reported.
func InstallHooks(home string, commands map[string]string) (bool, error) {
	path := HooksPath(home)
	d, existed, err := readHooksDoc(path)
	if err != nil {
		return false, err
	}
	before, err := d.marshal()
	if err != nil {
		return false, err
	}
	for ev, cmd := range commands {
		var groups []json.RawMessage
		have := 0
		exact := false
		for _, g := range d.events[ev] {
			ours, rest, err := splitGroup(g)
			if err != nil {
				return false, err
			}
			have += len(ours)
			for _, h := range ours {
				exact = exact || h.Command == cmd
			}
			if len(ours) == 0 {
				groups = append(groups, g)
			} else if rest != nil {
				groups = append(groups, rest)
			}
		}
		if have == 1 && exact {
			continue // already as wanted
		}
		entry, err := json.Marshal(map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": hookTimeout}}})
		if err != nil {
			return false, err
		}
		d.events[ev] = append(groups, entry)
	}
	after, err := d.marshal()
	if err != nil {
		return false, err
	}
	if bytes.Equal(before, after) && existed {
		return false, nil
	}
	return true, writeHooks(path, existed, d)
}

// RemoveHooks takes session-memory's handlers out of every event, leaving the
// user's own hooks. It returns how many it removed. A file that holds nothing
// else is removed with them.
func RemoveHooks(home string) (int, error) {
	path := HooksPath(home)
	d, existed, err := readHooksDoc(path)
	if err != nil || !existed {
		return 0, err
	}
	removed := 0
	for ev, groups := range d.events {
		var kept []json.RawMessage
		for _, g := range groups {
			ours, rest, err := splitGroup(g)
			if err != nil {
				return 0, err
			}
			removed += len(ours)
			if len(ours) == 0 {
				kept = append(kept, g)
			} else if rest != nil {
				kept = append(kept, rest)
			}
		}
		d.events[ev] = kept
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, writeHooks(path, existed, d)
}

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
