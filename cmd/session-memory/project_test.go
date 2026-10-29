package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
)

func TestProjectTrackAndRestoreAcrossMachines(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", shared)
	acodex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)

	projA := filepath.Join(A.home, "code", "app")
	other := filepath.Join(A.home, "code", "other")
	os.MkdirAll(projA, 0700)
	os.MkdirAll(filepath.Join(projA, "sub"), 0700)
	os.MkdirAll(other, 0700)
	add := func(seed int64, cwd string) string {
		tid := codexgen.ThreadIDFor(seed)
		p := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: seed, Turns: 2, Cwd: cwd}), 0600)
		return tid
	}
	t1, t2, tOther := add(11, projA), add(12, filepath.Join(projA, "sub")), add(13, other)

	var pt struct {
		ProjectRef string `json:"project_ref"`
		Tracked    int    `json:"tracked"`
		Already    int    `json:"already_tracked"`
	}
	if err := json.Unmarshal([]byte(A.must("project", "track", "--cwd", projA, "--json")), &pt); err != nil || pt.Tracked != 2 {
		t.Fatalf("project track: %v %+v", err, pt)
	}
	// running it again finds nothing new and reuses the same project
	var again struct {
		ProjectRef string `json:"project_ref"`
		Tracked    int    `json:"tracked"`
		Already    int    `json:"already_tracked"`
	}
	json.Unmarshal([]byte(A.must("project", "track", "--cwd", projA, "--json")), &again)
	if again.ProjectRef != pt.ProjectRef || again.Tracked != 0 || again.Already != 2 {
		t.Fatalf("second project track: %+v", again)
	}
	// changing the label must not switch off following the project
	A.must("project", "map", pt.ProjectRef, "--path", projA, "--label", "renamed")
	if raw, _ := os.ReadFile(filepath.Join(A.home, "sm", "projects", pt.ProjectRef+".json")); !strings.Contains(string(raw), `"codex"`) || !strings.Contains(string(raw), "renamed") {
		t.Fatalf("project map lost the watch list or ignored the label: %s", raw)
	}
	// a session created later under the project joins on the next sync; one elsewhere does not
	t3 := add(14, projA)
	var sy map[string]int
	json.Unmarshal([]byte(A.must("sync", "--json")), &sy)
	if sy["auto_tracked"] != 1 {
		t.Fatalf("sync auto-track: %+v", sy)
	}
	if out := A.must("list"); strings.Contains(out, tOther) || strings.Count(strings.TrimSpace(out), "\n") != 2 {
		t.Fatalf("list should hold exactly the 3 project sessions:\n%s", out)
	}

	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B.must("config", "set", "transport.folder", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	bcodex := filepath.Join(B.home, "codex")
	B.must("config", "set", "agents.codex.home", bcodex)
	os.MkdirAll(bcodex, 0700)
	os.WriteFile(filepath.Join(bcodex, "config.toml"), []byte("model_provider = \"custom\"\n"), 0600)
	B.must("sync")
	if out := B.must("project", "list"); !strings.Contains(out, pt.ProjectRef) || !strings.Contains(out, "sessions=3") {
		t.Fatalf("B project list: %s", out)
	}

	projB := filepath.Join(B.home, "work", "app")
	if code, _, errs := B.run("project", "restore", pt.ProjectRef, "--client-closed"); code == 0 || !strings.Contains(errs, "no folder named app") {
		t.Fatalf("restore of an unmapped project whose folder does not exist here must be refused with the reason: %d %s", code, errs)
	}
	// two candidates are not guessed between
	os.MkdirAll(filepath.Join(B.home, "Documents", "app"), 0700)
	os.MkdirAll(projB, 0700)
	if code, _, errs := B.run("project", "restore", pt.ProjectRef, "--client-closed"); code == 0 || !strings.Contains(errs, "2 folders named app") {
		t.Fatalf("ambiguous folders must be refused: %d %s", code, errs)
	}
	os.RemoveAll(filepath.Join(B.home, "Documents"))
	// exactly one folder with the project's name: found without --cwd
	if out := B.must("project", "restore", pt.ProjectRef, "--client-closed", "--allow-code-mismatch", "--json"); !json.Valid([]byte(out)) {
		t.Fatalf("--json output is not one JSON document:\n%s", out)
	}
	if out := B.must("project", "list"); !strings.Contains(out, projB) {
		t.Fatalf("project not mapped to the matching folder: %s", out)
	}
	files, _ := filepath.Glob(filepath.Join(bcodex, "sessions", "*", "*", "*", "*.jsonl"))
	if len(files) != 3 {
		t.Fatalf("B restored %d sessions, want 3: %v", len(files), files)
	}
	disc := B.must("discover")
	for _, id := range []string{t1, t2, t3} {
		if !strings.Contains(disc, id) {
			t.Errorf("session %s not on B", id)
		}
	}
	if strings.Contains(disc, tOther) {
		t.Error("session outside the project reached B")
	}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		if strings.Contains(string(raw), `"cwd":"`+projA) {
			t.Errorf("%s still points at A's directory", f)
		}
		if !strings.Contains(string(raw), `"model_provider":"custom"`) || strings.Contains(string(raw), `"model_provider":"openai"`) {
			t.Errorf("%s does not use B's provider", f)
		}
	}
	// idempotent: everything is up to date the second time
	if out := B.must("project", "restore", pt.ProjectRef, "--client-closed", "--allow-code-mismatch"); !strings.Contains(out, "up_to_date=3") {
		t.Fatalf("second restore: %s", out)
	}
	// removing an installed rollout (e.g. to redo it with a newer tool) lets restore install it again
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if out := B.must("project", "restore", pt.ProjectRef, "--client-closed", "--allow-code-mismatch"); !strings.Contains(out, "installed=1") || !strings.Contains(out, "up_to_date=2") {
		t.Fatalf("reinstall after removal: %s", out)
	}
	if again, _ := filepath.Glob(filepath.Join(bcodex, "sessions", "*", "*", "*", "*.jsonl")); len(again) != 3 {
		t.Fatalf("after reinstall B has %d rollouts", len(again))
	}
}

