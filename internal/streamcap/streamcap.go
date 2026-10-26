// Package streamcap captures append-only JSONL artifacts (plain or zstd) into
// segments: the generic engine behind the Codex and WorkBuddy adapters
// (spec §8.2).
package streamcap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"session-memory/internal/agent"
	"session-memory/internal/jsonl"
	"session-memory/internal/session"
)

// File is one on-disk form of a logical artifact.
type File struct {
	Rel  string // relative to the home, slash-separated, as on disk
	Zst  bool
	Mode os.FileMode
}

// Options tunes capture.
type Options struct{ MaxRecordBytes int64 }

func CaptureStream(ctx context.Context, home, logic string, fs []File, old session.ArtifactState, sink agent.SegmentSink, o Options) (*session.ArtifactState, agent.CaptureState, bool, []agent.Diag, error) {
	var diags []agent.Diag
	chosen, twinOK := ChooseTwin(home, fs)
	if !twinOK {
		return nil, agent.CaptureUnsafe, false, []agent.Diag{{Code: "twin_diverged", Detail: logic}}, nil
	}
	path := filepath.Join(home, filepath.FromSlash(chosen.Rel))
	src, err := jsonl.OpenSource(path)
	if err != nil {
		return nil, agent.CaptureOK, false, nil, err
	}
	form := session.FormPlain
	if chosen.Zst {
		form = session.FormZstd
	}

	state := agent.CaptureOK
	gen := 0
	var segs []session.SegmentRef
	var offset int64
	if len(old.Segments) > 0 || old.Cursor != nil {
		gen = 0
		if old.Cursor != nil {
			gen = old.Cursor.Generation
		}
		segs = append(segs, old.Segments...)
		if len(segs) > 0 {
			offset = segs[len(segs)-1].End
		}
		ok, err := PrefixMatches(src, path, old, segs)
		if err != nil {
			return nil, agent.CaptureOK, false, nil, err
		}
		if !ok {
			gen++
			segs, offset = nil, 0
			state = agent.CaptureReplaced
		}
	}

	key := agent.ArtifactKey{Path: logic, Kind: session.KindStream, Form: form, Generation: gen, Mode: uint32(chosen.Mode)}
	rc, _, err := src.OpenAt(offset)
	if err != nil {
		return nil, agent.CaptureOK, false, nil, err
	}
	rd := jsonl.NewReader(rc, offset, jsonl.Options{MaxRecordBytes: o.MaxRecordBytes, RequireJSON: true})
	defer rd.Close()

	var buf bytes.Buffer
	segStart := offset
	changed := false
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		end := segStart + int64(buf.Len())
		ref, err := sink.PutSegment(ctx, key, segStart, end, bytes.NewReader(buf.Bytes()))
		if err != nil {
			return err
		}
		segs = append(segs, ref)
		segStart = end
		buf.Reset()
		changed = true
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, agent.CaptureOK, false, nil, err
		}
		rec, err := rd.Next()
		if err != nil {
			var inv *jsonl.InvalidRecordError
			var big *jsonl.RecordTooLargeError
			switch {
			case errors.Is(err, io.EOF):
				if p := rd.Pending(); p > 0 {
					diags = append(diags, agent.Diag{Code: "partial_tail", Detail: logic, Offset: rd.Offset()})
				}
			case errors.As(err, &inv) || errors.As(err, &big):
				state = agent.CaptureUnsafe
				diags = append(diags, agent.Diag{Code: "unsafe_record", Detail: logic, Offset: rd.Offset()})
			default:
				return nil, agent.CaptureOK, false, nil, err
			}
			break
		}
		if buf.Len() > 0 && int64(buf.Len())+int64(len(rec.Line)) > int64(agent.MaxSegmentBytes) {
			if err := flush(); err != nil {
				return nil, agent.CaptureOK, false, nil, err
			}
		}
		buf.Write(rec.Line)
	}
	if err := flush(); err != nil {
		return nil, agent.CaptureOK, false, nil, err
	}
	if len(segs) == 0 {
		return nil, state, changed, diags, nil
	}
	end := segs[len(segs)-1].End
	st := session.ArtifactState{Kind: session.KindStream, Form: form, Mode: uint32(chosen.Mode), Segments: segs,
		Size: end, Digest: session.SegmentsDigest(segs)}
	cursor := &session.StreamCursor{Generation: gen, Offset: end}
	if !chosen.Zst {
		if c, err := jsonl.NewCursor(path, gen, end); err == nil {
			cursor = &session.StreamCursor{Generation: c.Generation, Offset: c.Offset, FileID: c.FileID, HeadHash: c.HeadHash, TailHash: c.TailHash}
		}
	}
	st.Cursor = cursor
	return &st, state, changed, diags, nil
}

