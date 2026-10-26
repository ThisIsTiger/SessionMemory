// Package codex implements the Codex client source adapter: rollout file
// discovery over date directories, source classification from session_meta,
// and byte-offset incremental parsing.
package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SourceName identifies this adapter in batches and checkpoints.
const SourceName = "codex"

// SessionMeta is the first-record payload of a rollout file.
type SessionMeta struct {
	SessionID    string
	Cwd          string
	Source       string
	Originator   string
	ThreadSource json.RawMessage
	Timestamp    time.Time
}

type metaPayload struct {
	SessionID    string          `json:"session_id"`
	ID           string          `json:"id"`
	Cwd          string          `json:"cwd"`
	Source       json.RawMessage `json:"source"`
	Originator   json.RawMessage `json:"originator"`
	ThreadSource json.RawMessage `json:"thread_source"`
	Timestamp    string          `json:"timestamp"`
}

// ParseSessionMeta decodes the first rollout line's payload. source and
// originator may be strings or structured objects; objects collapse to
// their JSON kind so classification sees "<object>".
func ParseSessionMeta(data []byte) (SessionMeta, error) {
	var env struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return SessionMeta{}, fmt.Errorf("invalid_meta: first record is not JSON: %w", err)
	}
	if env.Type != "session_meta" {
		return SessionMeta{}, fmt.Errorf("invalid_meta: first record type is %q", env.Type)
	}
	var p metaPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return SessionMeta{}, fmt.Errorf("invalid_meta: payload: %w", err)
	}
	meta := SessionMeta{
		SessionID:    p.SessionID,
		Cwd:          p.Cwd,
		Source:       rawToString(p.Source),
		Originator:   rawToString(p.Originator),
		ThreadSource: p.ThreadSource,
	}
	if meta.SessionID == "" {
		meta.SessionID = p.ID
	}
	if meta.SessionID == "" {
		return meta, fmt.Errorf("invalid_meta: missing session_id")
	}
	if meta.Cwd == "" {
		return meta, fmt.Errorf("invalid_meta: missing cwd")
	}
	if p.Timestamp != "" {
		if ts, err := time.Parse(time.RFC3339, p.Timestamp); err == nil {
			meta.Timestamp = ts
		} else if ts, err := time.Parse(time.RFC3339Nano, p.Timestamp); err == nil {
			meta.Timestamp = ts
		}
	}
	return meta, nil
}

// RawSourceObject marks sessions whose `source` field is a structured
// object — observed for subagent-spawned threads (thread_spawn). Their
// content duplicates the parent session and they MUST be skipped rather
// than classified via the originator fallback (design 5.2).
const RawSourceObject = "<object>"

// rawToString normalizes a field that may be a string or an object.
// Objects become "<object>" — never their content.
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return RawSourceObject
	}
	var arr []interface{}
	if err := json.Unmarshal(raw, &arr); err == nil {
		return "<array>"
	}
	return ""
}

// Classification is the outcome of source filtering (spec 7.4).
type Classification string

const (
	ClassIncluded Classification = "included"
	ClassExcluded Classification = "excluded"
	ClassUnknown  Classification = "unknown_source"
)

// Classify applies the ordered rule: exclude wins, then include,
// then originator fallback for missing/unknown source.
func Classify(meta SessionMeta, includeSources, excludeSources, includeOriginators []string) Classification {
	src := strings.ToLower(strings.TrimSpace(meta.Source))

	// structured source (subagent thread_spawn): never summarized
	if src == RawSourceObject {
		return ClassExcluded
	}

	for _, ex := range excludeSources {
		if src != "" && src == strings.ToLower(strings.TrimSpace(ex)) {
			return ClassExcluded
		}
	}
	for _, inc := range includeSources {
		if src != "" && src == strings.ToLower(strings.TrimSpace(inc)) {
			return ClassIncluded
		}
	}
	// source missing or unknown: originator fallback (exact match)
	if src == "" || !inList(src, includeSources) {
		for _, o := range includeOriginators {
			if meta.Originator != "" && meta.Originator == strings.TrimSpace(o) {
				return ClassIncluded
			}
		}
	}
	return ClassUnknown
}

func inList(v string, list []string) bool {
	for _, item := range list {
		if v == strings.ToLower(strings.TrimSpace(item)) {
			return true
		}
	}
	return false
}

// ConfigFingerprint hashes the classification-relevant config so ignored
// files are only re-classified when the rules change.
func ConfigFingerprint(includeSources, excludeSources, includeOriginators []string) string {
	preimage := strings.Join([]string{
		strings.Join(lowerAll(includeSources), ","),
		strings.Join(lowerAll(excludeSources), ","),
		strings.Join(includeOriginators, ","),
	}, "\x00")
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.ToLower(strings.TrimSpace(v))
	}
	return out
}
