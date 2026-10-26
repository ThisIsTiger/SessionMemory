// Package vault seals and opens the objects that leave the device, and builds
// the passphrase-protected bundle used to hand the vault key to a new device.
//
// Trust model (design §12): every device holding the vault key is equally
// trusted. There are no per-device keys, signatures or authorization records;
// authenticity of every synced byte comes from AES-256-GCM under a key only
// vault-key holders can derive.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// ObjectType is stored in the envelope header and bound into the AAD, so a
// sealed object cannot be replayed as another kind.
type ObjectType uint8

const (
	TypeSnapshot  ObjectType = 1
	TypeSegment   ObjectType = 2
	TypeBlob      ObjectType = 3
	TypeRepoState ObjectType = 4
	TypeManifest  ObjectType = 5
	TypeReport    ObjectType = 6
	TypeKeyCheck  ObjectType = 7
)

const (
	magic       = "SMOB"
	version     = 1
	saltLen     = 32
	nonceLen    = 12
	headerLen   = len(magic) + 1 + 1 + saltLen + nonceLen // 50
	keyLen      = 32
	tagLen      = 16
	keyCheckMsg = "session-memory vault key check v1"
)

// Sealer is what the rest of the program depends on; *Vault implements it, and
// tests of the store before M4 use a pass-through implementation behind a
// build tag that is never compiled into release binaries.
type Sealer interface {
	ID() string
	Seal(t ObjectType, extra, plain []byte) ([]byte, error)
	Open(t ObjectType, extra, sealed []byte) ([]byte, error)
}

var _ Sealer = (*Vault)(nil)

// MaxPlainObject is the largest plaintext accepted by Seal and Open: single-shot
// AEAD keeps whole objects in memory, so adapters must chunk (design §8.2).
const MaxPlainObject = 16 << 20

// ErrAuth means the object failed authentication: wrong vault, wrong type,
// tampered bytes or wrong extra AAD. Callers quarantine the file.
var ErrAuth = errors.New("vault: authentication failed")

// Vault holds the vault key in memory.
type Vault struct {
	id  string
	key [keyLen]byte
}

// New creates a Vault from the vault ID and its 32-byte key.
func New(id string, key []byte) (*Vault, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("vault: key must be %d bytes", keyLen)
	}
	v := &Vault{id: id}
	copy(v.key[:], key)
	return v, nil
}

// GenerateKey returns a new random vault key.
func GenerateKey() ([]byte, error) {
	k := make([]byte, keyLen)
	_, err := rand.Read(k)
	return k, err
}

func (v *Vault) ID() string { return v.id }

// objectKey = HKDF-SHA256(vaultKey, salt, "smv1/object/<vault_id>/<type>").
func (v *Vault) objectKey(salt []byte, t ObjectType) ([]byte, error) {
	info := fmt.Sprintf("smv1/object/%s/%d", v.id, t)
	return hkdf.Key(sha256.New, v.key[:], salt, info, keyLen)
}

// aad = header(magic|version|type) || 0x00 || vault_id || 0x00 || extra.
func (v *Vault) aad(t ObjectType, extra []byte) []byte {
	a := append([]byte(magic), version, byte(t), 0)
	a = append(a, v.id...)
	a = append(a, 0)
	return append(a, extra...)
}

// Seal encrypts plain. extra is additional authenticated data the reader must
// supply again (manifests bind device_id, sequence and the previous hash).
// Layout: "SMOB" | version u8 | type u8 | salt[32] | nonce[12] | ciphertext+tag.
func (v *Vault) Seal(t ObjectType, extra, plain []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return v.sealWith(t, extra, plain, salt, nonce)
}

func (v *Vault) sealWith(t ObjectType, extra, plain, salt, nonce []byte) ([]byte, error) {
	if len(plain) > MaxPlainObject {
		return nil, fmt.Errorf("vault: plaintext %d bytes exceeds %d", len(plain), MaxPlainObject)
	}
	key, err := v.objectKey(salt, t)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, headerLen+len(plain)+tagLen)
	out = append(out, magic...)
	out = append(out, version, byte(t))
	out = append(out, salt...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plain, v.aad(t, extra)), nil
}

