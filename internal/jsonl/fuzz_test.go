package jsonl

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

// A reader must return exact complete source records and leave its cursor at
// the start of a partial, oversized or invalid record, even for arbitrary input.
func FuzzReaderBoundaries(f *testing.F) {
	for _, seed := range [][]byte{nil, []byte("{}\n{\"partial\":"), []byte("\r\n[1,2]\r\n"), []byte("invalid\n{}\n"), bytes.Repeat([]byte("x"), 4097)} {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	f.Fuzz(func(t *testing.T, data []byte, requireJSON bool) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		r := NewReader(io.NopCloser(bytes.NewReader(data)), 0, Options{MaxRecordBytes: 4096, RequireJSON: requireJSON})
		defer r.Close()
		var consumed int64
		for {
			rec, err := r.Next()
			if err != nil {
				if r.Offset() != consumed {
					t.Fatalf("error advanced cursor: %d != %d", r.Offset(), consumed)
				}
				if _, again := r.Next(); again != err {
					t.Fatalf("terminal error changed: %v -> %v", err, again)
				}
				break
			}
			if rec.Start != consumed || rec.End <= rec.Start || rec.End > int64(len(data)) || rec.End-rec.Start > 4096 {
				t.Fatalf("invalid boundaries: %+v, consumed=%d", rec, consumed)
			}
			if !bytes.Equal(rec.Line, data[rec.Start:rec.End]) || rec.Line[len(rec.Line)-1] != '\n' {
				t.Fatal("reader changed bytes or consumed an incomplete record")
			}
			if requireJSON && !rec.Blank() && !json.Valid(bytes.TrimSpace(rec.Line)) {
				t.Fatal("accepted invalid JSON")
			}
			consumed = rec.End
		}
	})
}
