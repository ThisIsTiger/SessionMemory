package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is injected with -ldflags "-X main.version=...".
var version = "dev"

type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func buildCommit() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return s.Value[:12]
			}
		}
	}
	return "unknown"
}

func newVersionCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print version information", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			v := versionInfo{Version: version, Commit: buildCommit(), Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
			if a.JSON {
				return a.printJSON(v)
			}
			_, err := fmt.Fprintf(a.Out, "session-memory %s (%s) %s %s/%s\n", v.Version, v.Commit, v.Go, v.OS, v.Arch)
			return err
		},
	}
}