// Open verifies and decrypts a sealed object of the expected type.
func (v *Vault) Open(t ObjectType, extra, sealed []byte) ([]byte, error) {
	if len(sealed) < headerLen+tagLen || string(sealed[:len(magic)]) != magic {
		return nil, ErrAuth
	}
	if sealed[len(magic)] != version || ObjectType(sealed[len(magic)+1]) != t {
		return nil, ErrAuth
	}
	salt := sealed[len(magic)+2 : len(magic)+2+saltLen]
	nonce := sealed[len(magic)+2+saltLen : headerLen]
	key, err := v.objectKey(salt, t)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, sealed[headerLen:], v.aad(t, extra))
	if err != nil {
		return nil, ErrAuth
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// ManifestAAD is the extra AAD of a manifest: device_id, sequence and the
// previous manifest hash, each preceded by 0x00.
func ManifestAAD(deviceID string, sequence int64, prevHash string) []byte {
	var seq [8]byte
	binary.BigEndian.PutUint64(seq[:], uint64(sequence))
	a := append([]byte{0}, deviceID...)
	a = append(a, 0)
	a = append(a, seq[:]...)
	a = append(a, 0)
	return append(a, prevHash...)
}

// ---- vault.json (public) ----

// Info is public/vault.json: not secret, readable without the key. KeyCheck
// lets a joining device verify the key before touching any data.
type Info struct {
	SchemaVersion int    `json:"schema_version"`
	VaultID       string `json:"vault_id"`
	CreatedAt     string `json:"created_at"`
	KeyCheck      string `json:"key_check"` // base64 of Seal(TypeKeyCheck, nil, keyCheckMsg)
}

// NewInfo builds vault.json for a new vault.
func (v *Vault) NewInfo(createdAt string) (Info, error) {
	sealed, err := v.Seal(TypeKeyCheck, nil, []byte(keyCheckMsg))
	if err != nil {
		return Info{}, err
	}
	return Info{SchemaVersion: 1, VaultID: v.id, CreatedAt: createdAt, KeyCheck: base64.StdEncoding.EncodeToString(sealed)}, nil
}

// CheckKey reports whether the key matches vault.json.
func (v *Vault) CheckKey(info Info) bool {
	if info.VaultID != v.id {
		return false
	}
	sealed, err := base64.StdEncoding.DecodeString(info.KeyCheck)
	if err != nil {
		return false
	}
	plain, err := v.Open(TypeKeyCheck, nil, sealed)
	return err == nil && string(plain) == keyCheckMsg
}

// ---- device join bundle ----

// Bundle is the file handed to a new device (and the offline recovery file):
// the vault key encrypted under a passphrase-derived key.
type Bundle struct {
	Format     string    `json:"format"` // "smbundle1"
	VaultID    string    `json:"vault_id"`
	KDF        BundleKDF `json:"kdf"`
	Nonce      string    `json:"nonce"`      // base64, 12 bytes
	Ciphertext string    `json:"ciphertext"` // base64, AES-256-GCM(vault key) + tag
}

// BundleKDF are the argon2id parameters, stored so they can be raised later.
type BundleKDF struct {
	Alg       string `json:"alg"` // "argon2id"
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
	Salt      string `json:"salt"` // base64, 16 bytes
}

// DefaultKDF are the parameters used when creating a bundle.
var DefaultKDF = BundleKDF{Alg: "argon2id", Time: 3, MemoryKiB: 64 * 1024, Threads: 4}

// MinPassphraseLen is enforced when creating a bundle.
const MinPassphraseLen = 12

// CreateBundle encrypts the vault key under passphrase.
func CreateBundle(vaultID string, vaultKey []byte, passphrase string) ([]byte, error) {
	if len(passphrase) < MinPassphraseLen {
		return nil, fmt.Errorf("vault: passphrase must be at least %d characters", MinPassphraseLen)
	}
	kdf := DefaultKDF
	salt := make([]byte, 16)
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return createBundleWith(vaultID, vaultKey, passphrase, kdf, salt, nonce)
}

func createBundleWith(vaultID string, vaultKey []byte, passphrase string, kdf BundleKDF, salt, nonce []byte) ([]byte, error) {
	kdf.Salt = base64.StdEncoding.EncodeToString(salt)
	pk := argon2.IDKey([]byte(passphrase), salt, kdf.Time, kdf.MemoryKiB, kdf.Threads, keyLen)
	gcm, err := newGCM(pk)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, vaultKey, []byte("smbundle1\x00"+vaultID))
	return json.MarshalIndent(Bundle{
		Format: "smbundle1", VaultID: vaultID, KDF: kdf,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}, "", "  ")
}

// OpenBundle returns the vault ID and key. A wrong passphrase yields ErrAuth.
func OpenBundle(data []byte, passphrase string) (vaultID string, vaultKey []byte, err error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return "", nil, err
	}
	if b.Format != "smbundle1" || b.KDF.Alg != "argon2id" {
		return "", nil, errors.New("vault: unsupported bundle format")
	}
	salt, err1 := base64.StdEncoding.DecodeString(b.KDF.Salt)
	nonce, err2 := base64.StdEncoding.DecodeString(b.Nonce)
	ct, err3 := base64.StdEncoding.DecodeString(b.Ciphertext)
	if err1 != nil || err2 != nil || err3 != nil || len(nonce) != nonceLen {
		return "", nil, errors.New("vault: malformed bundle")
	}
	pk := argon2.IDKey([]byte(passphrase), salt, b.KDF.Time, b.KDF.MemoryKiB, b.KDF.Threads, keyLen)
	gcm, err := newGCM(pk)
	if err != nil {
		return "", nil, err
	}
	key, err := gcm.Open(nil, nonce, ct, []byte("smbundle1\x00"+b.VaultID))
	if err != nil {
		return "", nil, ErrAuth
	}
	return b.VaultID, key, nil
}
