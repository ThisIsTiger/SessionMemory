// Package p0 holds the throwaway tooling of the P0 experiments. The commands
// that use it are compiled only with `-tags dev` and never ship (dev.md §8, M1).
package p0

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"session-memory/internal/errcode"
)

// MarkerName is the file that marks a directory as an isolated sandbox. Every
// dev command that writes refuses a target whose sandbox root lacks it.
const MarkerName = ".sm-sandbox"

// Marker is the content of MarkerName.
type Marker struct {
	SchemaVersion int    `json:"schema_version"`
	CreatedAt     string `json:"created_at"`
	Purpose       string `json:"purpose"`
	Owner         string `json:"owner"` // always "session-memory-dev"
}

// Inventory is the output of `dev inventory`: structure only (names, counts,
// table/column names, record type names, JSON key names). It never contains
// titles, prompts, message text, cwd values or any file under an auth or
// credentials path.
type Inventory struct {
	SchemaVersion int          `json:"schema_version"`
	Agent         string       `json:"agent"`
	Home          string       `json:"home"`
	GeneratedAt   string       `json:"generated_at"`
	ClientVersion string       `json:"client_version,omitempty"`
	OS            string       `json:"os"`
	Dirs          []DirInfo    `json:"dirs"`
	SQLite        []SQLiteInfo `json:"sqlite"`
	JSONL         []JSONLInfo  `json:"jsonl"`
}

// DirInfo describes one top-level entry of the home.
type DirInfo struct {
	Path  string         `json:"path"` // relative to the home
	IsDir bool           `json:"is_dir"`
	Files int            `json:"files"` // recursive
	Bytes int64          `json:"bytes"`
	Ext   map[string]int `json:"ext"` // extension -> count ("jsonl", "jsonl.zst", "db", ...)
}

// SQLiteInfo is read through file:<path>?mode=ro&immutable=1.
type SQLiteInfo struct {
	Path        string      `json:"path"`
	UserVersion int         `json:"user_version"`
	Tables      []TableInfo `json:"tables"`
	Error       string      `json:"error,omitempty"` // code only
}

// TableInfo lists a table's columns and row count (COUNT(*) only).
type TableInfo struct {
	Name    string       `json:"name"`
	Rows    int64        `json:"rows"`
	Columns []ColumnInfo `json:"columns"`
}

// ColumnInfo is one column (from PRAGMA table_info).
type ColumnInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null"`
	PK      bool   `json:"pk"`
}

// JSONLInfo summarises the JSONL files matching one glob.
type JSONLInfo struct {
	Glob       string              `json:"glob"`
	Files      int                 `json:"files"`
	Forms      map[string]int      `json:"forms"`   // plain | zstd
	Records    int64               `json:"records"` // examined
	ByType     map[string]int      `json:"by_type"` // "type" or "type/payload.type" -> count
	KeysByType map[string][]string `json:"keys_by_type"`
}

// RequireSandbox returns nil only when target lies inside a directory that
// contains MarkerName and does not overlap any real agent home.
//
// Algorithm: resolve target with filepath.EvalSymlinks (the existing prefix
// when it does not exist yet); walk up to the first ancestor containing
// MarkerName; fail with errcode.NotIsolated if none is found, if the resolved
// path is, contains, or is contained by any of realHomes (also resolved), or
// if the user's home directory itself would be the sandbox root.
func RequireSandbox(target string, realHomes []string) error {
	resolved := resolveExisting(target)
	root := ""
	for dir := resolved; ; dir = filepath.Dir(dir) {
		if b, err := os.ReadFile(filepath.Join(dir, MarkerName)); err == nil {
			var m Marker
			if json.Unmarshal(b, &m) == nil && m.Owner == "session-memory-dev" {
				root = dir
				break
			}
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if root == "" {
		return errcode.New(errcode.NotIsolated, "target is not inside a marked sandbox")
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(root, home) {
		return errcode.New(errcode.NotIsolated, "the user's home directory cannot be a sandbox")
	}
	for _, h := range realHomes {
		rh := resolveExisting(h)
		if samePath(resolved, rh) || within(resolved, rh) || within(rh, resolved) {
			return errcode.New(errcode.NotIsolated, "target overlaps a real agent home")
		}
	}
	return nil
}

func resolveExisting(p string) string {
	p = filepath.Clean(p)
	for cur := p; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, strings.TrimPrefix(p, cur))
		}
		if filepath.Dir(cur) == cur {
			return p
		}
	}
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(resolveExisting(b)) }

func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}
