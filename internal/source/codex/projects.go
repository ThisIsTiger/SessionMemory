package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"session-memory/internal/platform"
)

// The desktop app keeps which project a thread belongs to in its own state
// file, not in Codex's databases (threads.project_id stays NULL) and not behind
// any app-server request. Without an entry there a thread installed from
// another machine shows under "recent" instead of under its project. This
// file is the app's private storage, so it is only touched while the app is
// closed, after a backup, and only to add the missing entry.

const globalStateFile = ".codex-global-state.json"

var appBinary = regexp.MustCompile(`(^|/)(ChatGPT|Codex)\.app/Contents/MacOS/[^/]+$`)

// AppRunning reports whether the Codex desktop app is running. Tests replace it.
var AppRunning = appRunning

func appRunning() bool {
	if runtime.GOOS != "darwin" {
		return true // unknown platform: never touch the state file
	}
	out, err := exec.Command("ps", "-axo", "comm=").Output()
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(out), "\n") {
		if appBinary.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// AssignOutcome is what happened to one thread.
type AssignOutcome string

const (
	Assigned        AssignOutcome = "assigned"
	AlreadyAssigned AssignOutcome = "already_assigned" // the app or the user already decided
	AppIsRunning    AssignOutcome = "app_running"
	NoProject       AssignOutcome = "no_project" // no project in the app covers the thread's directory
	NoStateFile     AssignOutcome = "no_state_file"
	NoThread        AssignOutcome = "no_thread" // rollout file or its directory not found
)

// AssignResult reports one thread; Project is the app's project name.
type AssignResult struct {
	ID      string
	Outcome AssignOutcome
	Project string
}

// ThreadCwd reads the working directory a thread was recorded with.
func ThreadCwd(home, id string) string {
	matches, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return ""
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 1<<20).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ""
	}
	var m struct {
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &m) != nil {
		return ""
	}
	return m.Payload.Cwd
}

type appProject struct {
	ID, Name string
	Roots    []string
}

func within(root, dir string) bool {
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}

// projectFor picks the app project whose folder is the deepest one containing dir.
func projectFor(projects []appProject, dir string) (appProject, bool) {
	best, bestLen := appProject{}, -1
	for _, p := range projects {
		for _, r := range p.Roots {
			if r != "" && within(r, dir) && len(r) > bestLen {
				best, bestLen = p, len(r)
			}
		}
	}
	return best, bestLen >= 0
}

// AssignProjects puts each thread under the app project that covers its
// directory. All threads share one read, one backup and one atomic write; when
// nothing needs changing the file is left untouched. backupDir receives a copy
// of the state file as it was before the write.
func AssignProjects(home, backupDir string, ids []string) ([]AssignResult, error) {
	path := filepath.Join(home, globalStateFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return results(ids, NoStateFile), nil
	}
	if err != nil {
		return nil, err
	}
	if AppRunning() {
		return results(ids, AppIsRunning), nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("codex state file is not valid JSON: %w", err)
	}
	var lp map[string]struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		RootPaths []string `json:"rootPaths"`
	}
	_ = json.Unmarshal(top["local-projects"], &lp)
	var projects []appProject
	for key, p := range lp {
		id := p.ID
		if id == "" {
			id = key
		}
		projects = append(projects, appProject{ID: id, Name: p.Name, Roots: p.RootPaths})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })

	assign := map[string]json.RawMessage{}
	_ = json.Unmarshal(top["thread-project-assignments"], &assign)
	var member map[string]json.RawMessage
	hasMember := false
	if raw, ok := top["thread-project-membership-host-ids"]; ok && json.Unmarshal(raw, &member) == nil && member != nil {
		hasMember = true
	}
	var projectless []string
	_ = json.Unmarshal(top["projectless-thread-ids"], &projectless)

	var out []AssignResult
	changed := false
	drop := map[string]bool{}
	for _, id := range ids {
		if _, ok := assign[id]; ok {
			out = append(out, AssignResult{ID: id, Outcome: AlreadyAssigned})
			continue
		}
		cwd := ThreadCwd(home, id)
		if cwd == "" {
			out = append(out, AssignResult{ID: id, Outcome: NoThread})
			continue
		}
		p, ok := projectFor(projects, cwd)
		if !ok {
			out = append(out, AssignResult{ID: id, Outcome: NoProject})
			continue
		}
		raw, _ := json.Marshal(map[string]string{"projectKind": "local", "projectId": p.ID})
		assign[id] = raw
		if hasMember {
			member[id] = json.RawMessage(`"local"`)
		}
		drop[id] = true
		changed = true
		out = append(out, AssignResult{ID: id, Outcome: Assigned, Project: p.Name})
	}
	if !changed {
		return out, nil
	}
	if err := backup(path, data, backupDir); err != nil {
		return nil, fmt.Errorf("backing up the Codex state file: %w", err)
	}
	if raw, err := json.Marshal(assign); err == nil {
		top["thread-project-assignments"] = raw
	}
	if hasMember {
		if raw, err := json.Marshal(member); err == nil {
			top["thread-project-membership-host-ids"] = raw
		}
	}
	if _, ok := top["projectless-thread-ids"]; ok {
		kept := projectless[:0:0]
		for _, id := range projectless {
			if !drop[id] {
				kept = append(kept, id)
			}
		}
		if raw, err := json.Marshal(kept); err == nil {
			top["projectless-thread-ids"] = raw
		}
	}
	var enc []byte
	if bytes.Contains(data, []byte("\n")) {
		enc, err = json.MarshalIndent(top, "", "  ")
	} else {
		enc, err = json.Marshal(top)
	}
	if err != nil {
		return nil, err
	}
	if AppRunning() { // the app may have been started while we prepared
		return results(ids, AppIsRunning), nil
	}
	mode := os.FileMode(0600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := platform.AtomicWrite(path, enc, mode); err != nil {
		return nil, err
	}
	return out, nil
}

func results(ids []string, o AssignOutcome) []AssignResult {
	out := make([]AssignResult, len(ids))
	for i, id := range ids {
		out[i] = AssignResult{ID: id, Outcome: o}
	}
	return out
}

// backup keeps the last few copies of the state file.
func backup(path string, data []byte, dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	name := fmt.Sprintf("codex-global-state.%s.json", time.Now().UTC().Format("20060102T150405Z"))
	if err := platform.AtomicWrite(filepath.Join(dir, name), data, 0600); err != nil {
		return err
	}
	old, _ := filepath.Glob(filepath.Join(dir, "codex-global-state.*.json"))
	sort.Strings(old)
	for len(old) > 10 {
		os.Remove(old[0])
		old = old[1:]
	}
	return nil
}
