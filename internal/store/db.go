package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"session-memory/internal/canonjson"
	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/vault"
)

func (s *FS) dbPath() string { return s.path("store", "sessions.db") }

// openDB opens sessions.db, dropping and recreating it when user_version does
// not match (it is a rebuildable view, spec §5), then refilling it.
func (s *FS) openDB() error {
	fresh := false
	for attempt := 0; attempt < 2; attempt++ {
		dsn := "file:" + filepath.ToSlash(s.dbPath()) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		var ver int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
			db.Close()
			return err
		}
		switch {
		case ver == DBSchemaVersion:
			s.db = db
			if fresh {
				return s.refill(context.Background())
			}
			return nil
		case ver == 0:
			var n int
			db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n)
			if n == 0 {
				if _, err := db.Exec(Schema); err != nil {
					db.Close()
					return err
				}
				if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, DBSchemaVersion)); err != nil {
					db.Close()
					return err
				}
				s.db = db
				return s.refill(context.Background())
			}
		}
		db.Close()
		for _, suf := range []string{"", "-wal", "-shm"} {
			os.Remove(s.dbPath() + suf)
		}
		fresh = true
	}
	return errors.New("store: cannot open sessions.db")
}

// Rebuild drops sessions.db and refills it from revisions, manifests and bindings.
func (s *FS) Rebuild(ctx context.Context) error {
	s.db.Close()
	for _, suf := range []string{"", "-wal", "-shm"} {
		os.Remove(s.dbPath() + suf)
	}
	return s.openDB()
}

// refill replays the sources of truth into an empty database.
func (s *FS) refill(ctx context.Context) error {
	revs, _ := filepath.Glob(s.path("store", "revisions", "*.json"))
	sort.Strings(revs)
	for _, p := range revs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r session.Revision
		if json.Unmarshal(b, &r) == nil && r.Validate() == nil {
			s.insertRevision(r)
		}
	}
	devs, _ := filepath.Glob(s.path("store", "manifests", "*"))
	for _, d := range devs {
		files, _ := filepath.Glob(filepath.Join(d, "*.manifest"))
		sort.Strings(files)
		prev := ""
		for _, f := range files {
			m, sha, err := s.openManifestFile(filepath.Base(d), f, prev)
			if err != nil {
				break
			}
			s.ApplyManifest(*m, filepath.Base(f), sha)
			prev = sha
		}
	}
	bs, _ := filepath.Glob(s.path("bindings", "*.json"))
	for _, p := range bs {
		var b session.Binding
		if raw, err := os.ReadFile(p); err == nil && json.Unmarshal(raw, &b) == nil {
			s.mirrorBinding(b)
		}
	}
	return nil
}

// ---- revisions ----

func (s *FS) revisionPath(id string) string { return s.path("store", "revisions", id+".json") }

// CommitRevision validates a locally produced revision, requires its snapshot
// objects, persists it and indexes it.
func (s *FS) CommitRevision(t Tx, r session.Revision) error {
	if err := r.Validate(); err != nil {
		return errcode.Wrap(errcode.UnsafeRecord, "invalid revision", err)
	}
	if err := s.requireSnapshotObjects(r); err != nil {
		return err
	}
	b, err := canonjson.Marshal(r)
	if err != nil {
		return err
	}
	if err := platform.AtomicWrite(s.revisionPath(r.RevisionID), append(b, '\n'), 0600); err != nil {
		return err
	}
	return s.insertRevision(r)
}

// requireSnapshotObjects checks that the snapshot and every segment it names
// are present locally.
func (s *FS) requireSnapshotObjects(r session.Revision) error {
	snap, err := s.LoadSnapshot(context.Background(), r.NativeSnapshotRef)
	if err != nil {
		return err
	}
	for _, a := range snap.Artifacts {
		for _, sg := range a.Segments {
			if !s.HasObject(sg.Object) {
				return errcode.New(errcode.DepsMissing, "snapshot segment object missing")
			}
		}
	}
	if r.RepoStateRef != "" && !s.HasObject(r.RepoStateRef) {
		return errcode.New(errcode.DepsMissing, "repo state object missing")
	}
	return nil
}

// LoadSnapshot opens and decodes a Snapshot object.
func (s *FS) LoadSnapshot(ctx context.Context, ref string) (session.Snapshot, error) {
	rc, err := s.OpenObject(ctx, ref, vault.TypeSnapshot, nil)
	if err != nil {
		return session.Snapshot{}, err
	}
	defer rc.Close()
	var snap session.Snapshot
	if err := json.NewDecoder(rc).Decode(&snap); err != nil {
		return session.Snapshot{}, errcode.Wrap(errcode.UnsafeRecord, "snapshot is not valid", err)
	}
	return snap, nil
}

