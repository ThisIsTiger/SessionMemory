package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// A rebuild that stopped after the tables were created but before it finished
// leaves no version behind; the next open must start over, not trust the
// half-filled view.
func TestOpenRebuildsAnUnfinishedIndex(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, suf := range []string{"", "-wal", "-shm"} {
		os.Remove(filepath.Join(root, "store", "sessions.db"+suf))
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(root, "store", "sessions.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(Schema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE leftover (x)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err = Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n, ver int
	s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='leftover'`).Scan(&n)
	s.db.QueryRow(`PRAGMA user_version`).Scan(&ver)
	if n != 0 || ver != DBSchemaVersion {
		t.Fatalf("unfinished index was kept: leftover=%d version=%d", n, ver)
	}
}
