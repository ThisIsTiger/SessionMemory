// Package config loads SessionMemory's settings: built-in defaults, overlaid by
// one global file, overlaid by the SM_HOME environment variable.
//
// There is deliberately no per-project config file. Settings choose the
// directories this tool reads and writes (state, agent homes, the sync
// folder), so letting a repository's checked-in file set them would let an
// untrusted checkout redirect writes.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"session-memory/internal/platform"
)

// EnvHome overrides Config.Home. Tests and two-device simulations use it to
// run several independent "devices" on one machine.
const EnvHome = "SM_HOME"

// AgentConfig is the per-agent block: where its native data lives and how the
// adapter treats it.
type AgentConfig struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Home    string `json:"home"`
	// MaxRecordBytes caps one JSONL record read from this agent's files.
	MaxRecordBytes int64 `json:"max_record_bytes"`
	// AutoInstall lets the background sync write new revisions into this
	// agent's native store while its client is not running. Off by default.
	AutoInstall *bool `json:"auto_install,omitempty"`
}

// IsEnabled reports whether the agent is enabled (default true).
func (a AgentConfig) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// IsAutoInstall reports whether background install is enabled (default false).
func (a AgentConfig) IsAutoInstall() bool { return a.AutoInstall != nil && *a.AutoInstall }

// AgentsConfig holds one block per supported agent.
type AgentsConfig struct {
	Codex     AgentConfig `json:"codex"`
	Claude    AgentConfig `json:"claude"`
	WorkBuddy AgentConfig `json:"workbuddy"`
}

// TransportConfig selects where encrypted objects are exchanged.
type TransportConfig struct {
	// Folder is the local directory synchronised by the cloud client.
	Folder string `json:"folder"`
}

// LogConfig controls local log retention.
type LogConfig struct {
	RetentionDays int `json:"retention_days"`
}

// Config is the effective configuration.
type Config struct {
	// Home is the state directory (default ~/.session-memory).
	Home      string          `json:"home"`
	Agents    AgentsConfig    `json:"agents"`
	Transport TransportConfig `json:"transport"`
	Log       LogConfig       `json:"log"`
}

func boolPtr(b bool) *bool { return &b }

// Byte limits for AgentConfig.MaxRecordBytes.
const (
	defaultMaxRecordBytes = 16 << 20
	minMaxRecordBytes     = 64 << 10
	maxMaxRecordBytes     = 64 << 20
)

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		Home: "~/.session-memory",
		Agents: AgentsConfig{
			Codex:     AgentConfig{Enabled: boolPtr(true), Home: "~/.codex", MaxRecordBytes: defaultMaxRecordBytes},
			Claude:    AgentConfig{Enabled: boolPtr(true), Home: "~/.claude", MaxRecordBytes: defaultMaxRecordBytes},
			WorkBuddy: AgentConfig{Enabled: boolPtr(true), Home: "~/.workbuddy", MaxRecordBytes: defaultMaxRecordBytes},
		},
		Log: LogConfig{RetentionDays: 30},
	}
}

// ---- Paths and loading ----

// GlobalConfigPath is the single config file location.
func GlobalConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "session-memory", "config.json")
}

// loadFile overlays path onto cfg. A missing file is not an error. Unknown
// keys are rejected so a typo (or a stale key from an older version) is not
// silently ignored.
func loadFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading config %s: %w", filepath.Base(path), err)
	}
	if err := decodeStrict(data, cfg); err != nil {
		return fmt.Errorf("parsing config %s: %w", filepath.Base(path), err)
	}
	return nil
}

func decodeStrict(data []byte, cfg *Config) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(cfg)
}

