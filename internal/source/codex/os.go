package codex

import (
	"encoding/json"
	"runtime"

	"session-memory/internal/config"
)

func osName() string { return runtime.GOOS }

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func configDefault() config.Config { return config.DefaultConfig() }
