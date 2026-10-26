package store

// DBSchemaVersion is stored in PRAGMA user_version. sessions.db is a rebuildable
// view, so a mismatch (older or newer) is handled by dropping the file and
// calling Store.Rebuild, never by migrating.
const DBSchemaVersion = 1

// Schema is the DDL of store/sessions.db. All IDs are the strings of
// internal/session. All times are session.TimeLayout strings.
const Schema = `
CREATE TABLE sessions (
  session_id        TEXT PRIMARY KEY,
  origin_agent      TEXT NOT NULL,
  origin_native_id  TEXT NOT NULL,
  project_ref       TEXT,
  created_at        TEXT NOT NULL
);

CREATE TABLE revisions (
  revision_id        TEXT PRIMARY KEY,
  session_id         TEXT NOT NULL REFERENCES sessions(session_id),
  parent_revision_id TEXT,
  producer_device_id TEXT NOT NULL,
  agent              TEXT NOT NULL,
  snapshot_ref       TEXT NOT NULL,
  repo_state_ref     TEXT,
  capture_state      TEXT NOT NULL,
  created_at         TEXT NOT NULL,
  status             TEXT NOT NULL CHECK (status IN ('committed','pending_parent','pending_objects')),
  json               TEXT NOT NULL            -- canonical Revision JSON
);
CREATE INDEX revisions_session ON revisions(session_id, created_at);
CREATE INDEX revisions_parent  ON revisions(parent_revision_id);

CREATE TABLE objects (
  ref          TEXT PRIMARY KEY,
  object_type  INTEGER NOT NULL,
  sealed_size  INTEGER NOT NULL,
  added_at     TEXT NOT NULL
);

CREATE TABLE revision_objects (
  revision_id TEXT NOT NULL REFERENCES revisions(revision_id),
  ref         TEXT NOT NULL,
  PRIMARY KEY (revision_id, ref)
);

CREATE TABLE manifests (
  device_id    TEXT NOT NULL,
  sequence     INTEGER NOT NULL,
  file_name    TEXT NOT NULL,
  file_sha256  TEXT NOT NULL,
  prev_hash    TEXT,
  status       TEXT NOT NULL CHECK (status IN ('applied','pending_prev','pending_objects','quarantined')),
  seen_at      TEXT NOT NULL,
  PRIMARY KEY (device_id, sequence)
);

CREATE TABLE entries (            -- applied manifest entries, deduplicated by key
  entry_key  TEXT PRIMARY KEY,
  type       TEXT NOT NULL,
  device_id  TEXT NOT NULL,
  sequence   INTEGER NOT NULL,
  json       TEXT NOT NULL
);

CREATE TABLE receipts (
  session_id  TEXT NOT NULL,
  revision_id TEXT NOT NULL,
  device_id   TEXT NOT NULL,
  status      TEXT NOT NULL,
  install_mode TEXT,
  at          TEXT NOT NULL,
  PRIMARY KEY (device_id, revision_id, status)
);

CREATE TABLE choices (
  entry_key   TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL,
  chosen      TEXT NOT NULL,
  seen_heads  TEXT NOT NULL,       -- JSON array
  device_id   TEXT NOT NULL,
  at          TEXT NOT NULL
);

CREATE TABLE bindings_view (        -- mirror of bindings/*.json
  binding_id            TEXT PRIMARY KEY,
  session_id            TEXT NOT NULL,
  agent                 TEXT NOT NULL,
  native_session_id     TEXT NOT NULL,
  install_mode          TEXT NOT NULL,
  state                 TEXT NOT NULL,
  installed_revision_id TEXT,
  updated_at            TEXT NOT NULL
);
CREATE UNIQUE INDEX one_active_binding
  ON bindings_view(session_id, agent) WHERE state = 'active';

CREATE TABLE quarantine (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  path      TEXT NOT NULL,
  reason    TEXT NOT NULL,          -- an errcode.Code
  at        TEXT NOT NULL
);

CREATE TABLE canonical_cache (      -- derived; safe to delete at any time
  revision_id     TEXT NOT NULL,
  adapter_version TEXT NOT NULL,
  path            TEXT NOT NULL,    -- cache file under store/cache/
  PRIMARY KEY (revision_id, adapter_version)
);
`
