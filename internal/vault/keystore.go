package vault

import (
	"encoding/base64"
	"errors"

	"github.com/zalando/go-keyring"
)

// KeyService is the credential-store service name.
const KeyService = "session-memory"

// ErrNoKey means the credential store has no key for the vault.
var ErrNoKey = errors.New("vault: no key in the credential store")

// KeyStore keeps the vault key outside any file (spec §6.5). There is no
// plaintext-file implementation by design.
type KeyStore interface {
	Load(vaultID string) ([]byte, error)
	Store(vaultID string, key []byte) error
}

type keyringStore struct{}

// NewKeyring returns the OS credential store implementation.
func NewKeyring() KeyStore { return keyringStore{} }

func (keyringStore) Load(id string) ([]byte, error) {
	s, err := keyring.Get(KeyService, id)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(s)
}

func (keyringStore) Store(id string, key []byte) error {
	return keyring.Set(KeyService, id, base64.StdEncoding.EncodeToString(key))
}

// MemStore is an in-memory KeyStore for tests.
type MemStore map[string][]byte

func (m MemStore) Load(id string) ([]byte, error) {
	if k, ok := m[id]; ok {
		return k, nil
	}
	return nil, ErrNoKey
}

func (m MemStore) Store(id string, key []byte) error { m[id] = append([]byte(nil), key...); return nil }
