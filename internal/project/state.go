// Package project observes code state without transferring workspace contents.
package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

type Dirty struct {
	Modified  int `json:"modified"`
	Untracked int `json:"untracked"`
}

// State carries no local path, remote credentials, file contents or diff.
// Status is git, directory (an existing non-Git workspace), or unavailable.
type State struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	Branch        string `json:"branch"`
	Head          string `json:"head"`
	RemoteHash    string `json:"remote_hash"`
	Dirty         Dirty  `json:"dirty"`
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Local inspection must not trigger optional index writes or prompting.
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "LC_ALL=C")
	return cmd.Output()
}

func NormalizeRemote(remote string) string {
	s := strings.TrimSpace(remote)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		s = strings.ToLower(u.Host) + "/" + strings.TrimPrefix(u.Path, "/")
	} else if colon := strings.Index(s, ":"); colon >= 0 && !strings.Contains(s[:colon], "/") {
		host := s[:colon]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		s = strings.ToLower(host) + "/" + strings.TrimPrefix(s[colon+1:], "/")
	}
	s = strings.TrimRight(s, "/")
	return strings.TrimSuffix(s, ".git")
}

func Inspect(ctx context.Context, dir string) (State, error) {
	s := State{SchemaVersion: 1, Status: "unavailable"}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return s, nil
	}
	inside, err := git(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		if ctx.Err() != nil {
			return s, ctx.Err()
		}
		// A missing git executable or unreadable repository must not be
		// misreported as a known non-Git directory.
		if _, lookErr := exec.LookPath("git"); lookErr != nil {
			return s, nil
		}
		if failure, ok := err.(*exec.ExitError); ok && strings.Contains(string(failure.Stderr), "not a git repository") {
			s.Status = "directory"
		}
		return s, nil
	}
	s.Status = "git"
	head, err := git(ctx, dir, "rev-parse", "--verify", "HEAD")
	if err == nil {
		s.Head = strings.TrimSpace(string(head))
	}
	branch, err := git(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err == nil {
		s.Branch = strings.TrimSpace(string(branch))
	}
	remote, err := git(ctx, dir, "remote", "get-url", "origin")
	if err == nil && len(remote) > 0 {
		h := sha256.Sum256([]byte(NormalizeRemote(string(remote))))
		s.RemoteHash = hex.EncodeToString(h[:])
	}
	dirty, err := git(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		s.Status = "unavailable"
		return s, ctx.Err()
	}
	entries := strings.Split(string(dirty), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 3 {
			continue
		}
		if strings.HasPrefix(e, "?? ") {
			s.Dirty.Untracked++
		} else {
			s.Dirty.Modified++
		}
		if e[0] == 'R' || e[0] == 'C' || e[1] == 'R' || e[1] == 'C' {
			i++
		}
	}
	return s, ctx.Err()
}

// Gaps reports the code-state differences that need an explicit override.
func Gaps(ctx context.Context, source *State, target State, dir string) []string {
	var gaps []string
	if source == nil {
		gaps = append(gaps, "source_repo_state_missing")
	} else {
		if source.Status == "unavailable" {
			gaps = append(gaps, "source_repo_state_unavailable")
		}
		if source.Status == "git" {
			if target.Status != "git" {
				gaps = append(gaps, "target_not_git")
			}
			if source.Head == "" {
				gaps = append(gaps, "source_commit_unavailable")
			} else {
				if _, err := git(ctx, dir, "cat-file", "-e", source.Head+"^{commit}"); err != nil {
					gaps = append(gaps, "missing_commit")
				}
				if target.Head != source.Head {
					gaps = append(gaps, "head_mismatch")
				}
			}
			if source.RemoteHash != "" && source.RemoteHash != target.RemoteHash {
				gaps = append(gaps, "remote_mismatch")
			}
		}
		if source.Dirty.Modified+source.Dirty.Untracked > 0 {
			gaps = append(gaps, "source_dirty_files_not_transferred")
		}
	}
	if target.Status == "unavailable" {
		gaps = append(gaps, "target_repo_state_unavailable")
	}
	if target.Dirty.Modified+target.Dirty.Untracked > 0 {
		gaps = append(gaps, "target_dirty")
	}
	return gaps
}
