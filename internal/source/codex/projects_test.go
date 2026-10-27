package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRollout(t *testing.T, home, id, cwd string) {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "07", "01")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": cwd}})
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-07-01T00-00-00-"+id+".jsonl"), append(line, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}

func setRunning(t *testing.T, v bool) {
	old := AppRunning
	AppRunning = func() bool { return v }
	t.Cleanup(func() { AppRunning = old })
}

const stateFixture = `{
  "unrelated": {"keep": [1, 2, 3], "big": 12345678901234567890},
  "local-projects": {
    "p-outer": {"id": "p-outer", "name": "Outer", "rootPaths": ["/work"]},
    "p-inner": {"id": "p-inner", "name": "Inner", "rootPaths": ["/work/inner"]}
  },
  "thread-project-assignments": {"old": {"projectKind": "local", "projectId": "p-outer"}},
  "thread-project-membership-host-ids": {"old": "local"},
  "projectless-thread-ids": ["t1", "other"]
}`

func setupState(t *testing.T, content string) (home, backups string) {
	home, backups = t.TempDir(), filepath.Join(t.TempDir(), "backups")
	if err := os.WriteFile(filepath.Join(home, globalStateFile), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return
}

func TestAssignProjectsFilesThreadUnderDeepestProject(t *testing.T) {
	setRunning(t, false)
	home, backups := setupState(t, stateFixture)
	writeRollout(t, home, "t1", "/work/inner/sub")
	writeRollout(t, home, "t2", "/elsewhere")
	writeRollout(t, home, "old", "/work")

	res, err := AssignProjects(home, backups, []string{"t1", "t2", "old", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]AssignOutcome{"t1": Assigned, "t2": NoProject, "old": AlreadyAssigned, "missing": NoThread}
	for _, r := range res {
		if r.Outcome != want[r.ID] {
			t.Errorf("%s: got %s want %s", r.ID, r.Outcome, want[r.ID])
		}
		if r.ID == "t1" && r.Project != "Inner" {
			t.Errorf("project = %q", r.Project)
		}
	}
	data, _ := os.ReadFile(filepath.Join(home, globalStateFile))
	var d map[string]json.RawMessage
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d["unrelated"]), "12345678901234567890") {
		t.Errorf("unrelated key altered: %s", d["unrelated"])
	}
	var as map[string]map[string]string
	json.Unmarshal(d["thread-project-assignments"], &as)
	if as["t1"]["projectId"] != "p-inner" || as["t1"]["projectKind"] != "local" || as["old"]["projectId"] != "p-outer" {
		t.Errorf("assignments = %v", as)
	}
	if _, ok := as["t2"]; ok {
		t.Error("unplaced thread was written")
	}
	var mem map[string]string
	json.Unmarshal(d["thread-project-membership-host-ids"], &mem)
	if mem["t1"] != "local" {
		t.Errorf("membership = %v", mem)
	}
	var pl []string
	json.Unmarshal(d["projectless-thread-ids"], &pl)
	if len(pl) != 1 || pl[0] != "other" {
		t.Errorf("projectless = %v", pl)
	}
	bk, _ := filepath.Glob(filepath.Join(backups, "codex-global-state.*.json"))
	if len(bk) != 1 {
		t.Fatalf("backups = %v", bk)
	}
	if b, _ := os.ReadFile(bk[0]); string(b) != stateFixture {
		t.Error("backup differs from the original")
	}
}

func TestAssignProjectsLeavesFileAloneWhenAppRunningOrNothingToDo(t *testing.T) {
	home, backups := setupState(t, stateFixture)
	writeRollout(t, home, "t1", "/work")

	setRunning(t, true)
	res, err := AssignProjects(home, backups, []string{"t1"})
	if err != nil || res[0].Outcome != AppIsRunning {
		t.Fatalf("running: %v %v", res, err)
	}
	setRunning(t, false)
	writeRollout(t, home, "t3", "/nowhere")
	if _, err := AssignProjects(home, backups, []string{"t3"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, globalStateFile)); string(b) != stateFixture {
		t.Error("state file changed although nothing was assigned")
	}
	if bk, _ := filepath.Glob(filepath.Join(backups, "*")); len(bk) != 0 {
		t.Errorf("backup made without a write: %v", bk)
	}
}

func TestAssignProjectsSkipsMembershipWhenAppDoesNotKeepIt(t *testing.T) {
	setRunning(t, false)
	home, backups := setupState(t, `{"local-projects":{"p":{"id":"p","name":"P","rootPaths":["/w"]}}}`)
	writeRollout(t, home, "t1", "/w")
	if res, err := AssignProjects(home, backups, []string{"t1"}); err != nil || res[0].Outcome != Assigned {
		t.Fatalf("%v %v", res, err)
	}
	data, _ := os.ReadFile(filepath.Join(home, globalStateFile))
	if strings.Contains(string(data), "membership") || strings.Contains(string(data), "projectless") {
		t.Errorf("created keys the app did not have: %s", data)
	}
	if res, _ := AssignProjects(t.TempDir(), backups, []string{"t1"}); res[0].Outcome != NoStateFile {
		t.Errorf("missing state file: %v", res)
	}
}