func TestRelayHandsWorkOverAndBack(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", shared)
	acodex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)
	projA := filepath.Join(A.home, "code", "app")
	os.MkdirAll(projA, 0700)
	tid := codexgen.ThreadIDFor(21)
	ap := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(ap), 0700)
	os.WriteFile(ap, codexgen.Rollout(codexgen.Options{Seed: 21, Turns: 2, Cwd: projA}), 0600)
	var pt struct {
		ProjectRef string `json:"project_ref"`
	}
	json.Unmarshal([]byte(A.must("project", "track", "--cwd", projA, "--json")), &pt)

	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B.must("config", "set", "transport.folder", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	bcodex := filepath.Join(B.home, "codex")
	B.must("config", "set", "agents.codex.home", bcodex)
	// with the WorkBuddy writer switched on relay also looks after WorkBuddy; the Codex hand-over must not change
	A.must("config", "set", "agents.workbuddy.allow_registry_write", "true")
	B.must("config", "set", "agents.workbuddy.allow_registry_write", "true")
	B.must("config", "set", "agents.workbuddy.home", filepath.Join(B.home, "wb"))
	A.must("config", "set", "agents.workbuddy.home", filepath.Join(A.home, "wb"))
	os.MkdirAll(bcodex, 0700)

	// B arrives: the project has no folder yet, relay says it is added by itself once one exists
	out := B.must("relay", "--client-closed")
	if !strings.Contains(out, "have no folder here yet (app)") || strings.Contains(out, "<") {
		t.Fatalf("first relay on B: %s", out)
	}
	projB := filepath.Join(B.home, "work", "app")
	os.MkdirAll(projB, 0700)
	// a plan only looks: nothing is mapped or installed, and it says "would"
	plan := B.must("project", "restore", pt.ProjectRef, "--cwd", projB, "--client-closed", "--allow-code-mismatch", "--plan")
	if strings.Contains(plan, "installed ") || !strings.Contains(plan, "would install") {
		t.Fatalf("plan output: %s", plan)
	}
	if out := B.must("project", "list"); strings.Contains(out, projB) {
		t.Fatalf("a plan must not map the project:\n%s", out)
	}
	if got, _ := filepath.Glob(filepath.Join(bcodex, "sessions", "*", "*", "*", "*.jsonl")); len(got) != 0 {
		t.Fatalf("a plan installed %v", got)
	}
	B.must("project", "restore", pt.ProjectRef, "--cwd", projB, "--client-closed", "--allow-code-mismatch")
	files, _ := filepath.Glob(filepath.Join(bcodex, "sessions", "*", "*", "*", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("B has %v", files)
	}

	// work on B, then hand back
	f, _ := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"WORK_DONE_ON_B"}}` + "\n")
	f.Close()
	if out := B.must("relay", "--client-closed"); !strings.Contains(out, "up to date 1") {
		t.Fatalf("B relay after work: %s", out)
	}
	out = A.must("relay", "--client-closed")
	if !strings.Contains(out, "received 1 version") || !strings.Contains(out, "updated 1") {
		t.Fatalf("A relay: %s", out)
	}
	raw, _ := os.ReadFile(ap)
	if !strings.Contains(string(raw), "WORK_DONE_ON_B") {
		t.Fatal("B's work did not arrive in A's Codex session")
	}
	// nothing left to do on either side
	A.must("relay", "--client-closed") // publishes the install receipt
	if out := A.must("relay", "--client-closed"); !strings.Contains(out, "uploaded 0 item(s), received 0 version(s)") || !strings.Contains(out, "up to date 1") {
		t.Fatalf("idle relay: %s", out)
	}
	// both sides continue: the second hand-over is also a clean fast-forward
	g, _ := os.OpenFile(ap, os.O_APPEND|os.O_WRONLY, 0600)
	g.WriteString(`{"timestamp":"2026-09-01T03:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"BACK_ON_A"}}` + "\n")
	g.Close()
	A.must("relay", "--client-closed")
	B.must("relay", "--client-closed")
	if raw, _ := os.ReadFile(files[0]); !strings.Contains(string(raw), "BACK_ON_A") {
		t.Fatal("A's follow-up did not reach B")
	}
}

