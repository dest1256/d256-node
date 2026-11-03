// Package vault seals the node's local data (state cache, traffic journal) with XChaCha20-Poly1305.
//
// The key is derived from a secret the node already has (stage 5) or supplied by the operator
// (protected mode, stage 7). Every blob carries its own random nonce and is bound to a purpose string, so
// a sealed journal record cannot be replayed as a cache entry or the other way round.
package vault

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// ErrOpen is returned for any blob that cannot be authenticated; the cause is deliberately not detailed.
var ErrOpen = errors.New("vault: cannot open sealed data (wrong key or corrupted)")

// Sealer encrypts and authenticates blobs.
type Sealer struct {
	aead interface {
		Seal(dst, nonce, plaintext, additionalData []byte) []byte
		Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
		NonceSize() int
	}
}

// New builds a Sealer from a 32-byte key.
func New(key []byte) (*Sealer, error) {
	a, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	return &Sealer{aead: a}, nil
}

// FromSecret derives the key with HKDF-SHA256 from an arbitrary-length secret.
func FromSecret(secret []byte, info string) (*Sealer, error) {
	if len(secret) == 0 {
		return nil, errors.New("vault: empty secret")
	}
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte("d256-node/vault/v1/"+info)), key); err != nil {
		return nil, err
	}
	return New(key)
}

// Seal returns nonce||ciphertext. purpose is authenticated but not stored.
func (s *Sealer) Seal(plain []byte, purpose string) []byte {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+chacha20poly1305.Overhead)
	if _, err := rand.Read(nonce); err != nil {
		panic("vault: no randomness: " + err.Error())
	}
	return s.aead.Seal(nonce, nonce, plain, []byte(purpose))
}

// Open reverses Seal.
func (s *Sealer) Open(blob []byte, purpose string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(blob) < n+chacha20poly1305.Overhead {
		return nil, ErrOpen
	}
	plain, err := s.aead.Open(nil, blob[:n], blob[n:], []byte(purpose))
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}