// Load returns defaults, overlaid by the global file, overlaid by SM_HOME, and
// validates the result.
func Load() (Config, error) {
	cfg := DefaultConfig()
	if err := loadFile(GlobalConfigPath(), &cfg); err != nil {
		return cfg, err
	}
	if v := os.Getenv(EnvHome); v != "" {
		cfg.Home = v
	}
	if err := Validate(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ExpandPath expands a leading ~ to the user home directory.
func ExpandPath(path string) string {
	if path == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

// StateDir returns the expanded state directory.
func (c Config) StateDir() string {
	if c.Home == "" {
		return ExpandPath(DefaultConfig().Home)
	}
	return ExpandPath(c.Home)
}

// CodexHome returns the expanded Codex home directory.
func (c Config) CodexHome() string { return expandOr(c.Agents.Codex.Home, "~/.codex") }

// WorkBuddyHome returns the expanded WorkBuddy home directory.
func (c Config) WorkBuddyHome() string { return expandOr(c.Agents.WorkBuddy.Home, "~/.workbuddy") }

// ClaudeHome returns the expanded Claude Code configuration and project
// directory. An explicit non-default setting wins over CLAUDE_CONFIG_DIR.
func (c Config) ClaudeHome() string {
	if h := c.Agents.Claude.Home; h != "" && h != "~/.claude" {
		return ExpandPath(h)
	}
	if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
		return env
	}
	return expandOr(c.Agents.Claude.Home, "~/.claude")
}

func expandOr(v, fallback string) string {
	if v == "" {
		v = fallback
	}
	return ExpandPath(v)
}

// ---- Validation ----

// Validate checks value ranges and that configured paths are absolute.
func Validate(cfg Config) error {
	for _, a := range []struct {
		key string
		cfg AgentConfig
	}{
		{"agents.codex", cfg.Agents.Codex},
		{"agents.claude", cfg.Agents.Claude},
		{"agents.workbuddy", cfg.Agents.WorkBuddy},
	} {
		if v := a.cfg.MaxRecordBytes; v != 0 && (v < minMaxRecordBytes || v > maxMaxRecordBytes) {
			return fmt.Errorf("%s.max_record_bytes: %d out of range [%d, %d]", a.key, v, minMaxRecordBytes, maxMaxRecordBytes)
		}
		if err := requireAbs(a.key+".home", a.cfg.Home); err != nil {
			return err
		}
	}
	if err := requireAbs("home", cfg.Home); err != nil {
		return err
	}
	if err := requireAbs("transport.folder", cfg.Transport.Folder); err != nil {
		return err
	}
	if cfg.Log.RetentionDays < 0 {
		return fmt.Errorf("log.retention_days: %d must not be negative", cfg.Log.RetentionDays)
	}
	return nil
}

func requireAbs(key, val string) error {
	if val == "" {
		return nil
	}
	if !filepath.IsAbs(ExpandPath(val)) {
		return fmt.Errorf("%s: %q is not an absolute path", key, val)
	}
	return nil
}

// ---- Keys, Set and sources ----

type valueKind int

const (
	kindString valueKind = iota
	kindPath
	kindInt
	kindBool
)

type keySpec struct {
	name string
	kind valueKind
	get  func(Config) string
}

func agentKeys(prefix string, sel func(*Config) *AgentConfig) []keySpec {
	return []keySpec{
		{prefix + ".enabled", kindBool, func(c Config) string { return boolStr(sel(&c).Enabled, true) }},
		{prefix + ".home", kindPath, func(c Config) string { return sel(&c).Home }},
		{prefix + ".max_record_bytes", kindInt, func(c Config) string { return strconv.FormatInt(sel(&c).MaxRecordBytes, 10) }},
		{prefix + ".auto_install", kindBool, func(c Config) string { return boolStr(sel(&c).AutoInstall, false) }},
	}
}

// keys is the single source of truth for settable keys: their type (which
// drives parsing in Set) and how to read the effective value (config show).
var keys = func() []keySpec {
	ks := []keySpec{{"home", kindPath, func(c Config) string { return c.Home }}}
	ks = append(ks, agentKeys("agents.codex", func(c *Config) *AgentConfig { return &c.Agents.Codex })...)
	ks = append(ks, agentKeys("agents.claude", func(c *Config) *AgentConfig { return &c.Agents.Claude })...)
	ks = append(ks, agentKeys("agents.workbuddy", func(c *Config) *AgentConfig { return &c.Agents.WorkBuddy })...)
	ks = append(ks,
		keySpec{"transport.folder", kindPath, func(c Config) string { return c.Transport.Folder }},
		keySpec{"log.retention_days", kindInt, func(c Config) string { return strconv.Itoa(c.Log.RetentionDays) }},
	)
	return ks
}()

func findKey(name string) (keySpec, bool) {
	for _, k := range keys {
		if k.name == name {
			return k, true
		}
	}
	return keySpec{}, false
}

// ValidKeys lists every settable key.
func ValidKeys() []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.name
	}
	return out
}

