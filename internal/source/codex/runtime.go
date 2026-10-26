package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"session-memory/internal/agent"
	"session-memory/internal/platform"
)

type runtimeDetector struct{}

func (a *Adapter) Runtime() agent.RuntimeDetector { return runtimeDetector{} }

// Running implements the conservative default of spec §8.3: a held thread
// writer lock means Running; a missing lock directory or a free lock cannot
// prove the client is not running, so the answer is Unknown until process
// detection (M5, experiment E2a) is available.
func (runtimeDetector) Running(ctx context.Context, home, nativeID string) (agent.RunState, agent.RunDetail, error) {
	locks := filepath.Join(home, "thread-writer-locks")
	if st, err := os.Stat(locks); err != nil || !st.IsDir() {
		return agent.Unknown, agent.DetailUnknown, nil
	}
	if nativeID != "" {
		f, err := os.OpenFile(filepath.Join(locks, strings.ToLower(nativeID)+".lock"), os.O_RDWR, 0)
		if err == nil {
			defer f.Close()
			if err := platform.LockExclusiveNonBlocking(f); errors.Is(err, platform.ErrLockHeld) {
				return agent.Running, agent.DetailWindowOpen, nil
			} else if err == nil {
				platform.Unlock(f)
			}
		}
	}
	return agent.Unknown, agent.DetailUnknown, nil
}
