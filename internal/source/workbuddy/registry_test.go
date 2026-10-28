package workbuddy

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

func schemaDB(t *testing.T, sqlText string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/x.db")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(sqlText); err != nil {
		t.Fatal(err)
	}
	return db
}

func readSchema(t *testing.T, name string) string {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The same 40 columns come in different orders depending on a database's
// migration history (the isolated fixture vs. real installs on two computers);
// the writer works by column name, so only the set of columns is pinned.
func TestRegistrySchemaIgnoresColumnOrderButNotContent(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"registry-5.8.3.sql", "registry-5.8.3-migrated.sql"} {
		if _, err := registryDefaults(ctx, schemaDB(t, readSchema(t, name))); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	base := readSchema(t, "registry-5.8.3.sql")
	for label, mutated := range map[string]string{
		"extra column":    strings.Replace(base, "unread INTEGER NOT NULL DEFAULT 0", "unread INTEGER NOT NULL DEFAULT 0, surprise TEXT", 1),
		"changed type":    strings.Replace(base, "title TEXT,", "title INTEGER,", 1),
		"changed notnull": strings.Replace(base, "cwd TEXT NOT NULL", "cwd TEXT", 1),
		"changed default": strings.Replace(base, "DEFAULT 'Pending'", "DEFAULT 'Idle'", 1),
	} {
		if mutated == base {
			t.Fatalf("%s: the mutation did not apply", label)
		}
		if _, err := registryDefaults(ctx, schemaDB(t, mutated)); err == nil || !strings.Contains(err.Error(), "outside the allowlist") {
			t.Errorf("%s: want an allowlist rejection, got %v", label, err)
		}
	}
}
