// Package errcode defines the stable error codes shown to users and scripts
// (CLI --json) and their process exit codes.
package errcode

import (
	"errors"
	"fmt"
)

// Code is a stable, machine-readable error identifier.
type Code string

const (
	UnsafeRecord     Code = "unsafe_record"      // complete but unparsable record; session marked unsafe
	FileReplaced     Code = "file_replaced"      // source truncated, replaced or rewritten
	PrefixMismatch   Code = "prefix_mismatch"    // consumed prefix no longer matches (deep check)
	UnknownFormat    Code = "unknown_format"     // client/version/format outside the compatibility matrix
	ClientRunning    Code = "client_running"     // target client running or state unknown
	BaselineChanged  Code = "baseline_changed"   // target differs from the plan's baseline
	DepsMissing      Code = "deps_missing"       // revision objects or attachments missing
	Conflict         Code = "conflict"           // more than one live head
	NeedsAction      Code = "needs_action"       // choice landing needs the user's confirmation
	SecretDetected   Code = "secret_detected"    // risk scan hit; publish/convert paused
	FidelityBlocked  Code = "fidelity_blocked"   // conversion fidelity too low; nothing written
	TargetRejected   Code = "target_rejected"    // target client rejected the output; rolled back
	RecoveryNeeded   Code = "recovery_needed"    // unfinished journal or damaged local state
	NotTracked       Code = "not_tracked"        // session is not under SessionMemory
	NotFound         Code = "not_found"          // unknown session/revision/binding id
	NoVault          Code = "no_vault"           // vault not initialised / key unavailable
	AuthFailed       Code = "auth_failed"        // object failed authentication (quarantined)
	Usage            Code = "usage"              // bad command line
	Internal         Code = "internal"           // bug or unexpected failure
	Unsupported      Code = "unsupported"        // feature not available for this agent/version
	NotIsolated      Code = "not_isolated"       // dev tool refused: target is not a marked sandbox
	DeviceIDConflict Code = "device_id_conflict" // sync folder has a newer sequence for this device id
)

// Exit codes: 0 success; each family of Code maps to one stable exit code so
// shell scripts can branch without parsing output.
var exitCodes = map[Code]int{
	Usage:            2,
	Conflict:         3,
	NeedsAction:      3,
	ClientRunning:    4,
	UnknownFormat:    5,
	Unsupported:      5,
	RecoveryNeeded:   6,
	DeviceIDConflict: 6,
	SecretDetected:   7,
	FidelityBlocked:  7,
	NotTracked:       8,
	NotFound:         8,
	NoVault:          9,
	AuthFailed:       9,
}

// ExitCode returns the process exit code for c (1 for any other code).
func ExitCode(c Code) int {
	if n, ok := exitCodes[c]; ok {
		return n
	}
	return 1
}

// Error is an error carrying a Code and non-sensitive details (IDs, counts,
// offsets, hashes; never conversation text).
type Error struct {
	Code    Code
	Message string
	Details map[string]any
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an Error.
func New(c Code, msg string) *Error { return &Error{Code: c, Message: msg} }

// Wrap builds an Error around a cause.
func Wrap(c Code, msg string, err error) *Error { return &Error{Code: c, Message: msg, Err: err} }

// With returns a copy of e with one more detail.
func (e *Error) With(key string, value any) *Error {
	d := make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		d[k] = v
	}
	d[key] = value
	cp := *e
	cp.Details = d
	return &cp
}

// CodeOf extracts the Code of err, or Internal.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Internal
}
