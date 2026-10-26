package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/vault"
)

// FS is the filesystem + SQLite implementation of Store.
type FS struct {
	Root   string
	sealer vault.Sealer
	db     *sql.DB
	Now    func() time.Time
	// Resumers handle journals the store cannot finish alone (publish, pull);
	// the sync package registers them.
	Resumers map[Kind]func(context.Context, *FS, JournalRecord) error
}

// Open prepares the state directory and opens (or rebuilds) sessions.db.
func Open(root string, sealer vault.Sealer) (*FS, error) {
	for _, d := range []string{"device", "projects", "bindings", "journal/done", "store/objects", "store/manifests", "store/revisions",
		"store/staging", "store/cache", "backups", "quarantine", "locks", "logs"} {
		if err := platform.EnsurePrivateDir(filepath.Join(root, d)); err != nil {
			return nil, err
		}
	}
	s := &FS{Root: root, sealer: sealer, Now: time.Now, Resumers: map[Kind]func(context.Context, *FS, JournalRecord) error{}}
	if err := s.openDB(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *FS) Close() error { return s.db.Close() }

// Sealer exposes the vault sealer (for manifest sealing in sync).
func (s *FS) Sealer() vault.Sealer { return s.sealer }

func (s *FS) path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

func (s *FS) objectPath(ref string) string {
	h := ref[len("obj_"):]
	return s.path("store", "objects", h[:2], h+".obj")
}

// ---- objects ----

// PutObject seals plaintext and stores it. The file appears under its final
// name only after fsync + rename, so a crash never leaves a partial object.
func (s *FS) PutObject(ctx context.Context, t vault.ObjectType, extra []byte, r io.Reader) (ObjectInfo, error) {
	plain, err := io.ReadAll(io.LimitReader(r, vault.MaxPlainObject+1))
	if err != nil {
		return ObjectInfo{}, err
	}
	if len(plain) > vault.MaxPlainObject {
		return ObjectInfo{}, errcode.New(errcode.UnsafeRecord, "object exceeds the maximum size")
	}
	sealed, err := s.sealer.Seal(t, extra, plain)
	if err != nil {
		return ObjectInfo{}, err
	}
	sum := sha256.Sum256(sealed)
	ref := session.ObjectRefOf(hex.EncodeToString(sum[:]))
	pp := sha256.Sum256(plain)
	info := ObjectInfo{Ref: ref, PlainSize: int64(len(plain)), PlainSHA256: hex.EncodeToString(pp[:]), SealedSize: int64(len(sealed))}
	if err := s.writeObject(ref, sealed, t); err != nil {
		return ObjectInfo{}, err
	}
	return info, nil
}

func (s *FS) writeObject(ref string, sealed []byte, t vault.ObjectType) error {
	p := s.objectPath(ref)
	if _, err := os.Stat(p); err == nil {
		return nil // content-addressed: same ref, same bytes
	}
	if err := platform.AtomicWrite(p, sealed, 0600); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO objects(ref, object_type, sealed_size, added_at) VALUES(?,?,?,?)`,
		ref, int(t), len(sealed), session.FormatTime(s.Now()))
	return err
}

// PutSealed stores an already sealed object (from the sync folder) after
// checking that its hash equals ref.
func (s *FS) PutSealed(ctx context.Context, ref string, sealed io.Reader) error {
	if !session.ValidObjectRef(ref) {
		return errcode.New(errcode.AuthFailed, "bad object ref")
	}
	b, err := io.ReadAll(sealed)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if session.ObjectRefOf(hex.EncodeToString(sum[:])) != ref {
		return errcode.New(errcode.AuthFailed, "object hash does not match its reference")
	}
	t := vault.ObjectType(0)
	if len(b) > 5 {
		t = vault.ObjectType(b[5])
	}
	return s.writeObject(ref, b, t)
}

// OpenObject verifies the file hash, then authenticates and decrypts it.
func (s *FS) OpenObject(ctx context.Context, ref string, t vault.ObjectType, extra []byte) (io.ReadCloser, error) {
	b, err := s.readSealed(ref)
	if err != nil {
		return nil, err
	}
	plain, err := s.sealer.Open(t, extra, b)
	if err != nil {
		return nil, errcode.Wrap(errcode.AuthFailed, "object failed authentication", err)
	}
	return io.NopCloser(bytes.NewReader(plain)), nil
}

func (s *FS) readSealed(ref string) ([]byte, error) {
	if !session.ValidObjectRef(ref) {
		return nil, errcode.New(errcode.NotFound, "bad object ref")
	}
	b, err := os.ReadFile(s.objectPath(ref))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errcode.New(errcode.DepsMissing, "object not present")
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if session.ObjectRefOf(hex.EncodeToString(sum[:])) != ref {
		return nil, errcode.New(errcode.AuthFailed, "stored object does not match its hash")
	}
	return b, nil
}

// SealedBytes returns the ciphertext file for upload.
func (s *FS) SealedBytes(ctx context.Context, ref string) (io.ReadCloser, error) {
	b, err := s.readSealed(ref)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// HasObject reports whether the object file exists.
func (s *FS) HasObject(ref string) bool {
	if !session.ValidObjectRef(ref) {
		return false
	}
	_, err := os.Stat(s.objectPath(ref))
	return err == nil
}

var _ = fmt.Sprintf

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
