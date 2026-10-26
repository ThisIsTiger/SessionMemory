package vault

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

func fixedKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

const testVaultID = "vlt_aaaaaaaaaaaaaaaaaaaaaaaaaa"

func fixedSaltNonce() (salt, nonce []byte) {
	salt = bytes.Repeat([]byte{0xa0}, 32)
	nonce = bytes.Repeat([]byte{0xb0}, 12)
	return
}

func TestRoundTripAndBindings(t *testing.T) {
	v, _ := New(testVaultID, fixedKey())
	sealed, err := v.Seal(TypeSegment, nil, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v.Open(TypeSegment, nil, sealed); err != nil || string(got) != "hello" {
		t.Fatalf("Open = %q %v", got, err)
	}
	if _, err := v.Open(TypeBlob, nil, sealed); !errors.Is(err, ErrAuth) {
		t.Error("an object opened as another type")
	}
	if _, err := v.Open(TypeSegment, []byte("x"), sealed); !errors.Is(err, ErrAuth) {
		t.Error("extra AAD was ignored")
	}
	other, _ := New("vlt_bbbbbbbbbbbbbbbbbbbbbbbbbb", fixedKey())
	if _, err := other.Open(TypeSegment, nil, sealed); !errors.Is(err, ErrAuth) {
		t.Error("an object opened under another vault id")
	}
	for i := range sealed {
		bad := append([]byte(nil), sealed...)
		bad[i] ^= 1
		if _, err := v.Open(TypeSegment, nil, bad); err == nil {
			t.Fatalf("flipping byte %d was not detected", i)
		}
	}
	if _, err := v.Open(TypeSegment, nil, sealed[:10]); err == nil {
		t.Error("truncated object accepted")
	}
}

func TestSealVector(t *testing.T) {
	v, _ := New(testVaultID, fixedKey())
	salt, nonce := fixedSaltNonce()
	sealed, err := v.sealWith(TypeSegment, nil, []byte("hello"), salt, nonce)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SEAL_VECTOR_HEX=%s", hex.EncodeToString(sealed))
	t.Logf("SEAL_LEN=%d", len(sealed))
	// manifest-style extra AAD
	m, _ := v.sealWith(TypeManifest, ManifestAAD("dev_bbbbbbbbbbbbbbbb", 7, "00"), []byte("{}"), salt, nonce)
	t.Logf("MANIFEST_VECTOR_HEX=%s", hex.EncodeToString(m))
	t.Logf("MANIFEST_AAD_HEX=%s", hex.EncodeToString(ManifestAAD("dev_bbbbbbbbbbbbbbbb", 7, "00")))
	if got, err := v.Open(TypeSegment, nil, sealed); err != nil || string(got) != "hello" {
		t.Fatalf("vector does not open: %v", err)
	}
}

func TestManifestAADBindsSequence(t *testing.T) {
	v, _ := New(testVaultID, fixedKey())
	sealed, _ := v.Seal(TypeManifest, ManifestAAD("dev_bbbbbbbbbbbbbbbb", 7, "ab"), []byte("{}"))
	if _, err := v.Open(TypeManifest, ManifestAAD("dev_bbbbbbbbbbbbbbbb", 7, "ab"), sealed); err != nil {
		t.Fatal(err)
	}
	for _, aad := range [][]byte{
		ManifestAAD("dev_bbbbbbbbbbbbbbbb", 8, "ab"),
		ManifestAAD("dev_cccccccccccccccc", 7, "ab"),
		ManifestAAD("dev_bbbbbbbbbbbbbbbb", 7, "ac"),
	} {
		if _, err := v.Open(TypeManifest, aad, sealed); err == nil {
			t.Error("a manifest opened under different device/sequence/prev")
		}
	}
}

func TestKeyCheck(t *testing.T) {
	v, _ := New(testVaultID, fixedKey())
	info, err := v.NewInfo("2026-10-26T10:00:00.000Z")
	if err != nil || !v.CheckKey(info) {
		t.Fatalf("own key check failed: %v", err)
	}
	k := fixedKey()
	k[0] ^= 1
	wrong, _ := New(testVaultID, k)
	if wrong.CheckKey(info) {
		t.Error("a wrong key passed the key check")
	}
}

func TestBundle(t *testing.T) {
	salt := bytes.Repeat([]byte{0xc0}, 16)
	nonce := bytes.Repeat([]byte{0xd0}, 12)
	kdf := DefaultKDF
	kdf.MemoryKiB = 1024 // keep the test fast; real bundles use DefaultKDF
	kdf.Time = 1
	data, err := createBundleWith(testVaultID, fixedKey(), "correct horse battery", kdf, salt, nonce)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("BUNDLE=%s", data)
	id, key, err := OpenBundle(data, "correct horse battery")
	if err != nil || id != testVaultID || !bytes.Equal(key, fixedKey()) {
		t.Fatalf("OpenBundle = %q %x %v", id, key, err)
	}
	if _, _, err := OpenBundle(data, "wrong passphrase!!"); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if _, err := CreateBundle(testVaultID, fixedKey(), "short"); err == nil {
		t.Error("a short passphrase was accepted")
	}
}
