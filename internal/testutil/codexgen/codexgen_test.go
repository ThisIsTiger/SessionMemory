package codexgen

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDeterministicAndWellFormed(t *testing.T) {
	o := Options{Seed: 1, Turns: 3, ToolCalls: 2, Ordinals: true, UILayer: true, Compacted: true, Images: true}
	a, b := Rollout(o), Rollout(o)
	if !bytes.Equal(a, b) {
		t.Fatal("same options gave different bytes")
	}
	if bytes.Equal(a, Rollout(Options{Seed: 2, Turns: 3})) {
		t.Fatal("different seeds gave identical bytes")
	}
	if a[len(a)-1] != '\n' {
		t.Fatal("must end with newline")
	}
	d, _ := zstd.NewReader(nil)
	out, err := d.DecodeAll(Zstd(a), nil)
	if err != nil || !bytes.Equal(out, a) {
		t.Fatalf("zstd round trip failed: %v", err)
	}
}