func TestRelayConflictChooseThenLoserTakesChosen(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", shared)
	acodex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)
	projA := filepath.Join(A.home, "code", "app")
	os.MkdirAll(projA, 0700)
	tid := codexgen.ThreadIDFor(31)
	ap := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(ap), 0700)
	os.WriteFile(ap, codexgen.Rollout(codexgen.Options{Seed: 31, Turns: 2, Cwd: projA}), 0600)
	var pt struct {
		ProjectRef string `json:"project_ref"`
	}
	json.Unmarshal([]byte(A.must("project", "track", "--cwd", projA, "--json")), &pt)
	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B.must("config", "set", "transport.folder", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	bcodex := filepath.Join(B.home, "codex")
	B.must("config", "set", "agents.codex.home", bcodex)
	os.MkdirAll(bcodex, 0700)
	projB := filepath.Join(B.home, "work", "app")
	os.MkdirAll(projB, 0700)
	B.must("project", "restore", pt.ProjectRef, "--cwd", projB, "--client-closed", "--allow-code-mismatch")
	bfiles, _ := filepath.Glob(filepath.Join(bcodex, "sessions", "*", "*", "*", "*.jsonl"))

	// both continue from the same version
	appendLine := func(p, msg string) {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
		f.WriteString(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"` + msg + `"}}` + "\n")
		f.Close()
	}
	appendLine(ap, "FROM_A")
	appendLine(bfiles[0], "FROM_B")
	B.must("relay", "--client-closed")
	if code, _, _ := A.run("relay", "--client-closed"); code != 6 {
		t.Fatalf("A relay with diverged branches must need attention (exit 6), got %d", code)
	}
	var list []struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal([]byte(A.must("list", "--json")), &list)
	var g struct {
		Revisions []struct {
			RevisionID string `json:"revision_id"`
			Parent     string `json:"parent_revision_id"`
			Producer   string `json:"producer_device_id"`
		} `json:"revisions"`
		Heads []string `json:"heads"`
	}
	json.Unmarshal([]byte(A.must("revisions", list[0].SessionID, "--json")), &g)
	aDev := ""
	for _, r := range g.Revisions {
		if r.Parent == "" {
			aDev = r.Producer
		}
	}
	aHead := ""
	for _, r := range g.Revisions {
		for _, h := range g.Heads {
			if r.RevisionID == h && r.Producer == aDev {
				aHead = h
			}
		}
	}
	if len(g.Heads) != 2 || aHead == "" {
		t.Fatalf("expected two heads, one from A: %+v", g)
	}
	A.must("choose", list[0].SessionID, "--revision", aHead)
	if out := A.must("relay", "--client-closed"); !strings.Contains(out, "up to date 1") {
		t.Fatalf("A after choosing its own branch: %s", out)
	}
	// B still has the abandoned branch installed: it is told, not overwritten
	if code, _, errs := B.run("relay", "--client-closed"); code != 6 || !strings.Contains(errs, "session-memory adopt") {
		t.Fatalf("B relay: exit %d %s", code, errs)
	}
	if raw, _ := os.ReadFile(bfiles[0]); !strings.Contains(string(raw), "FROM_B") {
		t.Fatal("B's own work must stay untouched until the user moves it")
	}
	// a preview changes nothing
	before, _ := os.ReadFile(bfiles[0])
	if out := B.must("adopt", list[0].SessionID, "--client-closed", "--plan"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("adopt --plan output: %s", out)
	}
	if after, err := os.ReadFile(bfiles[0]); err != nil || string(after) != string(before) {
		t.Fatalf("adopt --plan touched B's copy: %v", err)
	}
	if moved, _ := filepath.Glob(filepath.Join(B.home, "sm", "abandoned", "*", "*")); len(moved) != 0 {
		t.Fatalf("adopt --plan made a backup: %v", moved)
	}
	// one command takes the chosen version; B's own copy is kept in a backup folder
	out := B.must("adopt", list[0].SessionID, "--client-closed")
	if !strings.Contains(out, "your previous copy is in") {
		t.Fatalf("adopt output: %s", out)
	}
	backups, _ := filepath.Glob(filepath.Join(B.home, "sm", "abandoned", list[0].SessionID, "*", "sessions", "*", "*", "*", "*.jsonl"))
	if len(backups) != 1 {
		t.Fatalf("B's own copy was not backed up: %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); !strings.Contains(string(b), "FROM_B") {
		t.Fatal("the backup does not hold B's own work")
	}
	if out := B.must("relay", "--client-closed"); !strings.Contains(out, "up to date 1") {
		t.Fatalf("B relay after adopt: %s", out)
	}
	raw, _ := os.ReadFile(bfiles[0])
	if !strings.Contains(string(raw), "FROM_A") || strings.Contains(string(raw), "FROM_B") {
		t.Fatal("B did not end up on A's branch")
	}
	if out := B.must("relay", "--client-closed"); !strings.Contains(out, "up to date 1") {
		t.Fatalf("B after taking it: %s", out)
	}
	// whatever B published after taking the version must not trip A up, and
	// A must go on from the newest version instead of forking from an old one
	A.must("relay", "--client-closed")
	appendLine(ap, "A_AGAIN")
	A.must("relay", "--client-closed")
	B.must("relay", "--client-closed")
	if raw, _ := os.ReadFile(bfiles[0]); !strings.Contains(string(raw), "A_AGAIN") {
		t.Fatal("A's later work did not reach B")
	}
}

// A session that cannot be read must not make a sync look clean.
func TestSyncReportsSessionsItCouldNotCapture(t *testing.T) {
	root := t.TempDir()
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", filepath.Join(root, "cloud"))
	acodex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)
	tid := codexgen.ThreadIDFor(31)
	p := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 31, Turns: 2}), 0600)
	A.must("track", "--native-session", tid)
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}
	os.WriteFile(p, append(codexgen.Rollout(codexgen.Options{Seed: 31, Turns: 3}), '\n'), 0600)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0600)
	code, _, errs := A.run("sync")
	if code == 0 || !strings.Contains(errs, "could not capture") {
		t.Fatalf("sync of an unreadable session: exit %d, stderr %q", code, errs)
	}
}

// The chosen version builds on the installed one, yet this computer's thread
// has changed underneath (the agent rewrote a line in place): relay refuses to
// overwrite it, and adopt, which used to say "run relay", takes the chosen
// version and keeps the copy.
func TestAdoptReplacesACopyThatDivergedFromItsInstalledVersion(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", shared)
	acodex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)
	projA := filepath.Join(A.home, "code", "app")
	os.MkdirAll(projA, 0700)
	tid := codexgen.ThreadIDFor(32)
	ap := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(ap), 0700)
	os.WriteFile(ap, codexgen.Rollout(codexgen.Options{Seed: 32, Turns: 2, Cwd: projA}), 0600)
	var pt struct {
		ProjectRef string `json:"project_ref"`
	}
	json.Unmarshal([]byte(A.must("project", "track", "--cwd", projA, "--json")), &pt)
	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B.must("config", "set", "transport.folder", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	bcodex := filepath.Join(B.home, "codex")
	B.must("config", "set", "agents.codex.home", bcodex)
	os.MkdirAll(bcodex, 0700)
	projB := filepath.Join(B.home, "work", "app")
	os.MkdirAll(projB, 0700)
	B.must("project", "restore", pt.ProjectRef, "--cwd", projB, "--client-closed", "--allow-code-mismatch")
	bfiles, _ := filepath.Glob(filepath.Join(bcodex, "sessions", "*", "*", "*", "*.jsonl"))

	// A moves on; B's file is changed in place, same size and time, so nothing
	// notices before the next version arrives
	f, _ := os.OpenFile(ap, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"FROM_A"}}` + "\n")
	f.Close()
	A.must("relay", "--client-closed")
	st, _ := os.Stat(bfiles[0])
	raw, _ := os.ReadFile(bfiles[0])
	i := strings.Index(string(raw), "\"type\"")
	raw[i+2] ^= 0x01 // "type" -> "uype": same length, no longer the installed content
	os.WriteFile(bfiles[0], raw, 0600)
	os.Chtimes(bfiles[0], st.ModTime(), st.ModTime())

	var list []struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal([]byte(B.must("list", "--json")), &list)
	if out := B.must("adopt", list[0].SessionID, "--client-closed", "--plan"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("adopt --plan: %s", out)
	}
	// B's unseen change is first kept as its own version, so the chosen one is named again
	if code, _, errs := B.run("adopt", list[0].SessionID, "--client-closed"); code != 3 || !strings.Contains(errs, "run choose again") {
		t.Fatalf("first adopt: exit %d %s", code, errs)
	}
	var g struct {
		Revisions []struct {
			RevisionID string `json:"revision_id"`
			Parent     string `json:"parent_revision_id"`
			Producer   string `json:"producer_device_id"`
		} `json:"revisions"`
		Heads []string `json:"heads"`
	}
	json.Unmarshal([]byte(B.must("revisions", list[0].SessionID, "--json")), &g)
	aDev, aHead := "", ""
	for _, r := range g.Revisions {
		if r.Parent == "" {
			aDev = r.Producer
		}
	}
	for _, r := range g.Revisions {
		for _, h := range g.Heads {
			if r.RevisionID == h && r.Producer == aDev {
				aHead = h
			}
		}
	}
	if aHead == "" {
		t.Fatalf("no head from A: %+v", g)
	}
	B.must("choose", list[0].SessionID, "--revision", aHead)
	if out := B.must("adopt", list[0].SessionID, "--client-closed"); !strings.Contains(out, "your previous copy is in") {
		t.Fatalf("adopt: %s", out)
	}
	if out := B.must("relay", "--client-closed"); !strings.Contains(out, "up to date 1") {
		t.Fatalf("B relay after adopt: %s", out)
	}
	if raw, _ := os.ReadFile(bfiles[0]); !strings.Contains(string(raw), "FROM_A") || strings.Contains(string(raw), "uype") {
		t.Fatal("B did not end up on the chosen version")
	}
}

