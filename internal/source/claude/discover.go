package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"session-memory/internal/checkpoint"
	"session-memory/internal/config"
)

// Candidate is one Claude Code transcript file to consider.
type Candidate struct {
	SessionID       string    `json:"session_id"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	IsSubagent      bool      `json:"is_subagent,omitempty"`
	SubagentDesc    string    `json:"subagent_desc,omitempty"`
	Path            string    `json:"path"`
	Cwd             string    `json:"cwd"`
	Size            int64     `json:"size"`
	ModTime         time.Time `json:"mod_time"`
}

// DiscoverOptions controls the discovery scan window.
type DiscoverOptions struct {
	Now              time.Time
	Since            *time.Time
	All              bool
	Lookback         time.Duration // window when neither Since nor All is set; 0 means 7 days
	ExcludeSubagents bool          // if true, skips subagent discovery (defaults to including subagents)
	Root             string        // overrides cfg.ClaudeHome() for tests
	SkipRetryable    bool          // disables merging retryable checkpoints
}

// SubagentMeta matches the companion agent-*.meta.json file Claude Code writes.
type SubagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	ToolUseID   string `json:"toolUseId"`
	SpawnDepth  int    `json:"spawnDepth"`
}

// Discover scans ~/.claude/projects/ for main sessions and subagent transcripts,
// filtering by modification time against the lookback window or --since / --all,
// and merging retryable checkpointed paths. Sorted by path for determinism.
func Discover(cfg config.Config, store *checkpoint.Store, opts DiscoverOptions) ([]Candidate, error) {
	root := opts.Root
	if root == "" {
		root = cfg.ClaudeHome()
	}
	projectsDir := filepath.Join(root, "projects")
	if _, err := os.Stat(projectsDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil // clean empty result if projects directory has not been created yet
		}
		return nil, err
	}

	var candidates []Candidate
	seen := map[string]bool{}

	maxRecord := cfg.Agents.Claude.MaxRecordBytes
	if maxRecord <= 0 {
		maxRecord = 16 << 20
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	lookback := opts.Lookback
	if lookback <= 0 {
		lookback = 7 * 24 * time.Hour
	}

	windowStart := now.Add(-lookback)
	if opts.Since != nil {
		windowStart = *opts.Since
	}

	isEligibleTime := func(mtime time.Time) bool {
		if opts.All {
			return true
		}
		return !mtime.Before(windowStart)
	}

	add := func(c Candidate) {
		clean := filepath.Clean(c.Path)
		if seen[clean] {
			return
		}
		seen[clean] = true
		candidates = append(candidates, c)
	}

	projEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, err
	}

	for _, pe := range projEntries {
		if !pe.IsDir() {
			continue
		}
		projDir := filepath.Join(projectsDir, pe.Name())
		files, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}

		var projCwd string
		var subagentParentDirs []string

		for _, f := range files {
			fPath := filepath.Join(projDir, f.Name())
			if f.IsDir() {
				subDir := filepath.Join(fPath, "subagents")
				if info, err := os.Stat(subDir); err == nil && info.IsDir() {
					subagentParentDirs = append(subagentParentDirs, fPath)
				}
				continue
			}

			if filepath.Ext(f.Name()) != ".jsonl" {
				continue
			}

			info, err := f.Info()
			if err != nil {
				continue
			}
			if !isEligibleTime(info.ModTime()) {
				continue
			}

			sessionID := strings.TrimSuffix(f.Name(), ".jsonl")
			cwd, _ := readCwd(fPath, maxRecord)
			if cwd != "" && projCwd == "" {
				projCwd = cwd
			}

			add(Candidate{
				SessionID:       sessionID,
				ParentSessionID: "",
				IsSubagent:      false,
				Path:            fPath,
				Cwd:             cwd,
				Size:            info.Size(),
				ModTime:         info.ModTime(),
			})
		}

		// Second pass: subagent transcripts in <session-id>/subagents/
		if !opts.ExcludeSubagents {
			for _, parentDir := range subagentParentDirs {
				parentSessionID := filepath.Base(parentDir)
				subDir := filepath.Join(parentDir, "subagents")
				subEntries, err := os.ReadDir(subDir)
				if err != nil {
					continue
				}

				for _, se := range subEntries {
					if se.IsDir() || filepath.Ext(se.Name()) != ".jsonl" {
						continue
					}
					info, err := se.Info()
					if err != nil {
						continue
					}
					if !isEligibleTime(info.ModTime()) {
						continue
					}

					subPath := filepath.Join(subDir, se.Name())
					subSessionID := strings.TrimSuffix(se.Name(), ".jsonl")

					var desc string
					metaPath := filepath.Join(subDir, subSessionID+".meta.json")
					if metaBytes, err := os.ReadFile(metaPath); err == nil {
						var meta SubagentMeta
						if json.Unmarshal(metaBytes, &meta) == nil {
							desc = meta.Description
							if desc == "" {
								desc = meta.AgentType
							}
						}
					}

					cwd, _ := readCwd(subPath, maxRecord)
					if cwd == "" {
						cwd = projCwd
					}

					add(Candidate{
						SessionID:       subSessionID,
						ParentSessionID: parentSessionID,
						IsSubagent:      true,
						SubagentDesc:    desc,
						Path:            subPath,
						Cwd:             cwd,
						Size:            info.Size(),
						ModTime:         info.ModTime(),
					})
				}
			}
		}
	}

	// Retryable pending/failed sessions from checkpoint store regardless of window
	if store != nil && !opts.SkipRetryable {
		states, err := store.ListRetryable(SourceName)
		if err == nil {
			for _, st := range states {
				if st.RolloutPath != "" && !seen[filepath.Clean(st.RolloutPath)] {
					fi, err := os.Stat(st.RolloutPath)
					if err == nil && !fi.IsDir() {
						isSub := st.ParentSession != "" || strings.Contains(st.RolloutPath, "/subagents/")
						var parentID string
						if isSub {
							if st.ParentSession != "" {
								parentID = st.ParentSession
							} else {
								parentID = filepath.Base(filepath.Dir(filepath.Dir(st.RolloutPath)))
							}
						}
						cwd, _ := readCwd(st.RolloutPath, maxRecord)
						add(Candidate{
							SessionID:       st.SessionID,
							ParentSessionID: parentID,
							IsSubagent:      isSub,
							Path:            st.RolloutPath,
							Cwd:             cwd,
							Size:            fi.Size(),
							ModTime:         fi.ModTime(),
						})
					}
				}
			}
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Path < candidates[j].Path
	})
	return candidates, nil
}

// readCwd quickly extracts the working directory from the first valid JSON line
// containing a non-empty "cwd" field.
func readCwd(path string, maxRecord int64) (string, error) {
	if maxRecord <= 0 {
		maxRecord = 16 << 20
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for i := 0; i < 50; i++ {
		line, err := readFullLine(reader, maxRecord)
		if len(line) > 0 {
			var env struct {
				Cwd string `json:"cwd"`
			}
			if jerr := json.Unmarshal(line, &env); jerr == nil && env.Cwd != "" {
				return env.Cwd, nil
			}
		}
		if err != nil {
			break
		}
	}
	return "", fmt.Errorf("cwd not found in transcript %s", path)
}
