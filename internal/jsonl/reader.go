// Package jsonl reads append-only JSONL files incrementally and safely.
//
// Two rules make an offset-based cursor trustworthy:
//
//   - only records that end in '\n' are consumed; a trailing partial line is
//     left for the next read and never advances the offset;
//   - before resuming, the cursor is checked against the file (see Verify), so
//     truncation, replacement and same-length rewrites are detected instead of
//     silently continuing from a stale offset.
//
// The package is format-agnostic: it knows lines and, optionally, whether a
// line is valid JSON. Interpreting records belongs to the source adapters.
package jsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// DefaultMaxRecordBytes bounds a single record (including its newline).
const DefaultMaxRecordBytes int64 = 16 << 20

// ErrTruncated reports a start offset or cursor beyond the end of the file.
var ErrTruncated = errors.New("jsonl: file is shorter than the recorded offset")

// Options configures a Reader.
type Options struct {
	// MaxRecordBytes caps one record; <= 0 selects DefaultMaxRecordBytes.
	MaxRecordBytes int64
	// RequireJSON makes a complete line that is not valid JSON an error
	// (*InvalidRecordError). Blank lines are always accepted.
	RequireJSON bool
}

// Record is one complete line. Line includes the trailing newline (and a
// preceding '\r' if present) and is owned by the caller.
type Record struct {
	Start, End int64
	Line       []byte
}

// Blank reports whether the record holds only whitespace.
func (r Record) Blank() bool { return len(bytes.TrimSpace(r.Line)) == 0 }

// RecordTooLargeError reports a record longer than Options.MaxRecordBytes.
type RecordTooLargeError struct {
	Start int64
	Max   int64
}

func (e *RecordTooLargeError) Error() string {
	return fmt.Sprintf("jsonl: record at byte offset %d exceeds %d bytes", e.Start, e.Max)
}

// InvalidRecordError reports a complete line that is not valid JSON.
type InvalidRecordError struct {
	Start, End int64
}

func (e *InvalidRecordError) Error() string {
	return fmt.Sprintf("jsonl: invalid JSON record at byte offset %d", e.Start)
}

// Reader yields complete records starting at a byte offset.
//
// After Next returns any error the Reader is finished and keeps returning that
// error. To read data appended later, open a new Reader at Offset().
type Reader struct {
	c       io.Closer
	br      *bufio.Reader
	opts    Options
	offset  int64
	pending int64
	err     error
}

// Open positions a Reader at start. start must be a record boundary, normally
// the Offset of a verified Cursor. A start beyond the end of the file returns
// ErrTruncated.
func Open(path string, start int64, opts Options) (*Reader, error) {
	if start < 0 {
		return nil, fmt.Errorf("jsonl: negative start offset %d", start)
	}
	if opts.MaxRecordBytes <= 0 {
		opts.MaxRecordBytes = DefaultMaxRecordBytes
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() < start {
		f.Close()
		return nil, fmt.Errorf("%w: size %d, start %d", ErrTruncated, st.Size(), start)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &Reader{c: f, br: bufio.NewReaderSize(f, 1<<16), opts: opts, offset: start}, nil
}

// NewReader reads records from r, whose first byte is at stream offset start
// (a decompressed offset for zstd sources). Close closes r.
func NewReader(r io.ReadCloser, start int64, opts Options) *Reader {
	if opts.MaxRecordBytes <= 0 {
		opts.MaxRecordBytes = DefaultMaxRecordBytes
	}
	return &Reader{c: r, br: bufio.NewReaderSize(r, 1<<16), opts: opts, offset: start}
}

// Next returns the next complete record.
//
// io.EOF means a clean end of data, or that only a partial last line remains
// (see Pending). *RecordTooLargeError and *InvalidRecordError stop the Reader
// without advancing Offset past the offending record.
func (r *Reader) Next() (Record, error) {
	if r.err != nil {
		return Record{}, r.err
	}
	start := r.offset
	line, err := r.readLine(start)
	if err != nil {
		r.err = err
		return Record{}, err
	}
	end := start + int64(len(line))
	rec := Record{Start: start, End: end, Line: line}
	if r.opts.RequireJSON && !rec.Blank() && !json.Valid(line) {
		r.err = &InvalidRecordError{Start: start, End: end}
		return Record{}, r.err
	}
	r.offset = end
	return rec, nil
}

func (r *Reader) readLine(start int64) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		if int64(len(buf))+int64(len(chunk)) > r.opts.MaxRecordBytes {
			return nil, &RecordTooLargeError{Start: start, Max: r.opts.MaxRecordBytes}
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			r.pending = int64(len(buf))
			return nil, io.EOF
		default:
			return nil, err
		}
	}
}

// Offset is the end of the last record returned by Next: the safe position to
// store in a cursor.
func (r *Reader) Offset() int64 { return r.offset }

// Pending is the number of bytes of an unterminated trailing line that were
// seen but not consumed. It is only meaningful after Next returned io.EOF.
func (r *Reader) Pending() int64 { return r.pending }

// Close releases the file.
func (r *Reader) Close() error { return r.c.Close() }