func (s *FS) insertRevision(r session.Revision) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.insertRevisionTx(tx, r); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *FS) insertRevisionTx(tx *sql.Tx, r session.Revision) error {
	canon, err := canonjson.Marshal(r)
	if err != nil {
		return err
	}
	status := "committed"
	if !s.HasObject(r.NativeSnapshotRef) {
		status = "pending_objects"
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO sessions(session_id, origin_agent, origin_native_id, project_ref, created_at) VALUES(?,?,?,?,?)`,
		r.SessionID, r.AgentProfile.Agent, r.OriginNativeID, nullable(r.ProjectRef), r.CreatedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO revisions(revision_id, session_id, parent_revision_id, producer_device_id, agent, snapshot_ref, repo_state_ref, capture_state, created_at, status, json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, r.RevisionID, r.SessionID, nullable(r.ParentRevisionID), r.ProducerDeviceID, r.AgentProfile.Agent,
		r.NativeSnapshotRef, nullable(r.RepoStateRef), r.CaptureState, r.CreatedAt, status, string(canon)); err != nil {
		return err
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Revisions returns the committed revisions of a session.
func (s *FS) Revisions(sessionID string) ([]session.Revision, error) {
	rows, err := s.db.Query(`SELECT json FROM revisions WHERE session_id=? AND status='committed' ORDER BY created_at, revision_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.Revision
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		var r session.Revision
		if json.Unmarshal([]byte(j), &r) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

func (s *FS) LoadRevision(id string) (session.Revision, error) {
	var r session.Revision
	var raw string
	if err := s.db.QueryRow(`SELECT json FROM revisions WHERE revision_id=? AND status='committed'`, id).Scan(&raw); err != nil {
		return r, err
	}
	return r, json.Unmarshal([]byte(raw), &r)
}

// SessionIDs includes received sessions even before the first local restore.
func (s *FS) SessionIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT session_id FROM sessions ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Graph builds the revision graph of a session.
func (s *FS) Graph(sessionID string) (*session.Graph, error) {
	revs, err := s.Revisions(sessionID)
	if err != nil {
		return nil, err
	}
	return session.NewGraph(revs), nil
}

// Choices returns the recorded continuation choices of a session.
func (s *FS) Choices(sessionID string) ([]session.Choice, error) {
	rows, err := s.db.Query(`SELECT session_id, chosen, seen_heads, device_id, at FROM choices WHERE session_id=?`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.Choice
	for rows.Next() {
		var c session.Choice
		var seen string
		if err := rows.Scan(&c.SessionID, &c.Chosen, &seen, &c.DeviceID, &c.At); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(seen), &c.SeenHeads)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UnpublishedRevisions are this device's committed revisions that no applied
// manifest entry mentions yet, oldest first.
func (s *FS) UnpublishedRevisions(deviceID string) ([]session.Revision, error) {
	rows, err := s.db.Query(`SELECT json FROM revisions r WHERE producer_device_id=? AND status='committed'
		AND NOT EXISTS (SELECT 1 FROM entries e WHERE e.entry_key = 'revision:' || r.revision_id) ORDER BY created_at, revision_id`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.Revision
	for rows.Next() {
		var j string
		rows.Scan(&j)
		var r session.Revision
		if json.Unmarshal([]byte(j), &r) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// ---- manifests ----

// ApplyManifest records a verified manifest and its entries. Entries are
// deduplicated by Entry.Key; revisions whose snapshot object is missing are
// stored as pending_objects.
func (s *FS) ApplyManifest(m session.Manifest, fileName, fileSHA string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO manifests(device_id, sequence, file_name, file_sha256, prev_hash, status, seen_at)
		VALUES(?,?,?,?,?,'applied',?)`, m.DeviceID, m.Sequence, fileName, fileSHA, nullable(m.PrevManifestHash), session.FormatTime(s.Now())); err != nil {
		return err
	}
	for _, e := range m.Entries {
		key := e.Key()
		if err := e.Validate(); err != nil {
			return err
		}
		j, err := canonjson.Marshal(e)
		if err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO entries(entry_key, type, device_id, sequence, json) VALUES(?,?,?,?,?)`, key, e.Type, m.DeviceID, m.Sequence, string(j))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		switch {
		case e.Revision != nil:
			if err := s.insertRevisionTx(tx, e.Revision.Revision); err != nil {
				return err
			}
		case e.Receipt != nil:
			r := e.Receipt
			_, err = tx.Exec(`INSERT OR IGNORE INTO receipts(session_id, revision_id, device_id, status, install_mode, at) VALUES(?,?,?,?,?,?)`,
				r.SessionID, r.RevisionID, r.DeviceID, r.Status, nullable(r.InstallMode), r.At)
		case e.Choice != nil:
			c := e.Choice
			seen, _ := json.Marshal(c.SeenHeads)
			_, err = tx.Exec(`INSERT OR IGNORE INTO choices(entry_key, session_id, chosen, seen_heads, device_id, at) VALUES(?,?,?,?,?,?)`,
				key, c.SessionID, c.Chosen, string(seen), c.DeviceID, c.At)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// manifestAADFor reconstructs the AAD of a stored manifest.
func manifestAADFor(device string, seq int64, prev string) []byte {
	return vault.ManifestAAD(device, seq, prev)
}

// openManifestFile decrypts a stored manifest whose predecessor hash is prev.
func (s *FS) openManifestFile(device, path, prev string) (*session.Manifest, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var seq int64
	if _, err := fmt.Sscanf(filepath.Base(path), "%012d-", &seq); err != nil {
		return nil, "", err
	}
	plain, err := s.sealer.Open(vault.TypeManifest, manifestAADFor(device, seq, prev), b)
	if err != nil {
		return nil, "", err
	}
	var m session.Manifest
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, "", err
	}
	return &m, sha256hex(b), nil
}

// ---- bindings ----

func (s *FS) bindingPath(id string) string { return s.path("bindings", id+".json") }

// LoadBinding reads a binding file. A damaged file is renamed aside and
// reported as recovery_needed (spec §3.1).
func (s *FS) LoadBinding(id string) (session.Binding, error) {
	var b session.Binding
	if !session.ValidBindingID(id) {
		return b, errcode.New(errcode.NotFound, "bad binding id")
	}
	raw, err := os.ReadFile(s.bindingPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return b, errcode.New(errcode.NotFound, "binding not found")
	}
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		os.Rename(s.bindingPath(id), s.bindingPath(id)+".corrupt-"+strings.ReplaceAll(session.FormatTime(s.Now()), ":", ""))
		return b, errcode.Wrap(errcode.RecoveryNeeded, "binding file is damaged", err)
	}
	if b.SchemaVersion > session.SchemaVersion {
		return b, errcode.New(errcode.UnknownFormat, "binding written by a newer version")
	}
	return b, nil
}

// SaveBinding writes the binding file atomically and mirrors it into the view.
func (s *FS) SaveBinding(t Tx, b session.Binding) error {
	b.SchemaVersion = session.SchemaVersion
	b.UpdatedAt = session.FormatTime(s.Now())
	if b.CreatedAt == "" {
		b.CreatedAt = b.UpdatedAt
	}
	if b.Artifacts == nil {
		b.Artifacts = map[string]session.ArtifactState{}
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	if err := platform.AtomicWrite(s.bindingPath(b.BindingID), append(raw, '\n'), 0600); err != nil {
		return err
	}
	return s.mirrorBinding(b)
}

func (s *FS) mirrorBinding(b session.Binding) error {
	_, err := s.db.Exec(`INSERT INTO bindings_view(binding_id, session_id, agent, native_session_id, install_mode, state, installed_revision_id, updated_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(binding_id) DO UPDATE SET session_id=excluded.session_id, agent=excluded.agent,
		native_session_id=excluded.native_session_id, install_mode=excluded.install_mode, state=excluded.state,
		installed_revision_id=excluded.installed_revision_id, updated_at=excluded.updated_at`, b.BindingID, b.SessionID, b.AgentProfile.Agent, b.NativeSessionID, b.InstallMode, b.State, nullable(b.InstalledRevisionID), b.UpdatedAt)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errcode.New(errcode.Conflict, "another active binding exists for this session and agent")
	}
	return err
}

// Bindings lists the bindings of a session ("" = all).
func (s *FS) Bindings(sessionID string) ([]session.Binding, error) {
	files, _ := filepath.Glob(s.path("bindings", "*.json"))
	sort.Strings(files)
	var out []session.Binding
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		b, err := s.LoadBinding(id)
		if err != nil {
			return nil, err
		}
		if sessionID == "" || b.SessionID == sessionID {
			out = append(out, b)
		}
	}
	return out, nil
}

// DB exposes the view database to the sync engine (read/quarantine bookkeeping).
func (s *FS) DB() *sql.DB { return s.db }
