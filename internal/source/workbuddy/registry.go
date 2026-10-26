package workbuddy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"session-memory/internal/errcode"
	"session-memory/internal/store"
)

// Exact PRAGMA table_info fingerprint from the isolated 5.8.3 experiment.
const registrySchema = "884179348b27ff0de44059fc1483fb289a6edf29f5ae5a310428495a21eb928f"
const registryBackupName = "workbuddy-registry.json"

type row map[string]any
type registryBackup struct {
	SchemaVersion int
	NativeID      string
	Before        row
	After         row
	Files         map[string]string
	BeforeFiles   map[string]fileBaseline
}

func openRegistry(home string, writable bool) (*sql.DB, error) {
	path, err := safeTarget(home, "workbuddy.db")
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, errcode.New(errcode.NeedsAction, "open WorkBuddy and create a local session before restoring")
	}
	mode := "ro"
	if writable {
		mode = "rw"
	}
	db, err := sql.Open("sqlite", sqliteURI(path, mode))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=1000"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type sqlReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func registryDefaults(ctx context.Context, db sqlReader) (row, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		return nil, err
	}
	var signature []string
	defaults := row{}
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &nn, &def, &pk); err != nil {
			rows.Close()
			return nil, err
		}
		d := "NULL"
		var value any
		if def.Valid {
			d = def.String
			if strings.HasPrefix(d, "'") && strings.HasSuffix(d, "'") {
				value = strings.Trim(d, "'")
			} else {
				value, err = strconv.ParseInt(d, 10, 64)
				if err != nil {
					rows.Close()
					return nil, errcode.New(errcode.UnknownFormat, "unknown WorkBuddy registry default")
				}
			}
		}
		signature = append(signature, fmt.Sprintf("%s:%s:%d:%s:%d", name, typ, nn, d, pk))
		defaults[name] = value
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if hashBytes([]byte(strings.Join(signature, "\n"))) != registrySchema {
		return nil, errcode.New(errcode.UnknownFormat, "WorkBuddy registry schema is outside the allowlist")
	}
	var triggers int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='trigger' AND tbl_name='sessions'").Scan(&triggers); err != nil {
		return nil, err
	}
	if triggers != 0 {
		return nil, errcode.New(errcode.UnknownFormat, "unexpected WorkBuddy session triggers")
	}
	return defaults, nil
}

func readRow(ctx context.Context, db sqlReader, id string) (row, error) {
	rows, err := db.QueryContext(ctx, "SELECT * FROM sessions WHERE id=?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, rows.Err()
	}
	values := make([]any, len(names))
	ptrs := make([]any, len(names))
	for i := range ptrs {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := row{}
	for i, name := range names {
		switch value := values[i].(type) {
		case nil, string, int64:
			out[name] = value
		default:
			return nil, errcode.New(errcode.UnknownFormat, "unsupported WorkBuddy registry value")
		}
	}
	return out, rows.Err()
}

func localUser(ctx context.Context, db sqlReader) (string, error) {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT user_id FROM sessions WHERE deleted_at IS NULL AND user_id<>''")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", errcode.New(errcode.NeedsAction, "WorkBuddy target must have exactly one active local account; use a separate profile")
	}
	return ids[0], nil
}

func rowHash(r row) string {
	b, _ := json.Marshal(r) // SQLite NULL is meaningful; transport canonjson forbids it.
	return hashBytes(b)
}

func writeRow(ctx context.Context, db *sql.Tx, r row) error {
	var quoted, marks []string
	var values []any
	for _, k := range sortedKeys(r) {
		quoted = append(quoted, "\""+k+"\"")
		marks = append(marks, "?")
		values = append(values, r[k])
	}
	_, err := db.ExecContext(ctx, "INSERT INTO sessions ("+strings.Join(quoted, ",")+") VALUES ("+strings.Join(marks, ",")+")", values...)
	return err
}

