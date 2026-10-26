package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"session-memory/internal/agent"
	"session-memory/internal/jsonl"
)

// Probe inspects every Codex home read-only. The result holds structure only:
// existence flags, counts, record type names. No titles, cwd, ids or text.
func (a *Adapter) Probe(ctx context.Context) (agent.ProbeResult, error) {
	res := agent.ProbeResult{SchemaVersion: 1, Agent: AgentName, Counts: map[string]int{}}
	parsed, unparsed := 0, 0
	for _, home := range a.Cfg.CodexHomes() {
		hp := agent.HomeProbe{Stores: map[string]bool{}, FileForms: map[string]int{}, RecordTypes: map[string]int{}}
		if st, err := os.Stat(home); err == nil && st.IsDir() {
			hp.Exists = true
		}
		for name, rel := range map[string]string{
			"sessions_dir": "sessions", "archived_sessions_dir": "archived_sessions", "state_db": "state_5.sqlite",
			"history_db": "thread_history_1.sqlite", "session_index": "session_index.jsonl",
			"writer_locks_dir": "thread-writer-locks", "global_state": ".codex-global-state.json",
		} {
			_, err := os.Stat(filepath.Join(home, rel))
			hp.Stores[name] = err == nil
		}
		caps := map[string]bool{}
		if hp.Stores["writer_locks_dir"] {
			caps["dir:thread-writer-locks"] = true
		}
		threads, _ := threadFiles(home)
		type newest struct {
			f     file
			mtime string
		}
		var all []newest
		for _, fs := range threads {
			hasPlain, hasZst := false, false
			for _, f := range fs {
				if f.zst {
					hp.FileForms["jsonl.zst"]++
					hasZst = true
					caps["form:zst"] = true
				} else {
					hp.FileForms["jsonl"]++
					hasPlain = true
				}
				all = append(all, newest{f, f.mtime})
			}
			if hasPlain && hasZst {
				hp.FileForms["twin"]++
			}
		}
		res.Counts["threads"] += len(threads)
		sort.Slice(all, func(i, j int) bool { return all[i].mtime > all[j].mtime })
		for i, n := range all {
			if i >= 20 {
				break
			}
			if m, ok := readMeta(home, n.f); ok {
				parsed++
				if hp.ClientVer == "" {
					hp.ClientVer = m.CLIVersion
				}
			} else {
				unparsed++
			}
			histogram(home, n.f, hp.RecordTypes, caps)
		}
		for c := range caps {
			hp.Capabilities = append(hp.Capabilities, c)
		}
		sort.Strings(hp.Capabilities)
		res.Homes = append(res.Homes, hp)
	}
	switch {
	case parsed == 0 && unparsed == 0:
		res.Compatibility = "ok"
		res.Notes = []string{"no rollout files found"}
		anyHome := false
		for _, h := range res.Homes {
			anyHome = anyHome || h.Stores["sessions_dir"]
		}
		if !anyHome {
			res.Compatibility = "unsupported"
			res.Notes = []string{"no sessions directory"}
		}
	case parsed == 0:
		res.Compatibility = "unsupported"
		res.Notes = []string{"no parseable session_meta"}
	case unparsed > 0:
		res.Compatibility = "partial"
		res.Notes = []string{"some rollouts have unreadable session_meta"}
	default:
		res.Compatibility = "ok"
	}
	res.Counts["parsed_meta"], res.Counts["unparsed_meta"] = parsed, unparsed
	return res, nil
}

// histogram counts record type names (never contents) in the first 200 records.
func histogram(home string, f file, types map[string]int, caps map[string]bool) {
	src, err := jsonl.OpenSource(filepath.Join(home, filepath.FromSlash(f.rel)))
	if err != nil {
		return
	}
	rc, _, err := src.OpenAt(0)
	if err != nil {
		return
	}
	r := jsonl.NewReader(rc, 0, jsonl.Options{MaxRecordBytes: 4 << 20})
	defer r.Close()
	for i := 0; i < 200; i++ {
		rec, err := r.Next()
		if err != nil {
			return
		}
		var e struct {
			Type    string `json:"type"`
			Ordinal *int   `json:"ordinal"`
			Payload struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if json.Unmarshal(rec.Line, &e) != nil || e.Type == "" {
			continue
		}
		name := e.Type
		if e.Payload.Type != "" {
			name += "/" + e.Payload.Type
		}
		types[name]++
		if e.Ordinal != nil {
			caps["has:ordinal"] = true
		}
		switch name {
		case "event_msg/item_completed":
			caps["record:item_completed"] = true
		case "compacted":
			caps["record:compacted"] = true
		}
	}
}
