// Package claude implements the Claude Code source adapter: SessionEnd hook
// ingress, a queue + detached worker pipeline, and the transcript parser.
package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"session-memory/internal/config"
	"session-memory/internal/platform"
)

// HookInput is the Claude Code SessionEnd hook stdin schema.
type HookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// ParseHookInput validates the hook payload (spec 9.4).
func ParseHookInput(data []byte) (HookInput, error) {
	var in HookInput
	if err := json.Unmarshal(data, &in); err != nil {
		return in, fmt.Errorf("parsing hook input: %w", err)
	}
	if in.SessionID == "" {
		return in, fmt.Errorf("session_id is required")
	}
	if !filepath.IsAbs(in.TranscriptPath) {
		return in, fmt.Errorf("transcript_path must be absolute")
	}
	if !filepath.IsAbs(in.Cwd) {
		return in, fmt.Errorf("cwd must be absolute")
	}
	return in, nil
}

// Enqueue atomically writes the hook payload into the claude queue directory.
// It returns the queue file path. Directory is 0700, file 0600.
func Enqueue(cfg config.Config, in HookInput) (string, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	name := safeQueueName(in.SessionID)
	dir := filepath.Join(cfg.StateDir(), "queue", "claude")
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".json")
	if err := platform.AtomicWrite(path, data, 0600); err != nil {
		return "", err
	}
	return path, nil
}

// ListQueue returns pending queue files (sorted for determinism).
func ListQueue(cfg config.Config) ([]string, error) {
	dir := filepath.Join(cfg.StateDir(), "queue", "claude")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths, nil
}

// ReadQueueFile loads one queued hook payload.
func ReadQueueFile(path string) (HookInput, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HookInput{}, err
	}
	return ParseHookInput(data)
}

func safeQueueName(sessionID string) string {
	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
	out := make([]byte, 0, len(sessionID))
	for i := 0; i < len(sessionID); i++ {
		c := sessionID[i]
		ok := false
		for j := 0; j < len(allowed); j++ {
			if allowed[j] == c {
				ok = true
				break
			}
		}
		if ok {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	name := string(out)
	if name == "" {
		return "anonymous"
	}
	// never allow a path-traversal prefix even after sanitizing
	for strings.HasPrefix(name, ".") {
		name = strings.TrimPrefix(name, ".")
	}
	if name == "" {
		name = "anonymous"
	}
	h := sha256.Sum256([]byte(sessionID))
	return fmt.Sprintf("%s-%s", name, hex.EncodeToString(h[:4]))
}

// MaxHookInputBytes bounds stdin reads (spec: 1 MiB).
const MaxHookInputBytes = 1 << 20

// ReadHookStdin reads the bounded hook payload from stdin.
func ReadHookStdin(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, MaxHookInputBytes+1))
}

// StdinTooLarge reports whether the payload exceeded the bound.
func StdinTooLarge(data []byte) bool { return len(data) > MaxHookInputBytes }