func swapRow(ctx context.Context, home, id string, before, after row) error {
	db, err := openRegistry(home, true)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	defaults, err := registryDefaults(ctx, tx)
	if err != nil {
		return err
	}
	for _, r := range []row{before, after} {
		if r == nil {
			continue
		}
		if len(r) != len(defaults) || r["id"] != id {
			return errcode.New(errcode.RecoveryNeeded, "invalid registry row backup")
		}
		for key := range r {
			if _, ok := defaults[key]; !ok {
				return errcode.New(errcode.RecoveryNeeded, "unknown registry backup column")
			}
		}
	}
	// Reserve the SQLite writer before reading for CAS.
	if _, err := tx.ExecContext(ctx, "UPDATE sessions SET id=id WHERE id=?", id); err != nil {
		return err
	}
	current, err := readRow(ctx, tx, id)
	if err != nil {
		return err
	}
	if rowHash(current) == rowHash(after) {
		return nil
	}
	if rowHash(current) != rowHash(before) {
		return errcode.New(errcode.BaselineChanged, "WorkBuddy registry row changed")
	}
	if before != nil && after != nil {
		var sets []string
		var values []any
		for _, key := range sortedKeys(after) {
			if key == "id" {
				continue
			}
			sets = append(sets, "\""+key+"\"=?")
			values = append(values, after[key])
		}
		values = append(values, id)
		_, err = tx.ExecContext(ctx, "UPDATE sessions SET "+strings.Join(sets, ",")+" WHERE id=?", values...)
	} else if after == nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", id)
	} else {
		err = writeRow(ctx, tx, after)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// UndoRegistry restores one row, never the entire vendor database.
func UndoRegistry(ctx context.Context, d store.InstallData) error {
	if d.Agent != AgentName || d.RegistryUndo != registryBackupName {
		return errcode.New(errcode.RecoveryNeeded, "unknown registry rollback format")
	}
	b, err := os.ReadFile(filepath.Join(d.BackupDir, registryBackupName))
	if err != nil {
		return err
	}
	var undo registryBackup
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&undo); err != nil {
		return err
	}
	if undo.SchemaVersion != 1 || undo.NativeID != d.NativeSessionID {
		return errcode.New(errcode.RecoveryNeeded, "invalid registry backup")
	}
	for _, r := range []row{undo.Before, undo.After} {
		for k, value := range r {
			if number, ok := value.(json.Number); ok {
				n, err := number.Int64()
				if err != nil {
					return err
				}
				r[k] = n
			}
		}
	}
	// Refuse to overwrite files a concurrently started client has advanced.
	if native := scan(d.Home)[undo.NativeID]; native != nil {
		for _, file := range native.files {
			if _, known := undo.Files[file.rel]; !known {
				return errcode.New(errcode.RecoveryNeeded, "WorkBuddy gained native artifacts after the interrupted install")
			}
		}
	}
	for rel, afterHash := range undo.Files {
		path, err := safeTarget(d.Home, rel)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			if _, existed := undo.BeforeFiles[rel]; existed {
				return errcode.New(errcode.RecoveryNeeded, "WorkBuddy target file was deleted after backup")
			}
			for _, backed := range d.Backed {
				if rel == backed {
					return errcode.New(errcode.RecoveryNeeded, "WorkBuddy target file was deleted after backup")
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		before, berr := os.ReadFile(filepath.Join(d.BackupDir, filepath.FromSlash(rel)))
		if hashBytes(content) != afterHash && (berr != nil || !bytes.Equal(content, before)) {
			return errcode.New(errcode.RecoveryNeeded, "WorkBuddy files advanced after the interrupted install")
		}
	}
	for _, rel := range append(append([]string{}, d.Backed...), d.Created...) {
		if _, err := safeTarget(d.Home, rel); err != nil {
			return err
		}
	}
	return swapRow(ctx, d.Home, undo.NativeID, undo.After, undo.Before)
}
