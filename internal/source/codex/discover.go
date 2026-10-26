package codex

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/jsonl"
	"session-memory/internal/session"
)

// file is one rollout file on disk.
type file struct {
	rel   string // relative to home, slash-separated, as on disk
	logic string // rel without .zst
	zst   bool
	size  int64
	mtime string
	mode  os.FileMode
}

// threadFiles scans one home and groups rollout files by thread id.
func threadFiles(home string) (map[string][]file, error) {
	out := map[string][]file{}
	walk := func(sub string, maxDepth int) {
		root := filepath.Join(home, sub)
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if d.IsDir() {
				if rel != "." && strings.Count(rel, string(os.PathSeparator))+1 > maxDepth {
					return filepath.SkipDir
				}
				return nil
			}
			thread, zst, ok := parseName(d.Name())
			if !ok {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			r := filepath.ToSlash(filepath.Join(sub, rel))
			out[thread] = append(out[thread], file{rel: r, logic: logicalName(r), zst: zst, size: info.Size(),
				mtime: session.FormatTime(info.ModTime()), mode: info.Mode().Perm()})
			return nil
		})
	}
	walk("sessions", 3)
	walk("archived_sessions", 6)
	for _, fs := range out {
		sort.Slice(fs, func(i, j int) bool { return fs[i].rel < fs[j].rel })
	}
	return out, nil
}

type metaPayload struct {
	Cwd           string          `json:"cwd"`
	Originator    string          `json:"originator"`
	Source        json.RawMessage `json:"source"`
	CLIVersion    string          `json:"cli_version"`
	ModelProvider string          `json:"model_provider"`
	Timestamp     string          `json:"timestamp"`
}

// readMeta reads the first record (session_meta) of a rollout.
func readMeta(home string, f file) (metaPayload, bool) {
	src, err := jsonl.OpenSource(filepath.Join(home, filepath.FromSlash(f.rel)))
	if err != nil {
		return metaPayload{}, false
	}
	rc, _, err := src.OpenAt(0)
	if err != nil {
		return metaPayload{}, false
	}
	r := jsonl.NewReader(rc, 0, jsonl.Options{MaxRecordBytes: 1 << 20})
	defer r.Close()
	rec, err := r.Next()
	if err != nil && err != io.EOF {
		return metaPayload{}, false
	}
	var env struct {
		Type    string      `json:"type"`
		Payload metaPayload `json:"payload"`
	}
	if json.Unmarshal(rec.Line, &env) != nil || env.Type != "session_meta" {
		return metaPayload{}, false
	}
	return env.Payload, true
}

func sourceString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	if len(raw) > 0 && raw[0] == '{' {
		return "<object>"
	}
	return ""
}

func surfaceOf(m metaPayload) string {
	src := sourceString(m.Source)
	switch {
	case src == "vscode" || strings.Contains(m.Originator, "Desktop"):
		return "desktop"
	case src == "cli" || src == "exec":
		return src
	case src == "":
		return "unknown"
	}
	return src
}

func profileOf(m metaPayload) session.AgentProfile {
	return session.AgentProfile{Agent: AgentName, Surface: surfaceOf(m), Version: m.CLIVersion,
		Format: "rollout-jsonl", AdapterVersion: AdapterVersion, OS: osName()}
}

// Adapter implements agent.Adapter for Codex.
type Adapter struct{ Cfg config.Config }

// New returns the adapter for cfg.
func New(cfg config.Config) *Adapter { return &Adapter{Cfg: cfg} }

func (a *Adapter) Name() string { return AgentName }

func (a *Adapter) homes(o agent.DiscoverOptions) []string {
	if len(o.Homes) > 0 {
		return o.Homes
	}
	return a.Cfg.CodexHomes()
}

// Discover lists threads in every home. It reads only each thread's first
// record, never message text.
func (a *Adapter) Discover(ctx context.Context, o agent.DiscoverOptions) ([]agent.NativeSession, error) {
	want := map[string]bool{}
	for _, id := range o.NativeIDs {
		want[strings.ToLower(id)] = true
	}
	var out []agent.NativeSession
	for _, home := range a.homes(o) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		threads, _ := threadFiles(home)
		titles := loadTitles(home)
		for id, files := range threads {
			if len(want) > 0 && !want[id] {
				continue
			}
			out = append(out, nativeSession(home, id, files, titles))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out, nil
}

func nativeSession(home, id string, files []file, titles map[string]string) agent.NativeSession {
	ns := agent.NativeSession{Ref: agent.NativeRef{Home: home, NativeID: id}, Title: titles[id]}
	for _, f := range files {
		ns.Files = append(ns.Files, agent.FileStat{Path: f.rel, Size: f.size, ModTime: f.mtime})
		if f.mtime > ns.UpdatedAt {
			ns.UpdatedAt = f.mtime
		}
	}
	if m, ok := readMeta(home, files[0]); ok {
		ns.Cwd = m.Cwd
		ns.Profile = profileOf(m)
	} else {
		ns.Profile = session.AgentProfile{Agent: AgentName, Surface: "unknown", Format: "rollout-jsonl", AdapterVersion: AdapterVersion, OS: osName()}
	}
	return ns
}

// loadTitles reads session_index.jsonl (id -> thread_name). Local display
// text only.
func loadTitles(home string) map[string]string {
	out := map[string]string{}
	src, err := jsonl.OpenSource(filepath.Join(home, "session_index.jsonl"))
	if err != nil {
		return out
	}
	rc, _, err := src.OpenAt(0)
	if err != nil {
		return out
	}
	r := jsonl.NewReader(rc, 0, jsonl.Options{})
	defer r.Close()
	for {
		rec, err := r.Next()
		if err != nil {
			return out
		}
		var e struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(rec.Line, &e) == nil && e.ID != "" {
			out[strings.ToLower(e.ID)] = e.Name
		}
	}
}

// Locate finds a thread again by id (after a move, archive or compression).
func (a *Adapter) Locate(ctx context.Context, ref agent.NativeRef) (agent.NativeSession, bool, error) {
	threads, _ := threadFiles(ref.Home)
	files := threads[strings.ToLower(ref.NativeID)]
	if len(files) == 0 {
		return agent.NativeSession{}, false, nil
	}
	return nativeSession(ref.Home, strings.ToLower(ref.NativeID), files, nil), true, nil
}
