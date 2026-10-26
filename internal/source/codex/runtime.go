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

// Running implements the rule established by experiment E2a (docs/p0-report):
// Codex keeps a lock file per thread it has loaded, held while loaded, and
// removes it afterwards. So a held lock means Running; a missing or free lock
// means no Codex process has this thread loaded (NotRunning). A home without
// a thread-writer-locks directory (older Codex) cannot be judged: Unknown.
func (runtimeDetector) Running(ctx context.Context, home, nativeID string) (agent.RunState, agent.RunDetail, error) {
	locks := filepath.Join(home, "thread-writer-locks")
	if st, err := os.Stat(locks); err != nil || !st.IsDir() {
		return agent.Unknown, agent.DetailUnknown, nil
	}
	if nativeID == "" {
		return agent.Unknown, agent.DetailUnknown, nil
	}
	f, err := os.OpenFile(filepath.Join(locks, strings.ToLower(nativeID)+".lock"), os.O_RDWR, 0)
	if err != nil {
		return agent.NotRunning, agent.DetailNotRunning, nil
	}
	defer f.Close()
	if err := platform.LockExclusiveNonBlocking(f); errors.Is(err, platform.ErrLockHeld) {
		return agent.Running, agent.DetailWindowOpen, nil
	} else if err != nil {
		return agent.Unknown, agent.DetailUnknown, nil
	}
	platform.Unlock(f)
	return agent.NotRunning, agent.DetailNotRunning, nil
}
