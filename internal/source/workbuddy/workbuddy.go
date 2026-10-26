// Package workbuddy is the WorkBuddy / CodeBuddy Code adapter. The read side
// (discover, probe, capture, locate) follows spec §8.4; the writer stays
// unsupported until experiments E3/E4 decide what WorkBuddy needs.
package workbuddy

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/errcode"
	"session-memory/internal/jsonl"
	"session-memory/internal/session"
	"session-memory/internal/streamcap"
)

// AgentName identifies this adapter.
const AgentName = "workbuddy"

// AdapterVersion is recorded in profiles.
const AdapterVersion = "0.1.0"

// Adapter implements agent.Adapter.
type Adapter struct{ Cfg config.Config }

// New returns the adapter for cfg.
func New(cfg config.Config) *Adapter { return &Adapter{Cfg: cfg} }

func (a *Adapter) Name() string { return AgentName }

type artifact struct {
	rel  string
	kind string // session.KindStream | session.KindBlob
	mode os.FileMode
}

type sessFiles struct {
	id    string
	main  string // rel of <id>.jsonl, "" when only side files exist
	files []artifact
}

var scene = regexp.MustCompile(`^@[\w-]+:\S*$`)

// scan lists every session under home/projects.
func scan(home string) map[string]*sessFiles {
	out := map[string]*sessFiles{}
	root := filepath.Join(home, "projects")
	get := func(id string) *sessFiles {
		s := out[id]
		if s == nil {
			s = &sessFiles{id: id}
			out[id] = s
		}
		return s
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(home, p)
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		parts := strings.Split(rel, "/") // projects/<dir>/...
		name := d.Name()
		switch {
		case len(parts) == 3 && strings.HasSuffix(name, ".jsonl"):
			id := strings.TrimSuffix(name, ".jsonl")
			s := get(id)
			s.main = rel
			s.files = append(s.files, artifact{rel, session.KindStream, info.Mode().Perm()})
		case len(parts) == 3 && strings.HasSuffix(name, ".meta.json"):
			s := get(strings.TrimSuffix(name, ".meta.json"))
			s.files = append(s.files, artifact{rel, session.KindBlob, info.Mode().Perm()})
		case len(parts) == 3 && strings.HasSuffix(name, ".file-rollback.ndjson"):
			s := get(strings.TrimSuffix(name, ".file-rollback.ndjson"))
			s.files = append(s.files, artifact{rel, session.KindStream, info.Mode().Perm()})
		case len(parts) >= 5 && parts[3] == "subagents" && strings.HasSuffix(name, ".jsonl"):
			s := get(parts[2])
			s.files = append(s.files, artifact{rel, session.KindStream, info.Mode().Perm()})
		case len(parts) >= 5 && parts[3] == "tool-results":
			s := get(parts[2])
			s.files = append(s.files, artifact{rel, session.KindBlob, info.Mode().Perm()})
		}
		return nil
	})
	for _, s := range out {
		sort.Slice(s.files, func(i, j int) bool { return s.files[i].rel < s.files[j].rel })
	}
	return out
}

