package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Folder is a Transport over a local directory synchronised by a cloud client.
type Folder struct {
	Root   string
	Settle time.Duration // default 2s
	// retry tuning (defaults: 1s, doubling, cap 60s, give up after 10m)
	Backoff0   time.Duration
	BackoffMax time.Duration
	GiveUp     time.Duration
	sleep      func(time.Duration)
}

// NewFolder returns a Folder with spec defaults.
func NewFolder(root string) *Folder {
	return &Folder{Root: root, Settle: 2 * time.Second, Backoff0: time.Second, BackoffMax: 60 * time.Second, GiveUp: 10 * time.Minute, sleep: time.Sleep}
}

func (f *Folder) abs(p string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(p))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("transport: path escapes the root")
	}
	return filepath.Join(f.Root, clean), nil
}

var conflictCopy = regexp.MustCompile(`(?i)( \(\d+\)\.[^.]+$| \(conflicted copy[^)]*\)|-conflict-)`)

// Put publishes r at path, write-once (spec §7.2).
func (f *Folder) Put(ctx context.Context, path string, r io.Reader) error {
	dst, err := f.abs(path)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if existing, err := os.ReadFile(dst); err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return ErrExists
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	tmp := dst + PartialSuffix + hex.EncodeToString(rnd[:])
	if err := writeSynced(tmp, data); err != nil {
		os.Remove(tmp)
		return err
	}
	return f.retry(ctx, func() error {
		if _, err := os.Stat(dst); err == nil { // lost a race: identical or different
			if existing, rerr := os.ReadFile(dst); rerr == nil && bytes.Equal(existing, data) {
				os.Remove(tmp)
				return nil
			}
			os.Remove(tmp)
			return ErrExists
		}
		return os.Rename(tmp, dst)
	})
}

func writeSynced(path string, data []byte) error {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

// retry runs fn, backing off on errors other than ErrExists.
func (f *Folder) retry(ctx context.Context, fn func() error) error {
	wait, deadline := f.Backoff0, time.Now().Add(f.GiveUp)
	for {
		err := fn()
		if err == nil || errors.Is(err, ErrExists) {
			return err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		sl := f.sleep
		if sl == nil {
			sl = time.Sleep
		}
		sl(wait)
		if wait *= 2; wait > f.BackoffMax {
			wait = f.BackoffMax
		}
	}
}

// List returns files under prefix, skipping partial files.
func (f *Folder) List(ctx context.Context, prefix string) ([]Entry, error) {
	root, err := f.abs(prefix)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.Contains(d.Name(), PartialSuffix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(f.Root, p)
		out = append(out, Entry{Path: filepath.ToSlash(rel), Size: info.Size(), ModTime: info.ModTime(), Suspect: conflictCopy.MatchString(d.Name())})
		return nil
	})
	return out, err
}

// Stable checks that size and mtime hold across Settle and the file opens.
func (f *Folder) Stable(ctx context.Context, e Entry) (bool, error) {
	p, err := f.abs(e.Path)
	if err != nil {
		return false, err
	}
	a, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(f.Settle):
	}
	b, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	if a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) {
		return false, nil
	}
	fh, err := os.Open(p)
	if err != nil {
		return false, nil
	}
	fh.Close()
	return true, nil
}

// Get opens path for reading, or ErrNotStable.
func (f *Folder) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	p, err := f.abs(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	ok, err := f.Stable(ctx, Entry{Path: path, Size: st.Size(), ModTime: st.ModTime()})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotStable
	}
	return os.Open(p)
}
