//go:build darwin

package vault

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// security -i can wait indefinitely trying to create a missing default
// keychain. Probe it without secrets first and fail closed instead.
func keyringAvailable() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "/usr/bin/security", "default-keychain", "-d", "user").Run(); err != nil {
		return errors.New("default macOS keychain is unavailable; configure or unlock a keychain before initialising the vault")
	}
	return nil
}
