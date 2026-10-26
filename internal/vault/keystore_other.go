//go:build !darwin

package vault

func keyringAvailable() error { return nil }
