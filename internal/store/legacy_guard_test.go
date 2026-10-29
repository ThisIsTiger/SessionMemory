package store

import (
	"os"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/errcode"
)

// Before Written was durable, legacy recovery may only undo bytes that still
// equal the backup or remove a path that does not exist. It must fail closed
// for an existing created file whose intended content cannot be proved.
func TestLegacyInstallGuardWithoutWrittenHashes(t *testing.T) {
	for _, scenario := range []string{"unchanged-backup", "changed-backup", "deleted-backup", "missing-created", "unknown-created", "written-created"} {
		t.Run(scenario, func(t *testing.T) {
			home, backup := t.TempDir(), t.TempDir()
			d := InstallData{Home: home, BackupDir: backup}
			path := filepath.Join(home, "session")
			blocked := false
			write := func(path, data string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "unchanged-backup", "changed-backup", "deleted-backup":
				d.Backed = []string{"session"}
				write(filepath.Join(backup, "session"), "original")
				switch scenario {
				case "unchanged-backup":
					write(path, "original")
				case "changed-backup":
					write(path, "continued")
					blocked = true
				case "deleted-backup":
					blocked = true
				}
			default:
				d.Created = []string{"session"}
				if scenario != "missing-created" {
					write(path, "installed")
				}
				blocked = scenario == "unknown-created"
				if scenario == "written-created" {
					d.Written = []agent.PlannedWrite{{Path: "session", SHA256: sha256hex([]byte("installed"))}}
				}
			}
			err := checkInstallUntouched(d, "journal.json")
			if blocked {
				if errcode.CodeOf(err) != errcode.RecoveryNeeded {
					t.Fatalf("unsafe legacy recovery was allowed: %v", err)
				}
			} else if err != nil {
				t.Fatalf("safe legacy recovery was blocked: %v", err)
			}
		})
	}
}
