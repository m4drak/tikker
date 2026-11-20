// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package secrets

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func newRing(t testing.TB, b byte, retired ...[]byte) *Keyring {
	t.Helper()
	r, err := NewKeyring(testKey(b), retired...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSealOpenRoundTrip(t *testing.T) {
	ring := newRing(t, 1)
	tests := []struct {
		name string
		pt   string
	}{
		{"empty", ""},
		{"short", "x"},
		{"typical", "S3cret-router-pass"},
		{"unicode", "pąśword-🔑"},
		{"long", string(bytes.Repeat([]byte("a"), 10000))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sealed, err := ring.Seal("p", "rec", []byte(tt.pt))
			if err != nil {
				t.Fatal(err)
			}
			if sealed[0] != VersionV1 {
				t.Fatalf("version = %d", sealed[0])
			}
			// Only check plaintexts long enough that a random match in the
			// nonce/ciphertext is negligible (a 1-byte "x" appears by chance
			// in ~12% of 33 random bytes).
			if len(tt.pt) >= 8 && bytes.Contains(sealed, []byte(tt.pt)) {
				t.Fatal("plaintext visible in ciphertext")
			}
			got, err := ring.Open("p", "rec", sealed)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.pt {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	ring := newRing(t, 1)
	a, _ := ring.Seal("p", "r", []byte("same"))
	b, _ := ring.Seal("p", "r", []byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("two seals are identical")
	}
	if bytes.Equal(a[headerSize:headerSize+nonceSize], b[headerSize:headerSize+nonceSize]) {
		t.Fatal("nonce reused")
	}
}

func TestOpenFailures(t *testing.T) {
	ring := newRing(t, 1)
	sealed, err := ring.Seal("device-password", "dev1", []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte {
		c := bytes.Clone(sealed)
		c[i] ^= 0x01
		return c
	}
	tests := []struct {
		name    string
		ring    *Keyring
		purpose string
		record  string
		in      []byte
		want    error
	}{
		{"wrong key", newRing(t, 2), "device-password", "dev1", sealed, ErrUnknownKey},
		{"other record", ring, "device-password", "dev2", sealed, ErrDecrypt},
		{"other purpose", ring, "other", "dev1", sealed, ErrDecrypt},
		{"flipped nonce", ring, "device-password", "dev1", flip(headerSize), ErrDecrypt},
		{"flipped ciphertext", ring, "device-password", "dev1", flip(headerSize + nonceSize), ErrDecrypt},
		{"flipped tag", ring, "device-password", "dev1", flip(len(sealed) - 1), ErrDecrypt},
		{"flipped key id", ring, "device-password", "dev1", flip(1), ErrUnknownKey},
		{"unknown version", ring, "device-password", "dev1", flip(0), ErrUnknownVersion},
		{"version 0", ring, "device-password", "dev1", append([]byte{0}, sealed[1:]...), ErrUnknownVersion},
		{"truncated", ring, "device-password", "dev1", sealed[:len(sealed)-1], ErrDecrypt},
		{"too short", ring, "device-password", "dev1", sealed[:minCiphertext-1], ErrMalformed},
		{"empty", ring, "device-password", "dev1", nil, ErrMalformed},
		{"legacy plaintext", ring, "device-password", "dev1", []byte("secret-alpha"), ErrUnknownVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.ring.Open(tt.purpose, tt.record, tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestKeyringRotationOpensRetired(t *testing.T) {
	old := newRing(t, 1)
	sealed, _ := old.Seal("p", "r", []byte("pw"))
	next, err := NewKeyring(testKey(2), testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	got, err := next.Open("p", "r", sealed)
	if err != nil || string(got) != "pw" {
		t.Fatalf("got %q, %v", got, err)
	}
	fresh, _ := next.Seal("p", "r", []byte("pw"))
	id, err := KeyIDOf(fresh)
	if err != nil || id != next.KeyID() || id == old.KeyID() {
		t.Fatalf("key id %q (%v), active %q, old %q", id, err, next.KeyID(), old.KeyID())
	}
}

func TestNewKeyringRejectsBadKeys(t *testing.T) {
	if _, err := NewKeyring(make([]byte, 16)); err == nil {
		t.Fatal("short active key accepted")
	}
	if _, err := NewKeyring(testKey(1), make([]byte, 5)); err == nil {
		t.Fatal("short retired key accepted")
	}
}

func TestKeyIDOfErrors(t *testing.T) {
	if _, err := KeyIDOf(nil); !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v", err)
	}
	if _, err := KeyIDOf([]byte{9}); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("err = %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestSealRandFailure(t *testing.T) {
	ring := newRing(t, 1)
	ring.rand = errReader{}
	if _, err := ring.Seal("p", "r", []byte("x")); err == nil {
		t.Fatal("expected error")
	}
}

func FuzzOpen(f *testing.F) {
	ring := newRing(f, 1)
	// Deterministic nonce: fuzz workers rebuild this state in every process,
	// and the seed must be the same ciphertext in all of them.
	ring.rand = bytes.NewReader(bytes.Repeat([]byte{7}, nonceSize))
	good, _ := ring.Seal("device-password", "dev1", []byte("pw"))
	f.Add(good, "dev1")
	f.Add([]byte{}, "")
	f.Add([]byte{VersionV1}, "x")
	f.Add(good[:headerSize], "dev1")
	f.Fuzz(func(t *testing.T, in []byte, record string) {
		pt, err := ring.Open("device-password", record, in)
		if err == nil && (!bytes.Equal(in, good) || record != "dev1") {
			t.Fatalf("unexpected success: %q", pt)
		}
	})
}
