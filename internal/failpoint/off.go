//go:build !failpoint

package failpoint

func hit(string) error { return nil }
