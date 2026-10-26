package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProbeResult is the safe output of `session-memory codex probe` (spec 11.3).
// It never contains titles, cwds, message text or raw thread_source objects.
type ProbeResult struct {
	SchemaVersion      int            `json:"schema_version"`
	HomeExists         bool           `json:"home_exists"`
	SessionsExists     bool           `json:"sessions_dir_exists"`
	ArchiveExists      bool           `json:"archive_dir_exists"`
	StateDBExists      bool           `json:"state_db_exists"`
	CandidateFiles     int            `json:"candidate_files"`
	ParseableMeta      int            `json:"parseable_meta"`
	InvalidMeta        int            `json:"invalid_meta"`
	SourceCounts       map[string]int `json:"source_counts"`
	OriginatorCounts   map[string]int `json:"originator_counts"`
	UnknownSchemaCount int            `json:"unknown_schema_count"`
	Compatibility      string         `json:"compatibility"` // ok | partial | unsupported
	Note               string         `json:"note,omitempty"`
}

// Probe scans codex home read-only, first line only, and classifies
// compatibility. Root overrides the home dir (tests).
func Probe(ctx context.Context, root string, lookbackDays int) (ProbeResult, error) {
	res := ProbeResult{
		SchemaVersion:    1,
		SourceCounts:     map[string]int{},
		OriginatorCounts: map[string]int{},
	}
	if root == "" {
		return res, fmt.Errorf("codex home not configured")
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		res.Note = "codex home not found"
		res.Compatibility = "unsupported"
		return res, nil
	}
	res.HomeExists = true

	sessionsDir := filepath.Join(root, "sessions")
	if st, err := os.Stat(sessionsDir); err == nil && st.IsDir() {
		res.SessionsExists = true
	}
	if st, err := os.Stat(filepath.Join(root, "archived_sessions")); err == nil && st.IsDir() {
		res.ArchiveExists = true
	}
	if _, err := os.Stat(filepath.Join(root, "state_5.sqlite")); err == nil {
		res.StateDBExists = true
	}

	if !res.SessionsExists {
		res.Compatibility = "unsupported"
		res.Note = "sessions directory missing"
		return res, nil
	}

	// scan date dirs (bounded by lookback days, or all when lookbackDays<=0)
	var dirs []string
	years, _ := os.ReadDir(sessionsDir)
	for _, y := range years {
		if !y.IsDir() || !isDigitsName(y.Name(), 4) {
			continue
		}
		months, _ := os.ReadDir(filepath.Join(sessionsDir, y.Name()))
		for _, m := range months {
			if !m.IsDir() || !isDigitsName(m.Name(), 2) {
				continue
			}
			days, _ := os.ReadDir(filepath.Join(sessionsDir, y.Name(), m.Name()))
			for _, d := range days {
				if !d.IsDir() || !isDigitsName(d.Name(), 2) {
					continue
				}
				dirs = append(dirs, filepath.Join(sessionsDir, y.Name(), m.Name(), d.Name()))
			}
		}
	}

	for _, dir := range dirs {
		select {
		case <-ctx.Done():
			res.Compatibility = "partial"
			res.Note = "scan cancelled"
			return res, nil
		default:
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
				continue
			}
			res.CandidateFiles++
			meta, err := readMetaFirstLine(filepath.Join(dir, e.Name()))
			if err != nil {
				res.InvalidMeta++
				continue
			}
			res.ParseableMeta++
			src := meta.Source
			if src == "" {
				src = "<missing>"
			}
			res.SourceCounts[src]++
			orig := meta.Originator
			if orig == "" {
				orig = "<missing>"
			}
			res.OriginatorCounts[orig]++
		}
	}

	// compatibility decision
	if res.CandidateFiles == 0 {
		res.Compatibility = "ok"
		res.Note = "no rollout files found (empty history is compatible)"
		return res, nil
	}
	if res.ParseableMeta == 0 {
		res.Compatibility = "unsupported"
		res.Note = "no parseable session_meta"
		return res, nil
	}
	if res.InvalidMeta > 0 {
		res.Compatibility = "partial"
		res.Note = fmt.Sprintf("%d file(s) had unreadable meta", res.InvalidMeta)
		return res, nil
	}
	res.Compatibility = "ok"
	return res, nil
}

// candidates is an unexported scratch field guard; see candidatesField below.
func isDigitsName(s string, n int) bool {
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

// readMetaFirstLine reads only the first complete line of a rollout.
func readMetaFirstLine(path string) (SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<16)
	line, err := readProbeLine(br)
	if len(line) == 0 {
		return SessionMeta{}, fmt.Errorf("empty")
	}
	if err != nil {
		return SessionMeta{}, err
	}
	return ParseSessionMeta(line)
}

func readProbeLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return buf, nil
		case isEOF(err):
			return nil, err
		default:
			return nil, err
		}
	}
}

func isEOF(err error) bool {
	return err != nil && strings.Contains(err.Error(), "EOF")
}

// MarshalProbe renders the probe result as JSON.
func MarshalProbe(r ProbeResult) (string, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	return string(b), err
}
