package vault

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func FuzzObjectAuthentication(f *testing.F) {
	v, err := New("audit-vault", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		f.Fatal(err)
	}
	sealed, err := v.Seal(TypeSegment, []byte("context"), []byte("history"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(sealed)
	f.Add([]byte("SMOB"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		plain, err := v.Open(TypeSegment, []byte("context"), data)
		if err != nil {
			return
		}
		if _, err := v.Open(TypeBlob, []byte("context"), data); err == nil {
			t.Fatal("authenticated as a different object type")
		}
		if _, err := v.Open(TypeSegment, []byte("other-context"), data); err == nil {
			t.Fatal("authenticated with different additional data")
		}
		for _, offset := range []int{0, len(data) / 2, len(data) - 1} {
			changed := bytes.Clone(data)
			changed[offset] ^= 1
			if _, err := v.Open(TypeSegment, []byte("context"), changed); err == nil {
				t.Fatalf("accepted tampering at byte %d", offset)
			}
		}
		round, err := v.Seal(TypeSegment, []byte("context"), plain)
		if err != nil {
			t.Fatal(err)
		}
		got, err := v.Open(TypeSegment, []byte("context"), round)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatal("authenticated plaintext did not round trip")
		}
	})
}

// Exercise only invalid parameters: a fuzz input must never allocate a large
// Argon2 workspace merely because it happens to encode a valid expensive KDF.
func FuzzBundleRejectsInvalidKDF(f *testing.F) {
	f.Add(uint32(0), uint32(65536), uint8(4))
	f.Add(uint32(3), uint32(0), uint8(4))
	f.Add(uint32(3), uint32(65536), uint8(0))
	f.Fuzz(func(t *testing.T, rounds, memory uint32, threads uint8) {
		k := BundleKDF{Alg: "argon2id", Time: rounds, MemoryKiB: memory, Threads: threads, Salt: base64.StdEncoding.EncodeToString(make([]byte, 16))}
		if k.validate() == nil {
			t.Skip()
		}
		b, err := json.Marshal(Bundle{Format: "smbundle1", VaultID: "audit-vault", KDF: k, Nonce: base64.StdEncoding.EncodeToString(make([]byte, 12)), Ciphertext: base64.StdEncoding.EncodeToString(make([]byte, 48))})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := OpenBundle(b, "audit-passphrase"); err == nil {
			t.Fatal("accepted an invalid KDF")
		}
	})
}
