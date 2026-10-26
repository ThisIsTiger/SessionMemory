package errcode

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCodes(t *testing.T) {
	for c, want := range map[Code]int{Usage: 2, Conflict: 3, NeedsAction: 3, ClientRunning: 4, UnknownFormat: 5, Unsupported: 5,
		RecoveryNeeded: 6, DeviceIDConflict: 6, SecretDetected: 7, FidelityBlocked: 7, NotTracked: 8, NotFound: 8, NoVault: 9, AuthFailed: 9, Internal: 1, NotIsolated: 1} {
		if got := ExitCode(c); got != want {
			t.Errorf("%s -> %d, want %d", c, got, want)
		}
	}
}

func TestWrapAndCodeOf(t *testing.T) {
	base := errors.New("boom")
	e := Wrap(Conflict, "two heads", base).With("heads", 2)
	if !errors.Is(e, base) || CodeOf(fmt.Errorf("ctx: %w", e)) != Conflict || e.Details["heads"] != 2 {
		t.Errorf("wrap/unwrap broken: %v", e)
	}
	if CodeOf(errors.New("x")) != Internal {
		t.Error("plain error must be Internal")
	}
}