// PrefixMatches checks the consumed prefix. Plain files with stored edge hashes
// use the fast jsonl.Verify; everything else re-hashes each segment's range
// (the only option for zstd).
func PrefixMatches(src jsonl.Source, path string, old session.ArtifactState, segs []session.SegmentRef) (bool, error) {
	if len(segs) == 0 {
		return true, nil
	}
	if src.Form() == jsonl.FormPlain && old.Cursor != nil && old.Cursor.HeadHash != "" {
		c := jsonl.Cursor{Generation: old.Cursor.Generation, Offset: old.Cursor.Offset, FileID: old.Cursor.FileID,
			HeadHash: old.Cursor.HeadHash, TailHash: old.Cursor.TailHash}
		chk, err := jsonl.Verify(path, c)
		if err != nil {
			return false, err
		}
		return chk.Status == jsonl.StatusOK, nil
	}
	return VerifySegments(src, segs)
}

// VerifySegments re-hashes every segment range of the stream and compares it
// with raw_hash (deep verification, spec §2.2 rule 4).
func VerifySegments(src jsonl.Source, segs []session.SegmentRef) (bool, error) {
	rc, _, err := src.OpenAt(0)
	if err != nil {
		if errors.Is(err, jsonl.ErrTruncated) {
			return false, nil
		}
		return false, err
	}
	defer rc.Close()
	for _, s := range segs {
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(rc, s.End-s.Start))
		if err != nil {
			return false, nil
		}
		if n != s.End-s.Start || hex.EncodeToString(h.Sum(nil)) != s.RawHash {
			return false, nil
		}
	}
	return true, nil
}

// ChooseTwin picks the live stream of a logical file that may exist as both
// .jsonl and .jsonl.zst: the shorter must be a prefix of the longer.
func ChooseTwin(home string, fs []File) (File, bool) {
	if len(fs) == 1 {
		return fs[0], true
	}
	var plain, packed *File
	for i := range fs {
		if fs[i].Zst {
			packed = &fs[i]
		} else {
			plain = &fs[i]
		}
	}
	if plain == nil || packed == nil {
		return fs[0], true
	}
	a, err1 := readAllLimited(filepath.Join(home, filepath.FromSlash(plain.Rel)))
	b, err2 := readAllLimited(filepath.Join(home, filepath.FromSlash(packed.Rel)))
	if err1 != nil || err2 != nil {
		return *plain, true
	}
	switch {
	case bytes.HasPrefix(a, b):
		return *plain, true
	case bytes.HasPrefix(b, a):
		return *packed, true
	}
	return File{}, false
}

func readAllLimited(path string) ([]byte, error) {
	src, err := jsonl.OpenSource(path)
	if err != nil {
		return nil, err
	}
	rc, _, err := src.OpenAt(0)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// CaptureBlob stores an immutable file. It is re-captured only when its
// content changed: the chunk hashes are recomputed locally and compared with
// the recorded segments digest.
func CaptureBlob(ctx context.Context, home string, f File, logic string, old session.ArtifactState, sink agent.SegmentSink) (*session.ArtifactState, bool, error) {
	data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(f.Rel)))
	if err != nil {
		return nil, false, err
	}
	var fresh []session.SegmentRef
	for off := 0; off < len(data); off += agent.MaxSegmentBytes {
		end := off + agent.MaxSegmentBytes
		if end > len(data) {
			end = len(data)
		}
		h := sha256.Sum256(data[off:end])
		fresh = append(fresh, session.SegmentRef{RawHash: hex.EncodeToString(h[:])})
	}
	if old.Kind == session.KindBlob && old.Digest != "" && old.Digest == session.SegmentsDigest(fresh) && old.Size == int64(len(data)) {
		return &old, false, nil
	}
	key := agent.ArtifactKey{Path: logic, Kind: session.KindBlob, Mode: uint32(f.Mode)}
	segs, err := sink.PutBlob(ctx, key, int64(len(data)), bytes.NewReader(data))
	if err != nil {
		return nil, false, err
	}
	st := session.ArtifactState{Kind: session.KindBlob, Mode: uint32(f.Mode), Segments: segs, Size: int64(len(data)), Digest: session.SegmentsDigest(segs)}
	return &st, true, nil
}

// VerifyArtifacts checks captured artifacts against the files on disk and
// returns the logical paths that no longer match.
func VerifyArtifacts(home string, files map[string][]File, arts map[string]session.ArtifactState, deep bool) ([]string, error) {
	var bad []string
	for logic, st := range arts {
		fs := files[logic]
		if len(fs) == 0 {
			bad = append(bad, logic+" (missing)")
			continue
		}
		if st.Kind == session.KindBlob {
			data, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(fs[0].Rel)))
			if err != nil || int64(len(data)) != st.Size {
				bad = append(bad, logic)
			}
			continue
		}
		chosen, ok := ChooseTwin(home, fs)
		if !ok {
			bad = append(bad, logic+" (twin diverged)")
			continue
		}
		path := filepath.Join(home, filepath.FromSlash(chosen.Rel))
		src, err := jsonl.OpenSource(path)
		if err != nil {
			return nil, err
		}
		var good bool
		if deep || src.Form() != jsonl.FormPlain {
			good, err = VerifySegments(src, st.Segments)
		} else {
			good, err = PrefixMatches(src, path, st, st.Segments)
		}
		if err != nil {
			return nil, err
		}
		if !good {
			bad = append(bad, logic)
		}
	}
	return bad, nil
}
