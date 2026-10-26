// Package logx is the only way internal/ code logs. Its attribute helpers take
// typed values (IDs, counts, offsets, hashes, error codes), so conversation
// text, titles, paths of user projects and secrets cannot be passed by
// accident. A test (TestNoRawLogAttrs) fails the build if internal/ code calls
// slog's free-form String/Any attributes or fmt-formats into a message.
package logx

import (
	"io"
	"log/slog"
	"regexp"
	"time"

	"session-memory/internal/errcode"
)

var idPattern = regexp.MustCompile(`^(ss|dev|bind|tx|req|vlt)_[a-z2-7]{16,26}$|^(rev|obj)_[0-9a-f]{64}$`)

// New returns a JSON logger at the given level writing to w.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// ID logs a SessionMemory identifier; anything that is not one is replaced by
// "<invalid>" so a path or title passed here cannot leak. Long hash-style IDs
// are shortened to 12 hex characters after the prefix.
func ID(key, v string) slog.Attr {
	if !idPattern.MatchString(v) {
		return slog.String(key, "<invalid>")
	}
	for i := 0; i < len(v); i++ {
		if v[i] == '_' && len(v)-i-1 == 64 {
			return slog.String(key, v[:i+1+12])
		}
	}
	return slog.String(key, v)
}

func Count(key string, n int64) slog.Attr  { return slog.Int64(key, n) }
func Offset(key string, n int64) slog.Attr { return slog.Int64(key, n) }
func Dur(key string, d time.Duration) slog.Attr {
	return slog.Int64(key+"_ms", d.Milliseconds())
}

// Hash logs the first 12 hex characters of a hash.
func Hash(key, hex string) slog.Attr {
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return slog.String(key, hex)
}

// Code logs an error code.
func Code(key string, c errcode.Code) slog.Attr { return slog.String(key, string(c)) }

// Name logs one of a fixed set of identifiers chosen by the program (agent
// names, record type names, state names). Callers pass constants only; the
// lint test enforces that the argument is not a variable of type string
// derived from file or network input.
func Name(key, constant string) slog.Attr { return slog.String(key, constant) }
