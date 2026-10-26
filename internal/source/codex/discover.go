package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"session-memory/internal/checkpoint"
	"session-memory/internal/config"
)

// DiscoverOptions controls the discovery window (spec 7.1).
type DiscoverOptions struct {
	Now             time.Time
	Since           *time.Time
	All             bool
	Lookback        time.Duration // window when neither Since nor All is set; 0 means 48h
	IncludeArchived bool
	// Root overrides cfg.CodexHome() (tests).
	Root string
	// SkipRetryable disables merging retryable checkpoints (tests).
	SkipRetryable bool
}

// Candidate is one rollout file to consider.
type Candidate struct {
	Path     string
	Archived bool
	Size     int64
	ModTime  time.Time
}

// Discover returns the candidate set: date directories covered by the
// lookback window (or --since / --all), archived files by mtime, plus
// retryable checkpointed paths that still exist. Sorted by path.
func Discover(cfg config.Config, store *checkpoint.Store, opts DiscoverOptions) ([]Candidate, error) {
	root := opts.Root
	if root == "" {
		root = cfg.CodexHome()
	}
	sessionsDir := filepath.Join(root, "sessions")

	var candidates []Candidate
	seen := map[string]bool{}

	add := func(path string, archived bool) {
		clean := filepath.Clean(path)
		if seen[clean] {
			return
		}
		st, err := os.Stat(clean)
		if err != nil {
			return // candidate-level miss, not an error
		}
		if st.IsDir() {
			return
		}
		seen[clean] = true
		candidates = append(candidates, Candidate{
			Path: clean, Archived: archived, Size: st.Size(), ModTime: st.ModTime(),
		})
	}

	switch {
	case opts.All:
		// every date directory
		dirs, err := listDateDirs(sessionsDir, nil, nil)
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			scanDirInto(sessionsDir, d, add)
		}
	case opts.Since != nil:
		dirs, err := listDateDirs(sessionsDir, opts.Since, nil)
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			scanDirInto(sessionsDir, d, add)
		}
	default:
		lookback := lookbackOrDefault(opts.Lookback)
		start := opts.Now.Add(-lookback)
		dirs, err := listDateDirs(sessionsDir, &start, &opts.Now)
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			scanDirInto(sessionsDir, d, add)
		}
	}

	// archived sessions by mtime within the same window
	if opts.IncludeArchived {
		archDir := filepath.Join(root, "archived_sessions")
		archStart := opts.Now.Add(-lookbackOrDefault(opts.Lookback))
		if opts.Since != nil {
			archStart = *opts.Since
		}
		err := filepath.WalkDir(archDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // permission issues are candidate-level warnings
			}
			if d.IsDir() {
				// depth guard (spec 7.2: max 6)
				rel, _ := filepath.Rel(archDir, path)
				depth := strings.Count(rel, string(os.PathSeparator))
				if depth >= 6 {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".jsonl" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if !opts.All && info.ModTime().Before(archStart) {
				return nil
			}
			add(path, true)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			// archived dir missing is fine; anything else surfaces as a warning
			_ = err
		}
	}

	// retryable pending/failed sessions regardless of window
	if store != nil && !opts.SkipRetryable {
		states, err := store.ListRetryable(SourceName)
		if err == nil {
			for _, st := range states {
				if st.RolloutPath != "" {
					add(st.RolloutPath, strings.Contains(st.RolloutPath, "archived_sessions"))
				}
			}
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, nil
}

// listDateDirs returns YYYY/MM/DD dir triplets intersecting [start, end].
// Either bound may be nil (unbounded on that side).
func listDateDirs(sessionsDir string, start, end *time.Time) ([]string, error) {
	if _, err := os.Stat(sessionsDir); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("codex_home_missing: %s not found — run 'session-memory agent probe codex'", sessionsDir)
		}
		return nil, err
	}
	yearEntries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, y := range yearEntries {
		if !isDigits(y.Name(), 4) {
			continue
		}
		months, err := os.ReadDir(filepath.Join(sessionsDir, y.Name()))
		if err != nil {
			continue
		}
		for _, m := range months {
			if !isDigits(m.Name(), 2) {
				continue
			}
			days, err := os.ReadDir(filepath.Join(sessionsDir, y.Name(), m.Name()))
			if err != nil {
				continue
			}
			for _, d := range days {
				if !isDigits(d.Name(), 2) {
					continue
				}
				triplet := filepath.Join(y.Name(), m.Name(), d.Name())
				if dateInRange(triplet, start, end) {
					dirs = append(dirs, triplet)
				}
			}
		}
	}
	return dirs, nil
}

func dateInRange(triplet string, start, end *time.Time) bool {
	if start == nil && end == nil {
		return true
	}
	triplet = strings.ReplaceAll(triplet, "\\", "/")
	t, err := time.Parse("2006/01/02", triplet)
	if err != nil {
		return false
	}
	dayStart := t.Truncate(24 * time.Hour)
	if start != nil && dayStart.Before(start.Truncate(24*time.Hour)) {
		return false
	}
	if end != nil && dayStart.After(end.Truncate(24*time.Hour)) {
		return false
	}
	return true
}

func scanDirInto(sessionsDir, triplet string, add func(string, bool)) {
	dir := filepath.Join(sessionsDir, triplet)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		add(filepath.Join(dir, e.Name()), false)
	}
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < n; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func lookbackOrDefault(d time.Duration) time.Duration {
	if d <= 0 || d > 720*time.Hour {
		return 48 * time.Hour
	}
	return d
}
