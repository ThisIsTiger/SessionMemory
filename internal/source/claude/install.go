package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"session-memory/internal/platform"
)

// OwnershipMarker identifies entries managed by session-memory.
const OwnershipMarker = "managed-by=session-memory"

// SettingsPath returns the Claude Code user settings file.
func SettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// HookCommand builds the hook entry command for this executable.
func HookCommand(exe string) string {
	cmd := exe
	if strings.Contains(cmd, " ") && !strings.HasPrefix(cmd, "\"") {
		cmd = `"` + cmd + `"`
	}
	return cmd + " hook claude-session-end " + OwnershipMarker
}

// hookEntry is the settings.json shape for one hook entry.
type hookEntry struct {
	Matcher string     `json:"matcher,omitempty"`
	Hooks   []hookItem `json:"hooks"`
}

type hookItem struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// IsManaged reports whether a hook command belongs to session-memory.
func IsManaged(command string) bool {
	return containsSessionMemoryHook(command)
}

func containsSessionMemoryHook(cmd string) bool {
	return len(cmd) > 0 &&
		contains(cmd, "session-memory") &&
		contains(cmd, "claude-session-end")
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Install appends the session-memory SessionEnd hook without touching other
// entries. Idempotent. Returns installed=true when the entry was added.
func Install() (bool, error) {
	settings, err := readSettings()
	if err != nil {
		return false, err
	}
	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		hooks = map[string]interface{}{}
	}
	entries, _ := hooks["SessionEnd"].([]interface{})
	for _, e := range entries {
		em, _ := e.(map[string]interface{})
		if em == nil {
			continue
		}
		items, _ := em["hooks"].([]interface{})
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			if im == nil {
				continue
			}
			cmd, _ := im["command"].(string)
			if containsSessionMemoryHook(cmd) {
				return false, nil // already installed
			}
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("resolving executable: %w", err)
	}
	entries = append(entries, map[string]interface{}{
		"hooks": []interface{}{
			map[string]interface{}{"type": "command", "command": HookCommand(exe)},
		},
	})
	hooks["SessionEnd"] = entries
	settings["hooks"] = hooks
	if err := writeSettings(settings); err != nil {
		return false, err
	}
	return true, nil
}

// Uninstall removes exactly the session-memory hook entries. It refuses when
// it cannot confirm ownership (spec 11.2).
func Uninstall() (bool, error) {
	settings, err := readSettings()
	if err != nil {
		return false, err
	}
	hooks, _ := settings["hooks"].(map[string]interface{})
	if hooks == nil {
		return false, nil
	}
	entries, _ := hooks["SessionEnd"].([]interface{})
	if len(entries) == 0 {
		return false, nil
	}
	var kept []interface{}
	removed := 0
	for _, e := range entries {
		em, _ := e.(map[string]interface{})
		if em == nil {
			kept = append(kept, e)
			continue
		}
		items, _ := em["hooks"].([]interface{})
		var keptItems []interface{}
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			if im == nil {
				keptItems = append(keptItems, it)
				continue
			}
			cmd, _ := im["command"].(string)
			if containsSessionMemoryHook(cmd) {
				removed++
				continue
			}
			keptItems = append(keptItems, it)
		}
		if len(keptItems) > 0 {
			em["hooks"] = keptItems
			kept = append(kept, em)
		}
	}
	if removed == 0 {
		return false, nil
	}
	if len(kept) == 0 {
		delete(hooks, "SessionEnd")
	} else {
		hooks["SessionEnd"] = kept
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	if err := writeSettings(settings); err != nil {
		return false, err
	}
	return true, nil
}

// Status reports whether the session-memory hook is installed.
func Status() (bool, error) {
	settings, err := readSettings()
	if err != nil {
		return false, err
	}
	hooks, _ := settings["hooks"].(map[string]interface{})
	entries, _ := hooks["SessionEnd"].([]interface{})
	for _, e := range entries {
		em, _ := e.(map[string]interface{})
		if em == nil {
			continue
		}
		items, _ := em["hooks"].([]interface{})
		for _, it := range items {
			im, _ := it.(map[string]interface{})
			cmd, _ := im["command"].(string)
			if containsSessionMemoryHook(cmd) {
				return true, nil
			}
		}
	}
	return false, nil
}

func readSettings() (map[string]interface{}, error) {
	path := SettingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]interface{}{}, nil
		}
		return nil, fmt.Errorf("reading settings: %w", err)
	}
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parsing settings: %w", err)
	}
	if settings == nil {
		settings = map[string]interface{}{}
	}
	return settings, nil
}

func writeSettings(settings map[string]interface{}) error {
	path := SettingsPath()
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}
	return platform.AtomicWrite(path, append(data, '\n'), 0600)
}
