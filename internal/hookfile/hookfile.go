// Package hookfile adds and removes session-memory's command handlers in the
// JSON file an agent keeps its hooks in. Codex (hooks.json) and WorkBuddy
// (settings.json) use the same shape: a "hooks" object that maps an event name
// to a list of groups, each group holding a list of handlers. In WorkBuddy that
// object sits among the application's own settings, so everything that is not
// ours is written back exactly as it was read, key order and layout included.
package hookfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"session-memory/internal/platform"
)

// Timeout is the seconds the host waits for the command; it only starts a
// background job and returns, so this is a ceiling, not an expectation.
const Timeout = 10

// Mark in front of the command says whose it is, whatever the program is
// called.
const Mark = "SM_HOOK=1"

// Command is the command line installed for an event of an agent. smHome
// (SM_HOME at install time, if any) is carried along because the host starts
// the command with its own environment.
func Command(exe, smHome, event, agent string) string {
	var b strings.Builder
	b.WriteString(Mark + " ")
	if smHome != "" {
		b.WriteString("SM_HOME=" + shellQuote(smHome) + " ")
	}
	b.WriteString(shellQuote(exe) + " hook --event " + event + " --agent " + agent)
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Fields splits a command line the way a shell would for the simple
// shapes Command writes (quotes, no expansions).
func Fields(s string) []string {
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

// IsOurs reports whether a hook command is one session-memory installed: the
// program called with "hook" after the mark (or, for a command typed by hand,
// the session-memory program itself).
func IsOurs(command string) bool {
	f := Fields(command)
	marked := false
	for len(f) > 0 && isAssignment(f[0]) {
		marked = marked || f[0] == Mark
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

// doc is a file with everything in it kept verbatim except the handlers of
// ours that are being added or removed.
type doc struct {
	order  []string // top-level keys as they stand in the file
	top    map[string]json.RawMessage
	events map[string][]json.RawMessage // event -> groups
}

func newDoc() *doc {
	return &doc{top: map[string]json.RawMessage{}, events: map[string][]json.RawMessage{}}
}

// parseTop reads a JSON object into its keys, in file order, and the exact
// bytes of each value.
func parseTop(b []byte) (order []string, top map[string]json.RawMessage, err error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, errors.New("not a JSON object")
	}
	top = map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, nil, errors.New("not a JSON object")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, err
		}
		if _, seen := top[key]; !seen {
			order = append(order, key)
		}
		top[key] = raw
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, nil, errors.New("not a JSON object")
	}
	if _, err := dec.Token(); err == nil {
		return nil, nil, errors.New("more than one JSON value")
	}
	return order, top, nil
}

func read(path string) (*doc, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newDoc(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	d := newDoc()
	if len(bytes.TrimSpace(b)) == 0 {
		return d, true, nil
	}
	if d.order, d.top, err = parseTop(b); err != nil {
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

func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSpace(b.String())
}

// hooksValue renders the "hooks" object for a file whose other keys sit at
// two spaces; nil when no event has a group.
func (d *doc) hooksValue() ([]byte, error) {
	evs := map[string][]json.RawMessage{}
	for ev, g := range d.events {
		if len(g) > 0 {
			evs[ev] = g
		}
	}
	if len(evs) == 0 {
		return nil, nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("  ", "  ")
	if err := enc.Encode(evs); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

// marshal writes the file; nil when nothing is left in it.
func (d *doc) marshal() ([]byte, error) {
	hv, err := d.hooksValue()
	if err != nil {
		return nil, err
	}
	var entries []string
	placed := false
	for _, k := range d.order {
		if k == "hooks" {
			if hv != nil {
				entries = append(entries, "  "+quote(k)+": "+string(hv))
				placed = true
			}
			continue
		}
		entries = append(entries, "  "+quote(k)+": "+string(d.top[k]))
	}
	if hv != nil && !placed {
		entries = append(entries, "  \"hooks\": "+string(hv))
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return []byte("{\n" + strings.Join(entries, ",\n") + "\n}"), nil
}

type handler struct {
	Command string `json:"command"`
}

// splitGroup separates a group into the handlers that are session-memory's and
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
		if json.Unmarshal(h, &hd) == nil && IsOurs(hd.Command) {
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

// State says what is installed in one hooks file.
type State struct {
	Path      string
	Exists    bool
	Installed map[string]bool // event -> a session-memory handler is there
	Current   map[string]bool // event -> and it is the command that would be installed now
}

// Read reports which events carry a session-memory handler. commands maps an
// event to the command that would be installed now.
func Read(path string, commands map[string]string) (State, error) {
	st := State{Path: path, Installed: map[string]bool{}, Current: map[string]bool{}}
	d, exists, err := read(path)
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

// Key says whether the file has a top-level key and its raw value, for the
// callers that look at the host's own switches (for example disableAllHooks).
func Key(path, key string) (json.RawMessage, bool) {
	d, _, err := read(path)
	if err != nil {
		return nil, false
	}
	v, ok := d.top[key]
	return v, ok
}

func write(path string, existed, deleteEmpty bool, d *doc) error {
	out, err := d.marshal()
	if err != nil {
		return err
	}
	if out == nil { // nothing is left in the file
		if !deleteEmpty {
			out = []byte("{}")
		} else {
			if existed {
				return os.Remove(path)
			}
			return nil
		}
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

// Install makes sure each event in commands has exactly one session-memory
// handler running that command, leaving everything else as it is. It reports
// whether the file changed. A file that cannot be read as hooks is left alone
// and reported. deleteEmpty says whether a file that ends up with nothing in it
// is removed (a hooks file of our own making) or kept as {} (the host's
// settings).
func Install(path string, commands map[string]string, deleteEmpty bool) (bool, error) {
	d, existed, err := read(path)
	if err != nil {
		return false, err
	}
	before, err := d.marshal()
	if err != nil {
		return false, err
	}
	// an event an earlier version listened to and this one does not
	for ev, groups := range d.events {
		if _, wanted := commands[ev]; wanted {
			continue
		}
		var kept []json.RawMessage
		for _, g := range groups {
			ours, rest, err := splitGroup(g)
			if err != nil {
				return false, err
			}
			if len(ours) == 0 {
				kept = append(kept, g)
			} else if rest != nil {
				kept = append(kept, rest)
			}
		}
		d.events[ev] = kept
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
		entry, err := json.Marshal(map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd, "timeout": Timeout}}})
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
	return true, write(path, existed, deleteEmpty, d)
}

// Remove takes session-memory's handlers out of every event, leaving the rest.
// It returns how many it removed.
func Remove(path string, deleteEmpty bool) (int, error) {
	d, existed, err := read(path)
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
	return removed, write(path, existed, deleteEmpty, d)
}