// IsValidKey reports whether key is a known config key.
func IsValidKey(key string) bool {
	_, ok := findKey(key)
	return ok
}

// Set writes one key into the global config file, preserving unrelated
// fields. The merged result is validated before anything is written.
func Set(key, value string) error {
	spec, ok := findKey(key)
	if !ok {
		return fmt.Errorf("unknown key: %s\nvalid keys: %s", key, strings.Join(ValidKeys(), ", "))
	}
	typed, err := parseValue(spec, value)
	if err != nil {
		return err
	}

	path := GlobalConfigPath()
	raw := make(map[string]interface{})
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parsing config %s: %w", filepath.Base(path), err)
		}
	}
	setNested(raw, strings.Split(key, "."), typed)

	merged, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	check := DefaultConfig()
	if err := decodeStrict(merged, &check); err != nil {
		return fmt.Errorf("validating config: %w", err)
	}
	if err := Validate(check); err != nil {
		return err
	}

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return platform.AtomicWrite(path, append(data, '\n'), 0600)
}

func parseValue(spec keySpec, value string) (interface{}, error) {
	switch spec.kind {
	case kindInt:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: must be an integer", spec.name)
		}
		return n, nil
	case kindBool:
		switch strings.ToLower(value) {
		case "true", "1":
			return true, nil
		case "false", "0":
			return false, nil
		}
		return nil, fmt.Errorf("%s: must be true or false", spec.name)
	case kindPath:
		if value != "" && !strings.HasPrefix(value, "~") && !filepath.IsAbs(value) {
			abs, err := filepath.Abs(value)
			if err != nil {
				return nil, fmt.Errorf("%s: cannot make %q absolute: %v", spec.name, value, err)
			}
			return abs, nil
		}
	}
	return value, nil
}

func setNested(m map[string]interface{}, path []string, v interface{}) {
	for _, p := range path[:len(path)-1] {
		next, _ := m[p].(map[string]interface{})
		if next == nil {
			next = make(map[string]interface{})
			m[p] = next
		}
		m = next
	}
	m[path[len(path)-1]] = v
}

// ConfigSource is one key's effective value and the layer that set it.
type ConfigSource struct {
	Key    string
	Value  string
	Source string // default | global | env
}

// LoadSources returns each key's effective value and which layer set it.
func LoadSources() ([]ConfigSource, error) {
	def := DefaultConfig()
	global := DefaultConfig()
	if err := loadFile(GlobalConfigPath(), &global); err != nil {
		return nil, err
	}
	effective := global
	envHome := os.Getenv(EnvHome)
	if envHome != "" {
		effective.Home = envHome
	}

	out := make([]ConfigSource, 0, len(keys))
	for _, k := range keys {
		src, val := "default", k.get(def)
		if g := k.get(global); g != val {
			src, val = "global", g
		}
		if k.name == "home" && envHome != "" {
			src, val = "env", k.get(effective)
		}
		out = append(out, ConfigSource{Key: k.name, Value: val, Source: src})
	}
	return out, nil
}

func boolStr(b *bool, def bool) string {
	if b == nil {
		return strconv.FormatBool(def)
	}
	return strconv.FormatBool(*b)
}
