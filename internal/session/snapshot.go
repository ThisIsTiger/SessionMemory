package session

import (
	"fmt"
	"sort"
	"strings"
)

// ValidArtifactPath enforces spec §2.2 rule 1: relative, slash-separated, no
// "..", no drive letter, no backslash or NUL, no empty component.
func ValidArtifactPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == ".." || part == "." {
			return false
		}
	}
	if len(p) >= 2 && p[1] == ':' {
		return false
	}
	return true
}

// Validate checks a snapshot's structure: sorted unique paths, valid kinds,
// contiguous segments starting at 0, matching size and digest, valid refs.
func (s Snapshot) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("snapshot: unsupported schema_version %d", s.SchemaVersion)
	}
	if !sort.SliceIsSorted(s.Artifacts, func(i, j int) bool { return s.Artifacts[i].Path < s.Artifacts[j].Path }) {
		return fmt.Errorf("snapshot: artifacts not sorted by path")
	}
	seen := map[string]bool{}
	for _, a := range s.Artifacts {
		if !ValidArtifactPath(a.Path) || seen[a.Path] {
			return fmt.Errorf("snapshot: bad or duplicate path %q", a.Path)
		}
		seen[a.Path] = true
		if a.Kind != KindStream && a.Kind != KindBlob {
			return fmt.Errorf("snapshot: bad kind %q", a.Kind)
		}
		var next int64
		for _, sg := range a.Segments {
			if sg.Start != next || sg.End <= sg.Start || !ValidObjectRef(sg.Object) || len(sg.RawHash) != 64 {
				return fmt.Errorf("snapshot: bad segment in %q", a.Path)
			}
			next = sg.End
		}
		if a.Size != next || a.Digest != SegmentsDigest(a.Segments) {
			return fmt.Errorf("snapshot: size or digest mismatch in %q", a.Path)
		}
		if a.Generation < 0 {
			return fmt.Errorf("snapshot: negative generation")
		}
	}
	return nil
}

// ObjectRefs lists every object the snapshot references (segments only).
func (s Snapshot) ObjectRefs() []string {
	var out []string
	for _, a := range s.Artifacts {
		for _, sg := range a.Segments {
			out = append(out, sg.Object)
		}
	}
	return out
}
