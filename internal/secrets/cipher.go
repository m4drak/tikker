// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

const (
	// VersionV1 is the first ciphertext format.
	VersionV1 byte = 1

	// KeySize is the AES-256 key length.
	KeySize = 32

	keyIDSize  = 4
	nonceSize  = 12
	tagSize    = 16
	headerSize = 1 + keyIDSize
	// minCiphertext is the shortest valid sealed value (empty plaintext).
	minCiphertext = headerSize + nonceSize + tagSize
)

// Errors returned by [Keyring.Open]. They never contain key or plaintext data.
var (
	// ErrMalformed means the value is too short to be a sealed secret.
	ErrMalformed = errors.New("secrets: malformed ciphertext")
	// ErrUnknownVersion means the format version is not supported.
	ErrUnknownVersion = errors.New("secrets: unknown ciphertext version")
	// ErrUnknownKey means the value was sealed with a key the keyring does not
	// hold: the secret key is new or different from the one used before.
	ErrUnknownKey = errors.New("secrets: ciphertext was sealed with a different key")
	// ErrDecrypt means authentication failed: the ciphertext was modified, or
	// it belongs to another record or purpose.
	ErrDecrypt = errors.New("secrets: decryption failed")
)

type gcmKey struct {
	id  [keyIDSize]byte
	gcm cipher.AEAD
}

// Keyring seals with its active key and opens with any key it holds. The
// MVP uses a single key; retired keys prepare for rotation. It is safe for
// concurrent use.
type Keyring struct {
	active gcmKey
	byID   map[[keyIDSize]byte]gcmKey
	rand   io.Reader
}

// NewKeyring builds a keyring from a 32-byte active key and optional retired
// keys, which are only used to open old values.
func NewKeyring(active []byte, retired ...[]byte) (*Keyring, error) {
	a, err := newGCMKey(active)
	if err != nil {
		return nil, err
	}
	k := &Keyring{active: a, byID: map[[keyIDSize]byte]gcmKey{a.id: a}, rand: rand.Reader}
	for _, r := range retired {
		g, err := newGCMKey(r)
		if err != nil {
			return nil, err
		}
		k.byID[g.id] = g
	}
	return k, nil
}

func newGCMKey(key []byte) (gcmKey, error) {
	if len(key) != KeySize {
		return gcmKey{}, fmt.Errorf("secrets: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return gcmKey{}, fmt.Errorf("secrets: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return gcmKey{}, fmt.Errorf("secrets: create gcm: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("tikker/key-id"))
	var id [keyIDSize]byte
	copy(id[:], mac.Sum(nil))
	return gcmKey{id: id, gcm: gcm}, nil
}

// KeyID returns the ID of the active key (hex); it is safe to log.
func (k *Keyring) KeyID() string { return fmt.Sprintf("%x", k.active.id[:]) }

func aad(header []byte, purpose, recordID string) []byte {
	b := make([]byte, 0, len(header)+len(purpose)+len(recordID)+2)
	b = append(b, header...)
	b = append(b, 0)
	b = append(b, purpose...)
	b = append(b, 0)
	b = append(b, recordID...)
	return b
}

// Seal encrypts plaintext with the active key. purpose names the kind of
// secret (for example "device-password") and recordID the owning record; both
// are authenticated and must be passed again to [Keyring.Open].
func (k *Keyring) Seal(purpose, recordID string, plaintext []byte) ([]byte, error) {
	out := make([]byte, headerSize+nonceSize, headerSize+nonceSize+len(plaintext)+tagSize)
	out[0] = VersionV1
	copy(out[1:headerSize], k.active.id[:])
	nonce := out[headerSize : headerSize+nonceSize]
	if _, err := io.ReadFull(k.rand, nonce); err != nil {
		return nil, fmt.Errorf("secrets: generate nonce: %w", err)
	}
	return k.active.gcm.Seal(out, nonce, plaintext, aad(out[:headerSize], purpose, recordID)), nil
}

// KeyIDOf extracts the key ID from a sealed value without decrypting it.
func KeyIDOf(sealed []byte) (string, error) {
	if err := checkHeader(sealed); err != nil {
		return "", err
	}
	var id [keyIDSize]byte
	copy(id[:], sealed[1:])
	return fmt.Sprintf("%x", id[:]), nil
}

func checkHeader(sealed []byte) error {
	if len(sealed) < 1 {
		return ErrMalformed
	}
	if sealed[0] != VersionV1 {
		return fmt.Errorf("%w: %d", ErrUnknownVersion, sealed[0])
	}
	if len(sealed) < minCiphertext {
		return ErrMalformed
	}
	return nil
}

// Open decrypts a value produced by [Keyring.Seal] with the same purpose and
// recordID.
func (k *Keyring) Open(purpose, recordID string, sealed []byte) ([]byte, error) {
	if err := checkHeader(sealed); err != nil {
		return nil, err
	}
	var id [keyIDSize]byte
	copy(id[:], sealed[1:headerSize])
	key, ok := k.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w (value key id %x, active key id %s)", ErrUnknownKey, id[:], k.KeyID())
	}
	nonce := sealed[headerSize : headerSize+nonceSize]
	pt, err := key.gcm.Open(nil, nonce, sealed[headerSize+nonceSize:], aad(sealed[:headerSize], purpose, recordID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}
