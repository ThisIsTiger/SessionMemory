package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Use only a local HTTP fixture and a harmless script as the downloaded
// executable. Bad downloads must leave an existing installation intact.
func TestInstallerChecksDownloadBeforeReplacingBinary(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the shell installer supports macOS and Linux")
	}
	for _, scenario := range []string{"valid", "setup", "bad-checksum", "missing-checksum", "download-failure"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			dest := filepath.Join(home, "bin with space")
			if err := os.MkdirAll(dest, 0700); err != nil {
				t.Fatal(err)
			}
			installed := filepath.Join(dest, "session-memory")
			const previous = "existing-installation"
			if err := os.WriteFile(installed, []byte(previous), 0755); err != nil {
				t.Fatal(err)
			}
			const binary = "#!/bin/sh\nprintf 'installer-test:%s\\n' \"$1\"\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/checksums.txt" {
					if scenario == "missing-checksum" {
						fmt.Fprintln(w, "unrelated-file")
						return
					}
					content := binary
					if scenario == "bad-checksum" {
						content = "wrong"
					}
					sum := sha256.Sum256([]byte(content))
					// List every supported name; the script selects the host's name.
					for _, osName := range []string{"darwin", "linux"} {
						for _, arch := range []string{"arm64", "amd64"} {
							fmt.Fprintf(w, "%x  session-memory-%s-%s\n", sum, osName, arch)
						}
					}
					return
				}
				if scenario == "download-failure" {
					http.NotFound(w, r)
					return
				}
				io.WriteString(w, binary)
			}))
			defer server.Close()
			cmd := osexec.Command("sh", filepath.Join("..", "..", "install.sh"))
			skipSetup := "1"
			if scenario == "setup" {
				skipSetup = ""
			}
			cmd.Env = append(os.Environ(), "HOME="+home, "SM_BASE_URL="+server.URL, "SM_BIN_DIR="+dest, "SM_NO_SETUP="+skipSetup)
			out, err := cmd.CombinedOutput()
			got, readErr := os.ReadFile(installed)
			if readErr != nil {
				t.Fatal(readErr)
			}
			good := scenario == "valid" || scenario == "setup"
			if good {
				if err != nil || string(got) != binary {
					t.Fatalf("installation: %v %s", err, out)
				}
				st, err := os.Stat(installed)
				if err != nil || st.Mode().Perm() != 0755 {
					t.Fatalf("installed permissions: %v", err)
				}
				if scenario == "setup" && !strings.Contains(string(out), "installer-test:setup") {
					t.Fatalf("installer did not run setup: %s", out)
				}
			} else if err == nil || string(got) != previous {
				t.Fatalf("bad download changed the existing installation: %v %q %s", err, got, out)
			}
		})
	}
}
