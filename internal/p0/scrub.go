package p0

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ScrubOptions controls Scrub.
type ScrubOptions struct {
	Seed  string        // makes id mapping reproducible
	Shift time.Duration // added to every timestamp value
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// keepKeys hold structural values that are safe to keep as they are (spec §14.2).
var keepKeys = map[string]bool{"type": true, "role": true, "status": true, "name": true, "originator": true, "cli_version": true,
	"source": true, "model_provider": true, "kind": true, "phase": true, "model": true}

// Scrub copies a JSONL stream keeping every key and the structure but
// replacing string values with same-length filler, UUIDs with mapped UUIDs and
// shifting timestamps. Numbers and booleans are kept.
func Scrub(r io.Reader, w io.Writer, o ScrubOptions) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var out bytes.Buffer
			dec := json.NewDecoder(bytes.NewReader(line))
			dec.UseNumber()
			if serr := scrubValue(dec, &out, "", o); serr != nil {
				return fmt.Errorf("scrub: %w", serr)
			}
			out.WriteByte('\n')
			if _, werr := w.Write(out.Bytes()); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func scrubValue(dec *json.Decoder, out *bytes.Buffer, key string, o ScrubOptions) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			out.WriteByte('{')
			first := true
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k := kt.(string)
				if !first {
					out.WriteByte(',')
				}
				first = false
				kb, _ := json.Marshal(k)
				out.Write(kb)
				out.WriteByte(':')
				if err := scrubValue(dec, out, k, o); err != nil {
					return err
				}
			}
			dec.Token()
			out.WriteByte('}')
		case '[':
			out.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					out.WriteByte(',')
				}
				first = false
				if err := scrubValue(dec, out, key, o); err != nil {
					return err
				}
			}
			dec.Token()
			out.WriteByte(']')
		}
	case string:
		b, _ := json.Marshal(scrubString(t, key, o))
		out.Write(b)
	case json.Number:
		out.WriteString(t.String())
	case bool:
		fmt.Fprintf(out, "%v", t)
	case nil:
		out.WriteString("null")
	}
	return nil
}

func scrubString(s, key string, o ScrubOptions) string {
	switch {
	case keepKeys[key]:
		return s
	case uuidRe.MatchString(s):
		sum := sha256.Sum256([]byte(o.Seed + s))
		return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	case strings.Contains(key, "timestamp") || key == "at" || key == "updated_at" || key == "created_at":
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts.Add(o.Shift).UTC().Format("2006-01-02T15:04:05.000Z")
		}
	case key == "encrypted_content":
		return strings.Repeat("A", 16)
	}
	n := utf8.RuneCountInString(s)
	if n > 64 {
		n = 64
	}
	return strings.Repeat("x", n)
}
