package jsonl

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendFile(t *testing.T, p, content string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, r *Reader) ([]Record, error) {
	t.Helper()
	var out []Record
	for {
		rec, err := r.Next()
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
}

func TestReaderOffsetsAndLines(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n{\"b\":2}\r\n\n{\"c\":3}\n")
	r, err := Open(p, 0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	recs, err := readAll(t, r)
	if err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4", len(recs))
	}
	want := []struct {
		start, end int64
		blank      bool
	}{{0, 8, false}, {8, 17, false}, {17, 18, true}, {18, 26, false}}
	for i, w := range want {
		if recs[i].Start != w.start || recs[i].End != w.end || recs[i].Blank() != w.blank {
			t.Errorf("record %d = [%d,%d) blank=%v, want [%d,%d) blank=%v",
				i, recs[i].Start, recs[i].End, recs[i].Blank(), w.start, w.end, w.blank)
		}
	}
	if r.Offset() != 26 || r.Pending() != 0 {
		t.Errorf("offset/pending = %d/%d, want 26/0", r.Offset(), r.Pending())
	}
}

func TestReaderLeavesPartialLineForNextRead(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n{\"b\":")
	r, err := Open(p, 0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := readAll(t, r)
	r.Close()
	if err != io.EOF || len(recs) != 1 {
		t.Fatalf("records=%d err=%v, want 1 record then EOF", len(recs), err)
	}
	if r.Offset() != 8 {
		t.Errorf("offset %d advanced into the partial line, want 8", r.Offset())
	}
	if r.Pending() != 5 {
		t.Errorf("pending = %d, want 5", r.Pending())
	}

	appendFile(t, p, "2}\n")
	r2, err := Open(p, r.Offset(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	recs, err = readAll(t, r2)
	if err != io.EOF || len(recs) != 1 || string(recs[0].Line) != "{\"b\":2}\n" {
		t.Fatalf("resumed read = %v err=%v", recs, err)
	}
	if recs[0].Start != 8 {
		t.Errorf("resumed record start = %d, want 8", recs[0].Start)
	}
}

func TestReaderLongRecordAcrossBuffer(t *testing.T) {
	long := `{"x":"` + strings.Repeat("a", 200_000) + `"}` + "\n"
	p := writeFile(t, long+"{\"y\":1}\n")
	r, err := Open(p, 0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	recs, _ := readAll(t, r)
	if len(recs) != 2 || int64(len(long)) != recs[0].End {
		t.Fatalf("long record mishandled: %d records", len(recs))
	}
	if string(recs[0].Line) != long {
		t.Error("long record content differs")
	}
}

func TestReaderRecordTooLarge(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n{\"big\":\""+strings.Repeat("z", 500)+"\"}\n{\"c\":3}\n")
	r, err := Open(p, 0, Options{MaxRecordBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	recs, err := readAll(t, r)
	var big *RecordTooLargeError
	if !errors.As(err, &big) {
		t.Fatalf("want RecordTooLargeError, got %v", err)
	}
	if big.Start != 8 || len(recs) != 1 {
		t.Errorf("start=%d records=%d, want 8 and 1", big.Start, len(recs))
	}
	if r.Offset() != 8 {
		t.Errorf("offset %d moved past the oversized record, want 8", r.Offset())
	}
	if _, err2 := r.Next(); !errors.As(err2, &big) {
		t.Errorf("reader must stay failed, got %v", err2)
	}
}

func TestReaderRequireJSONStopsBeforeBadRecord(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n\n{not json}\n{\"c\":3}\n")
	r, err := Open(p, 0, Options{RequireJSON: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	recs, err := readAll(t, r)
	var bad *InvalidRecordError
	if !errors.As(err, &bad) {
		t.Fatalf("want InvalidRecordError, got %v", err)
	}
	if bad.Start != 9 || len(recs) != 2 {
		t.Errorf("start=%d records=%d, want 9 and 2 (record + blank line)", bad.Start, len(recs))
	}
	if r.Offset() != 9 {
		t.Errorf("offset = %d, must stay before the corrupt record (9)", r.Offset())
	}
}

func TestReaderWithoutRequireJSONPassesGarbageThrough(t *testing.T) {
	p := writeFile(t, "not json\n")
	r, _ := Open(p, 0, Options{})
	defer r.Close()
	recs, err := readAll(t, r)
	if err != io.EOF || len(recs) != 1 {
		t.Fatalf("records=%d err=%v", len(recs), err)
	}
}

func TestOpenBeyondEOF(t *testing.T) {
	p := writeFile(t, "{}\n")
	if _, err := Open(p, 100, Options{}); !errors.Is(err, ErrTruncated) {
		t.Errorf("want ErrTruncated, got %v", err)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "nope"), 0, Options{}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: got %v", err)
	}
}

func cursorAtEnd(t *testing.T, p string) Cursor {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCursor(p, 0, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerifyOKAfterAppend(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n{\"b\":2}\n")
	c := cursorAtEnd(t, p)
	appendFile(t, p, "{\"c\":3}\n")
	got, err := Verify(p, c)
	if err != nil || got.Status != StatusOK {
		t.Fatalf("Verify = %+v err=%v, want OK", got, err)
	}
	if got.Size != 24 {
		t.Errorf("size = %d, want 24", got.Size)
	}
}

func TestVerifyZeroOffsetCursor(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n")
	c, err := NewCursor(p, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.HeadHash != "" || c.TailHash != "" {
		t.Errorf("an empty prefix must not carry hashes: %+v", c)
	}
	if got, err := Verify(p, c); err != nil || got.Status != StatusOK {
		t.Errorf("Verify = %+v err=%v", got, err)
	}
}

func TestVerifyDetectsTruncation(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n{\"b\":2}\n")
	c := cursorAtEnd(t, p)
	if err := os.WriteFile(p, []byte("{\"a\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Verify(p, c)
	if err != nil || got.Status != StatusTruncated {
		t.Fatalf("Verify = %+v err=%v, want truncated", got, err)
	}
}

func TestVerifyDetectsSameLengthRewrite(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n{\"b\":2}\n")
	c := cursorAtEnd(t, p)
	// identical size, different bytes: a size/mtime check would miss this
	if err := os.WriteFile(p, []byte("{\"a\":9}\n{\"b\":2}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Verify(p, c)
	if err != nil || got.Status != StatusReplaced {
		t.Fatalf("Verify = %+v err=%v, want replaced", got, err)
	}
}

func TestVerifyDetectsReplacementThatGrewPastOffset(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n")
	c := cursorAtEnd(t, p)
	// truncated and regrown beyond the old offset with different content
	if err := os.WriteFile(p, []byte("{\"z\":0}\n{\"y\":1}\n{\"x\":2}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Verify(p, c)
	if err != nil || got.Status != StatusReplaced {
		t.Fatalf("Verify = %+v err=%v, want replaced", got, err)
	}
}

func TestVerifyTreatsAtomicRewriteOfSameContentAsOK(t *testing.T) {
	content := "{\"a\":1}\n{\"b\":2}\n"
	p := writeFile(t, content)
	c := cursorAtEnd(t, p)

	tmp := p + ".new"
	if err := os.WriteFile(tmp, []byte(content+"{\"c\":3}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	got, err := Verify(p, c)
	if err != nil || got.Status != StatusOK {
		t.Fatalf("Verify = %+v err=%v, want OK (same prefix, new file)", got, err)
	}
	if runtime.GOOS != "windows" && !got.FileIDChanged {
		t.Errorf("a new inode should be reported through FileIDChanged")
	}
}

func TestVerifyDetectsHeadRewriteInLongFile(t *testing.T) {
	line := "{\"k\":\"" + strings.Repeat("v", 100) + "\"}\n"
	content := strings.Repeat(line, 200) // ~21 KB, longer than both windows combined
	p := writeFile(t, content)
	c := cursorAtEnd(t, p)

	b := []byte(content)
	b[10] ^= 0x01 // flips a byte inside the head window
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if got, _ := Verify(p, c); got.Status != StatusReplaced {
		t.Errorf("head rewrite: status %v, want replaced", got.Status)
	}

	b = []byte(content)
	b[len(b)-10] ^= 0x01 // inside the tail window
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if got, _ := Verify(p, c); got.Status != StatusReplaced {
		t.Errorf("tail rewrite: status %v, want replaced", got.Status)
	}
}

func TestVerifyMissingFile(t *testing.T) {
	p := writeFile(t, "{}\n")
	c := cursorAtEnd(t, p)
	os.Remove(p)
	if _, err := Verify(p, c); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: want os.ErrNotExist, got %v", err)
	}
}

func TestPrefixHashCatchesMiddleRewriteThatEdgeHashesMiss(t *testing.T) {
	line := "{\"k\":\"" + strings.Repeat("v", 100) + "\"}\n"
	content := strings.Repeat(line, 200)
	p := writeFile(t, content)
	c := cursorAtEnd(t, p)
	before, err := PrefixHash(p, c.Offset)
	if err != nil {
		t.Fatal(err)
	}

	b := []byte(content)
	b[len(b)/2] ^= 0x01 // far from both 4 KiB windows
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if got, _ := Verify(p, c); got.Status != StatusOK {
		t.Fatalf("documented limitation changed: edge hashes now see a middle rewrite (%v)", got.Status)
	}
	after, _ := PrefixHash(p, c.Offset)
	if before == after {
		t.Error("PrefixHash must detect the middle rewrite")
	}
	if _, err := PrefixHash(p, c.Offset+1); !errors.Is(err, ErrTruncated) {
		t.Errorf("PrefixHash past EOF: got %v, want ErrTruncated", err)
	}
}

func TestNewCursorRejectsOffsetPastEOF(t *testing.T) {
	p := writeFile(t, "{}\n")
	if _, err := NewCursor(p, 0, 99); !errors.Is(err, ErrTruncated) {
		t.Errorf("got %v, want ErrTruncated", err)
	}
}

func TestResumeFromCursorReadsOnlyNewRecords(t *testing.T) {
	p := writeFile(t, "{\"a\":1}\n{\"b\":2}\n")
	r, _ := Open(p, 0, Options{RequireJSON: true})
	if _, err := readAll(t, r); err != io.EOF {
		t.Fatal(err)
	}
	c, err := NewCursor(p, 0, r.Offset())
	r.Close()
	if err != nil {
		t.Fatal(err)
	}

	appendFile(t, p, "{\"c\":3}\n{\"d\":")
	if got, err := Verify(p, c); err != nil || got.Status != StatusOK {
		t.Fatalf("Verify = %+v err=%v", got, err)
	}
	r2, err := Open(p, c.Offset, Options{RequireJSON: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	recs, err := readAll(t, r2)
	if err != io.EOF || len(recs) != 1 || string(recs[0].Line) != "{\"c\":3}\n" {
		t.Fatalf("got %v err=%v", recs, err)
	}
}

func TestStatusString(t *testing.T) {
	for s, want := range map[Status]string{StatusOK: "ok", StatusTruncated: "truncated", StatusReplaced: "replaced"} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", int(s), s.String(), want)
		}
	}
}
