package workbuddy

import (
	"encoding/json"
	"path/filepath"

	"session-memory/internal/hookfile"
)

// WorkBuddy runs the commands in the "hooks" object of <home>/settings.json
// the way Claude Code does: event name -> groups of handlers. Tencent's own
// Drive skill hooks WorkBuddy the same way, and a hook added there runs from
// the next reply on with nothing for the user to confirm (checked against the
// installed app). The rest of settings.json belongs to the application (some of
// it encrypted) and is written back exactly as it was.

// HookEvents are the events session-memory listens to: a reply finishing.
var HookEvents = []string{"Stop"}

// HooksPath is the file that holds WorkBuddy's hooks.
func HooksPath(home string) string { return filepath.Join(home, "settings.json") }

// HookCommand is the command line installed for an event.
func HookCommand(exe, smHome, event string) string {
	return hookfile.Command(exe, smHome, event, AgentName)
}

// ReadHooks reports which events carry a session-memory handler.
func ReadHooks(home string, commands map[string]string) (hookfile.State, error) {
	return hookfile.Read(HooksPath(home), commands)
}

// InstallHooks makes sure each event in commands has exactly one
// session-memory handler running that command. settings.json is never
// removed, even when nothing else is left in it.
func InstallHooks(home string, commands map[string]string) (bool, error) {
	return hookfile.Install(HooksPath(home), commands, false)
}

// RemoveHooks takes session-memory's handlers out and returns how many it
// removed.
func RemoveHooks(home string) (int, error) { return hookfile.Remove(HooksPath(home), false) }

// HooksSwitchedOff says whether the settings turn every hook off.
func HooksSwitchedOff(home string) bool {
	v, ok := hookfile.Key(HooksPath(home), "disableAllHooks")
	if !ok {
		return false
	}
	var off bool
	return json.Unmarshal(v, &off) == nil && off
}
