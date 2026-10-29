package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"session-memory/internal/config"
	"session-memory/internal/errcode"
	"session-memory/internal/logx"
	"session-memory/internal/vault"
)

// App carries what commands need; tests construct it directly.
type App struct {
	Cfg  config.Config
	In   io.Reader
	Out  io.Writer
	Err  io.Writer
	JSON bool
	Now  func() time.Time
	Log  *slog.Logger
	Keys vault.KeyStore
	// quietRun is set by a background run (relay --quiet): it leaves a Weiyun
	// account alone while that account's quota is known to be used up.
	quietRun bool
	// jsonPrinted is set once a JSON document went to stdout: a later failure
	// is then reported on stderr, so stdout never holds two documents.
	jsonPrinted bool
}

// printJSON writes exactly one JSON document.
func (a *App) printJSON(v any) error {
	a.jsonPrinted = true
	enc := json.NewEncoder(a.Out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// run executes the CLI and returns the process exit code.
func run(args []string, in io.Reader, out, errW io.Writer) int {
	app := &App{In: in, Out: out, Err: errW, Now: time.Now, Keys: newKeyStore()}
	root := newRoot(app)
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errW)
	if err := root.Execute(); err != nil {
		return app.fail(err)
	}
	return 0
}

// fail prints err per spec §12.1 and returns its exit code.
func (a *App) fail(err error) int {
	var e *errcode.Error
	if !errors.As(err, &e) {
		// cobra's own errors (unknown flag, bad args) are usage errors
		e = errcode.Wrap(errcode.Usage, err.Error(), err)
		if !isUsage(err) {
			e = errcode.Wrap(errcode.Internal, err.Error(), err)
		}
	}
	if a.JSON && !a.jsonPrinted {
		doc := map[string]any{"error": map[string]any{"code": e.Code, "message": e.Message}}
		if len(e.Details) > 0 {
			doc["error"].(map[string]any)["details"] = e.Details
		}
		a.printJSON(doc)
	} else {
		fmt.Fprintf(a.Err, "error: %s: %s\n", e.Code, e.Message)
	}
	return errcode.ExitCode(e.Code)
}

func isUsage(err error) bool {
	s := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand", "accepts ", "requires at least", "invalid argument", "flag needs", "required flag"} {
		if len(s) >= len(p) && contains(s, p) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// setup applies global flags: --home, config load, logger.
func (a *App) setup(cmd *cobra.Command) error {
	if h, _ := cmd.Flags().GetString("home"); h != "" {
		os.Setenv(config.EnvHome, h)
	}
	a.JSON, _ = cmd.Flags().GetBool("json")
	cfg, err := config.Load()
	if err != nil {
		return errcode.Wrap(errcode.Usage, "invalid configuration: "+err.Error(), err)
	}
	a.Cfg = cfg
	level := slog.LevelError
	if v, _ := cmd.Flags().GetBool("verbose"); v {
		level = slog.LevelDebug
	}
	a.Log = logx.New(a.Err, level)
	return nil
}
