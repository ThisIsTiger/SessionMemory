// Package canonjson produces the canonical JSON encoding used wherever bytes
// must be identical on every device: revision IDs, manifest hashes, plan
// digests.
//
// Profile (a strict subset of RFC 8785 JCS, enough because every key is ASCII
// and every number is an integer):
//
//   - object keys sorted by their UTF-8 bytes, no duplicates;
//   - no insignificant whitespace;
//   - strings escape only '"', '\\' and control characters < 0x20 (as \b \f
//     \n \r \t where a short form exists, otherwise \u00xx with lower-case
//     hex); '/', '<', '>', '&', U+2028 and U+2029 are written as they are;
//   - numbers must be integers in the int64 range, written without sign
//     prefix, leading zeros, fraction or exponent; "-0" is rejected;
//   - null is rejected: optional fields use omitempty or a pointer left nil;
//   - arrays keep their order.
package canonjson

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// Marshal encodes v canonically. v is first encoded with encoding/json
// (struct tags apply), then re-encoded under the profile above.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

// Canonicalize re-encodes already-encoded JSON under the profile.
func Canonicalize(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("canonjson: trailing data after the JSON value")
	}
	var buf bytes.Buffer
	if err := encode(&buf, x); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Hash is the lower-case hex SHA-256 of Marshal(v).
func Hash(v any) (string, error) {
	b, err := Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func encode(buf *bytes.Buffer, x any) error {
	switch t := x.(type) {
	case nil:
		return errors.New("canonjson: null is not allowed (use omitempty)")
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case json.Number:
		s := t.String()
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || strconv.FormatInt(n, 10) != s {
			return fmt.Errorf("canonjson: %q is not a canonical int64", s)
		}
		buf.WriteString(s)
	case string:
		writeString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encode(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // byte-wise order
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := encode(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonjson: unsupported type %T", x)
	}
	return nil
}

const hexDigits = "0123456789abcdef"

func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c == '\b':
			buf.WriteString(`\b`)
		case c == '\f':
			buf.WriteString(`\f`)
		case c == '\n':
			buf.WriteString(`\n`)
		case c == '\r':
			buf.WriteString(`\r`)
		case c == '\t':
			buf.WriteString(`\t`)
		case c < 0x20:
			buf.WriteString(`\u00`)
			buf.WriteByte(hexDigits[c>>4])
			buf.WriteByte(hexDigits[c&0xf])
		default:
			buf.WriteByte(c) // bytes >= 0x80 are already valid UTF-8 after decoding
		}
	}
	buf.WriteByte('"')
}
