package p0

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"session-memory/internal/jsonl"
)

// skipDir names are never entered: credentials and secrets (spec §15).
var skipNames = map[string]bool{"auth.json": true, "credentials": true, ".env": true, "keychain": true, "cookies": true}

func skipped(name string) bool {
	l := strings.ToLower(name)
	return skipNames[l] || strings.Contains(l, "credential") || strings.Contains(l, "secret") || strings.Contains(l, "token") || l == "cookies"
}

func extOf(name string) string {
	switch {
	case strings.HasSuffix(name, ".jsonl.zst"):
		return "jsonl.zst"
	case strings.HasSuffix(name, ".sqlite-wal"), strings.HasSuffix(name, ".sqlite-shm"):
		return "sqlite-sidecar"
	}
	return strings.TrimPrefix(filepath.Ext(name), ".")
}

// Collect builds the structural inventory of an agent home. It reads names,
// counts, SQLite schemas and record type / key names only.
func Collect(ctx context.Context, agentName, home string) (Inventory, error) {
	inv := Inventory{SchemaVersion: 1, Agent: agentName, Home: home, GeneratedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), OS: runtime.GOOS}
	ents, err := os.ReadDir(home)
	if err != nil {
		return inv, err
	}
	var sqlites, jsonls []string
	for _, e := range ents {
		if skipped(e.Name()) {
			inv.Dirs = append(inv.Dirs, DirInfo{Path: e.Name(), IsDir: e.IsDir(), Ext: map[string]int{"(skipped)": 1}})
			continue
		}
		d := DirInfo{Path: e.Name(), IsDir: e.IsDir(), Ext: map[string]int{}}
		root := filepath.Join(home, e.Name())
		if !e.IsDir() {
			info, _ := e.Info()
			d.Files, d.Ext[extOf(e.Name())] = 1, 1
			if info != nil {
				d.Bytes = info.Size()
			}
			switch {
			case strings.HasSuffix(e.Name(), ".sqlite"), strings.HasSuffix(e.Name(), ".db"), strings.HasSuffix(e.Name(), ".vscdb"):
				sqlites = append(sqlites, root)
			case strings.HasSuffix(e.Name(), ".jsonl"):
				jsonls = append(jsonls, root)
			}
			inv.Dirs = append(inv.Dirs, d)
			continue
		}
		filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if de.IsDir() {
				if skipped(de.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := de.Info()
			if err != nil {
				return nil
			}
			d.Files++
			d.Bytes += info.Size()
			d.Ext[extOf(de.Name())]++
			if strings.HasSuffix(de.Name(), ".sqlite") || strings.HasSuffix(de.Name(), ".db") {
				if filepath.Dir(p) == root || strings.Count(strings.TrimPrefix(p, root), string(os.PathSeparator)) <= 2 {
					sqlites = append(sqlites, p)
				}
			}
			if strings.HasSuffix(de.Name(), ".jsonl") || strings.HasSuffix(de.Name(), ".jsonl.zst") {
				jsonls = append(jsonls, p)
			}
			return nil
		})
		inv.Dirs = append(inv.Dirs, d)
	}
	sort.Strings(sqlites)
	for _, p := range sqlites {
		rel, _ := filepath.Rel(home, p)
		inv.SQLite = append(inv.SQLite, inspectSQLite(ctx, p, filepath.ToSlash(rel)))
	}
	inv.JSONL = summarizeJSONL(home, jsonls)
	return inv, nil
}

func inspectSQLite(ctx context.Context, path, rel string) SQLiteInfo {
	info := SQLiteInfo{Path: rel}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		info.Error = "open_failed"
		return info
	}
	defer db.Close()
	db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&info.UserVersion)
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		info.Error = "schema_unreadable"
		return info
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		t := TableInfo{Name: n}
		q := `"` + strings.ReplaceAll(n, `"`, `""`) + `"`
		db.QueryRowContext(ctx, `SELECT count(*) FROM `+q).Scan(&t.Rows)
		cr, err := db.QueryContext(ctx, `PRAGMA table_info(`+q+`)`)
		if err == nil {
			for cr.Next() {
				var cid, notnull, pk int
				var name, typ string
				var dflt any
				cr.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
				t.Columns = append(t.Columns, ColumnInfo{Name: name, Type: typ, NotNull: notnull == 1, PK: pk > 0})
			}
			cr.Close()
		}
		info.Tables = append(info.Tables, t)
	}
	return info
}

// summarizeJSONL groups files by their top-level directory and examines up to
// 20 newest files per group (200 records each).
func summarizeJSONL(home string, files []string) []JSONLInfo {
	groups := map[string][]string{}
	for _, f := range files {
		rel, _ := filepath.Rel(home, f)
		top := strings.Split(filepath.ToSlash(rel), "/")[0]
		groups[top+"/**"] = append(groups[top+"/**"], f)
	}
	var keys []string
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []JSONLInfo
	for _, g := range keys {
		fs := groups[g]
		sort.Slice(fs, func(i, j int) bool {
			a, _ := os.Stat(fs[i])
			b, _ := os.Stat(fs[j])
			return a != nil && b != nil && a.ModTime().After(b.ModTime())
		})
		ji := JSONLInfo{Glob: g, Files: len(fs), Forms: map[string]int{}, ByType: map[string]int{}, KeysByType: map[string][]string{}}
		keysets := map[string]map[string]bool{}
		for i, f := range fs {
			if strings.HasSuffix(f, ".zst") {
				ji.Forms["zstd"]++
			} else {
				ji.Forms["plain"]++
			}
			if i >= 20 {
				continue
			}
			src, err := jsonl.OpenSource(f)
			if err != nil {
				continue
			}
			rc, _, err := src.OpenAt(0)
			if err != nil {
				continue
			}
			r := jsonl.NewReader(rc, 0, jsonl.Options{MaxRecordBytes: 4 << 20})
			for n := 0; n < 200; n++ {
				rec, err := r.Next()
				if err != nil {
					break
				}
				var m map[string]json.RawMessage
				if json.Unmarshal(rec.Line, &m) != nil {
					continue
				}
				typ := rawString(m["type"])
				if typ == "" {
					typ = "<none>"
				}
				var payload map[string]json.RawMessage
				json.Unmarshal(m["payload"], &payload)
				name := typ
				if pt := rawString(payload["type"]); pt != "" {
					name += "/" + pt
				}
				ji.ByType[name]++
				ji.Records++
				ks := keysets[name]
				if ks == nil {
					ks = map[string]bool{}
					keysets[name] = ks
				}
				for k := range m {
					ks[k] = true
				}
				for k := range payload {
					ks["payload."+k] = true
				}
			}
			r.Close()
		}
		for name, ks := range keysets {
			var l []string
			for k := range ks {
				l = append(l, k)
			}
			sort.Strings(l)
			ji.KeysByType[name] = l
		}
		out = append(out, ji)
	}
	return out
}

func rawString(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return ""
}

var _ = errors.New
var _ = io.EOF