// taskProjectFixture tracks a WorkBuddy task project on machine a and syncs it
// to machine b, which has no task folder yet. It returns b, the project ref and
// the folder a restore would make on b.
func taskProjectFixture(t *testing.T) (b *machine, ref, want string) {
	t.Helper()
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	a := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	b = &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(a.home, 0700)
	os.MkdirAll(b.home, 0700)
	a.must("init")
	a.must("transport", "attach-folder", shared)
	name := "2026-10-29-01-02-03"
	task := filepath.Join(a.home, "WorkBuddy", name)
	os.MkdirAll(task, 0700)
	wb := filepath.Join(a.home, "wb")
	a.must("config", "set", "agents.workbuddy.home", wb)
	id := "0192f3a4-0000-7000-8000-000000000042"
	dir := filepath.Join(wb, "projects", strings.Trim(strings.ReplaceAll(task, "/", "-"), "-"))
	os.MkdirAll(dir, 0700)
	line, _ := json.Marshal(map[string]any{"type": "message", "id": "u1", "sessionId": id, "timestamp": int64(1793000000000), "role": "user", "cwd": task, "content": "synthetic task"})
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), append(line, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	var track struct {
		Ref string `json:"project_ref"`
	}
	if err := json.Unmarshal([]byte(a.must("project", "track", "--agent", "workbuddy", "--cwd", task, "--json")), &track); err != nil || track.Ref == "" {
		t.Fatalf("track %v", err)
	}
	a.must("sync")
	b.must("config", "set", "transport.folder", shared)
	b.must("device", "join", "--from-cloud")
	b.must("sync")
	want = filepath.Join(b.home, "WorkBuddy", name)
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("setup: target task folder already exists: %v", err)
	}
	return b, track.Ref, want
}

// A preview makes nothing on disk; a real restore of a task project makes the
// task folder the agent itself would have made, whether or not the install that
// follows succeeds.
func TestProjectRestoreTaskFolderOnlyOnRealRun(t *testing.T) {
	b, ref, want := taskProjectFixture(t)
	os.MkdirAll(filepath.Join(b.home, "wb"), 0700)
	b.must("config", "set", "agents.workbuddy.home", filepath.Join(b.home, "wb"))
	b.must("config", "set", "agents.workbuddy.allow_registry_write", "true")
	// The fixture has no WorkBuddy registry on b, so the install itself stops at
	// "create a local session" (in the plan and in the real run alike); what is
	// checked here is the folder the command makes before that point.
	b.run("project", "restore", ref, "--agent", "workbuddy", "--plan")
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("project restore --plan created task folder before install checks: %v", err)
	}
	b.run("project", "restore", ref, "--agent", "workbuddy")
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("a real restore should create the task folder: %v", err)
	}
}