// softDeleted returns session ids the app marked deleted in workbuddy.db.
func softDeleted(home string) (map[string]bool, bool) {
	p := filepath.Join(home, "workbuddy.db")
	if _, err := os.Stat(p); err != nil {
		return nil, false
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(p), RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, false
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id FROM sessions WHERE deleted_at IS NOT NULL`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out[id] = true
	}
	return out, true
}

type headInfo struct {
	cwd, title string
}

// readHead scans the first records of the main file for cwd and titles.
func readHead(home string, s *sessFiles) headInfo {
	var h headInfo
	if s.main != "" {
		if src, err := jsonl.OpenSource(filepath.Join(home, filepath.FromSlash(s.main))); err == nil {
			if rc, _, err := src.OpenAt(0); err == nil {
				r := jsonl.NewReader(rc, 0, jsonl.Options{MaxRecordBytes: 4 << 20})
				var ai, custom string
				for i := 0; i < 400; i++ {
					rec, err := r.Next()
					if err != nil {
						break
					}
					var e struct {
						Type        string `json:"type"`
						Cwd         string `json:"cwd"`
						AITitle     string `json:"aiTitle"`
						CustomTitle string `json:"customTitle"`
					}
					if json.Unmarshal(rec.Line, &e) != nil {
						continue
					}
					if h.cwd == "" {
						h.cwd = e.Cwd
					}
					if e.Type == "ai-title" && e.AITitle != "" {
						ai = e.AITitle
					}
					if e.Type == "custom-title" && e.CustomTitle != "" {
						custom = e.CustomTitle
					}
				}
				r.Close()
				h.title = custom
				if h.title == "" {
					h.title = ai
				}
			}
		}
	}
	if h.cwd == "" {
		for _, f := range s.files {
			if strings.HasSuffix(f.rel, ".meta.json") {
				if b, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(f.rel))); err == nil {
					var m struct {
						Cwd string `json:"cwd"`
					}
					if json.Unmarshal(b, &m) == nil {
						h.cwd = m.Cwd
					}
				}
			}
		}
	}
	return h
}

func (a *Adapter) homes(o agent.DiscoverOptions) []string {
	if len(o.Homes) > 0 {
		return o.Homes
	}
	return a.Cfg.WorkBuddyHomes()
}

func profile() session.AgentProfile {
	return session.AgentProfile{Agent: AgentName, Surface: "desktop", Format: "workbuddy-jsonl", AdapterVersion: AdapterVersion, OS: runtime.GOOS}
}

func nativeSession(home string, s *sessFiles) agent.NativeSession {
	h := readHead(home, s)
	ns := agent.NativeSession{Ref: agent.NativeRef{Home: home, NativeID: s.id}, Profile: profile(), Cwd: h.cwd, Title: h.title}
	for _, f := range s.files {
		st, err := os.Stat(filepath.Join(home, filepath.FromSlash(f.rel)))
		if err != nil {
			continue
		}
		mt := session.FormatTime(st.ModTime())
		ns.Files = append(ns.Files, agent.FileStat{Path: f.rel, Size: st.Size(), ModTime: mt})
		if mt > ns.UpdatedAt {
			ns.UpdatedAt = mt
		}
	}
	return ns
}

// Discover lists sessions in every home, skipping ones the app soft-deleted.
func (a *Adapter) Discover(ctx context.Context, o agent.DiscoverOptions) ([]agent.NativeSession, error) {
	want := map[string]bool{}
	for _, id := range o.NativeIDs {
		want[id] = true
	}
	var out []agent.NativeSession
	for _, home := range a.homes(o) {
		deleted, _ := softDeleted(home)
		for id, s := range scan(home) {
			if s.main == "" || deleted[id] || (len(want) > 0 && !want[id]) {
				continue
			}
			out = append(out, nativeSession(home, s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

// Locate finds a session by id.
func (a *Adapter) Locate(ctx context.Context, ref agent.NativeRef) (agent.NativeSession, bool, error) {
	s := scan(ref.Home)[ref.NativeID]
	if s == nil || s.main == "" {
		return agent.NativeSession{}, false, nil
	}
	return nativeSession(ref.Home, s), true, nil
}

func (a *Adapter) maxRecord() int64 {
	if n := a.Cfg.Agents.WorkBuddy.MaxRecordBytes; n > 0 {
		return n
	}
	return jsonl.DefaultMaxRecordBytes
}

// Capture streams new bytes of every artifact of the session.
func (a *Adapter) Capture(ctx context.Context, ref agent.NativeRef, cur agent.Cursor, sink agent.SegmentSink) (agent.CaptureResult, error) {
	res := agent.CaptureResult{State: agent.CaptureOK, Cursor: agent.Cursor{Artifacts: map[string]session.ArtifactState{}}}
	s := scan(ref.Home)[ref.NativeID]
	if s == nil || s.main == "" {
		res.State = agent.CaptureDetached
		return res, nil
	}
	for _, f := range s.files {
		old := cur.Artifacts[f.rel]
		if f.kind == session.KindBlob {
			st, changed, err := streamcap.CaptureBlob(ctx, ref.Home, streamcap.File{Rel: f.rel, Mode: f.mode}, f.rel, old, sink)
			if err != nil {
				return res, err
			}
			res.Changed = res.Changed || changed
			res.Cursor.Artifacts[f.rel] = *st
			continue
		}
		st, state, changed, diags, err := streamcap.CaptureStream(ctx, ref.Home, f.rel, []streamcap.File{{Rel: f.rel, Mode: f.mode}}, old, sink,
			streamcap.Options{MaxRecordBytes: a.maxRecord()})
		if err != nil {
			return res, err
		}
		res.Diags = append(res.Diags, diags...)
		res.Changed = res.Changed || changed
		if st != nil {
			res.Cursor.Artifacts[f.rel] = *st
		} else if o, ok := cur.Artifacts[f.rel]; ok {
			res.Cursor.Artifacts[f.rel] = o
		}
		if state != agent.CaptureOK && (res.State == agent.CaptureOK || state == agent.CaptureUnsafe) {
			res.State = state
		}
	}
	return res, nil
}

// VerifyBinding checks captured artifacts against the files on disk.
func (a *Adapter) VerifyBinding(ctx context.Context, ref agent.NativeRef, arts map[string]session.ArtifactState, deep bool) ([]string, error) {
	s := scan(ref.Home)[ref.NativeID]
	files := map[string][]streamcap.File{}
	if s != nil {
		for _, f := range s.files {
			files[f.rel] = []streamcap.File{{Rel: f.rel, Mode: f.mode}}
		}
	}
	bad, err := streamcap.VerifyArtifacts(ref.Home, files, arts, deep)
	sort.Strings(bad)
	return bad, err
}

type runtimeDetector struct{}

func (a *Adapter) Runtime() agent.RuntimeDetector { return runtimeDetector{} }

// Running is conservative until E2a: WorkBuddy is a desktop app whose process
// identity is not yet determined.
func (runtimeDetector) Running(context.Context, string, string) (agent.RunState, agent.RunDetail, error) {
	return agent.Unknown, agent.DetailUnknown, nil
}

type writerStub struct{}

func (a *Adapter) Writer() agent.Writer { return writerStub{} }

var errWriter = errcode.New(errcode.Unsupported, "the WorkBuddy writer is not implemented until experiments E3/E4 finish")

func (writerStub) Plan(context.Context, agent.WriteInput) (agent.Plan, error) {
	return agent.Plan{}, errWriter
}
func (writerStub) Apply(context.Context, agent.WriteInput, agent.Plan, agent.Tx) (agent.Receipt, error) {
	return agent.Receipt{}, errWriter
}
func (writerStub) Verify(context.Context, agent.WriteInput, agent.Receipt) error { return errWriter }
func (writerStub) Rollback(context.Context, string, agent.Tx) error              { return errWriter }

// Probe inspects every WorkBuddy home read-only (structure only).
func (a *Adapter) Probe(ctx context.Context) (agent.ProbeResult, error) {
	res := agent.ProbeResult{SchemaVersion: 1, Agent: AgentName, Counts: map[string]int{}}
	parsed := 0
	for _, home := range a.Cfg.WorkBuddyHomes() {
		hp := agent.HomeProbe{Stores: map[string]bool{}, FileForms: map[string]int{}, RecordTypes: map[string]int{}}
		if st, err := os.Stat(home); err == nil && st.IsDir() {
			hp.Exists = true
		}
		for name, rel := range map[string]string{"projects_dir": "projects", "registry_db": "workbuddy.db", "legacy_kv": "../Library/Application Support/WorkBuddy/codebuddy-sessions.vscdb"} {
			_, err := os.Stat(filepath.Join(home, rel))
			hp.Stores[name] = err == nil
		}
		caps := map[string]bool{}
		all := scan(home)
		res.Counts["sessions"] += len(all)
		var mains []string
		for _, s := range all {
			if s.main != "" {
				mains = append(mains, s.main)
				hp.FileForms["jsonl"]++
			}
			for _, f := range s.files {
				switch {
				case strings.HasSuffix(f.rel, ".meta.json"):
					hp.FileForms["meta.json"]++
				case strings.Contains(f.rel, "/subagents/"):
					hp.FileForms["subagent"]++
				case strings.Contains(f.rel, "/tool-results/"):
					hp.FileForms["tool-result"]++
				}
			}
		}
		sort.Slice(mains, func(i, j int) bool {
			x, _ := os.Stat(filepath.Join(home, mains[i]))
			y, _ := os.Stat(filepath.Join(home, mains[j]))
			return x != nil && y != nil && x.ModTime().After(y.ModTime())
		})
		for i, m := range mains {
			if i >= 20 {
				break
			}
			parsed++
			histogram(filepath.Join(home, filepath.FromSlash(m)), hp.RecordTypes, caps)
		}
		if hp.Stores["registry_db"] {
			caps["db:sessions"] = true
		}
		for c := range caps {
			hp.Capabilities = append(hp.Capabilities, c)
		}
		sort.Strings(hp.Capabilities)
		res.Homes = append(res.Homes, hp)
	}
	res.Counts["parsed"] = parsed
	res.Compatibility = "ok"
	if parsed == 0 {
		res.Compatibility = "unsupported"
		res.Notes = []string{"no sessions found"}
	}
	return res, nil
}

func histogram(path string, types map[string]int, caps map[string]bool) {
	src, err := jsonl.OpenSource(path)
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
		var e map[string]json.RawMessage
		if json.Unmarshal(rec.Line, &e) != nil {
			continue
		}
		var t string
		json.Unmarshal(e["type"], &t)
		if t != "" {
			types[t]++
		}
		if _, ok := e["parentId"]; ok {
			caps["has:parentId"] = true
		}
	}
}

// IsScene reports whether a user text is a scene tag (non-human input).
func IsScene(text string) bool { return scene.MatchString(strings.TrimSpace(text)) }
