package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A session from another computer names a folder that does not exist here.
// When the user has not said where the project lives on this computer, the
// folder with the same name is looked for: first among the agent's own known
// project folders (hints), then in the usual places under the home directory.
// Only an unambiguous match is used; with none or several the caller keeps
// asking the user.

var autoDirDepth = 3

const autoDirScanLimit = 30000

var autoDirRootNames = []string{"Documents", "Desktop", "Developer", "Projects", "projects", "Code", "code", "src", "work", "Work", "workspace", "Workspace", "repos", "Repos", "GitHub", "github", "dev"}

var autoDirSkip = map[string]bool{"node_modules": true, "Library": true, "vendor": true, "venv": true, "Applications": true, "Pictures": true, "Music": true, "Movies": true, "Downloads": true}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func sameAsAny(list []os.FileInfo, st os.FileInfo) bool {
	for _, o := range list {
		if os.SameFile(o, st) {
			return true
		}
	}
	return false
}

func autoDirRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	roots := []string{}
	var infos []os.FileInfo
	for _, n := range autoDirRootNames {
		p := filepath.Join(home, n)
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() || sameAsAny(infos, st) {
			continue // on a case-insensitive disk "work" and "Work" are one folder
		}
		infos = append(infos, st)
		roots = append(roots, p)
	}
	return roots
}

// LocalDirCandidates lists the local folders named like origin's last element.
func LocalDirCandidates(origin string, hints []string) []string {
	name := filepath.Base(filepath.Clean(origin))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return nil
	}
	var seen []os.FileInfo
	var out []string
	add := func(p string) {
		st, err := os.Stat(p)
		if err != nil || sameAsAny(seen, st) {
			return
		}
		seen = append(seen, st)
		out = append(out, p)
	}
	for _, h := range hints {
		if strings.EqualFold(filepath.Base(filepath.Clean(h)), name) && dirExists(h) {
			add(h)
		}
	}
	if len(out) > 0 { // the agent's own project list is authoritative
		return out
	}
	scanned := 0
	for _, root := range autoDirRoots() {
		rootDepth := strings.Count(root, string(filepath.Separator))
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return fs.SkipDir
			}
			if !d.IsDir() {
				return nil
			}
			if scanned++; scanned > autoDirScanLimit {
				return fs.SkipAll
			}
			if p == root {
				return nil
			}
			base := d.Name()
			if strings.HasPrefix(base, ".") || autoDirSkip[base] {
				return fs.SkipDir
			}
			if strings.EqualFold(base, name) {
				add(p)
				return fs.SkipDir // do not look for the folder inside itself
			}
			if strings.Count(p, string(filepath.Separator))-rootDepth >= autoDirDepth {
				return fs.SkipDir
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// UniqueLocalDir returns the single local folder matching origin, if there is exactly one.
func UniqueLocalDir(origin string, hints []string) (string, bool) {
	c := LocalDirCandidates(origin, hints)
	if len(c) == 1 {
		return c[0], true
	}
	return "", false
}
