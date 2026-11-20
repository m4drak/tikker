// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestNewToken(t *testing.T) {
	re := regexp.MustCompile(`^tkr_[A-Za-z0-9]{8}_[A-Za-z0-9]{32}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		full, prefix, hash, err := newToken(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(full) || !strings.HasPrefix(full, "tkr_"+prefix+"_") {
			t.Fatalf("token %q prefix %q", full, prefix)
		}
		p, secret, err := ParseToken(full)
		if err != nil || p != prefix {
			t.Fatalf("ParseToken(%q) = %q, %v", full, p, err)
		}
		want := sha256.Sum256([]byte(secret))
		if !bytes.Equal(hash, want[:]) {
			t.Fatal("hash is not SHA-256 of the secret")
		}
		if seen[full] {
			t.Fatal("duplicate token")
		}
		seen[full] = true
	}
	if _, _, _, err := newToken(failReader{}); err == nil {
		t.Fatal("newToken with failing entropy succeeded")
	}
}

// cycleReader yields 0, 1, ..., 255, 0, 1, ... forever.
type cycleReader struct{ n byte }

func (c *cycleReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = c.n
		c.n++
	}
	return len(p), nil
}

// TestRandomAlnumUnbiased feeds every byte value and checks that only the
// unbiased range is used and that all 62 characters occur equally.
func TestRandomAlnumUnbiased(t *testing.T) {
	s, err := randomAlnum(&cycleReader{}, 248*4)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[rune]int{}
	for _, c := range s {
		counts[c]++
	}
	if len(counts) != 62 {
		t.Fatalf("%d distinct characters", len(counts))
	}
	for c, n := range counts {
		if n != 16 { // 4 bytes per character per round, 4 rounds
			t.Fatalf("character %q occurs %d times", c, n)
		}
	}
}

func TestParseToken(t *testing.T) {
	valid := "tkr_k7Qm2xPa_V9pL3rT8wZ1nB6cX4yH0sD2fG5jK7mQ9" // API contract example
	tests := []struct {
		name, in string
		ok       bool
	}{
		{"valid", valid, true},
		{"empty", "", false},
		{"no marker", "abc_k7Qm2xPa_V9pL3rT8wZ1nB6cX4yH0sD2fG5jK7mQ9", false},
		{"upper marker", "TKR_k7Qm2xPa_V9pL3rT8wZ1nB6cX4yH0sD2fG5jK7mQ9", false},
		{"short secret", valid[:len(valid)-1], false},
		{"long secret", valid + "A", false},
		{"short prefix", "tkr_k7Qm2xP_V9pL3rT8wZ1nB6cX4yH0sD2fG5jK7mQ9A", false},
		{"separator moved", "tkr_k7Qm2xPaV_9pL3rT8wZ1nB6cX4yH0sD2fG5jK7mQ9", false},
		{"symbol in secret", "tkr_k7Qm2xPa_V9pL3rT8wZ1nB6cX4yH0sD2fG5jK7mQ-", false},
		{"underscore in secret", "tkr_k7Qm2xPa_V9pL3rT8wZ1nB6cX4yH0sD2fG5jK7m_9", false},
		{"symbol in prefix", "tkr_k7Qm2xP!_V9pL3rT8wZ1nB6cX4yH0sD2fG5jK7mQ9", false},
		{"non-ascii", "tkr_k7Qm2xPą_V9pL3rT8wZ1nB6cX4yH0sD2fG5jK7mQ", false},
		{"bearer included", "Bearer " + valid, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prefix, secret, err := ParseToken(tc.in)
			if tc.ok {
				if err != nil || prefix != "k7Qm2xPa" || secret != "V9pL3rT8wZ1nB6cX4yH0sD2fG5jK7mQ9" {
					t.Fatalf("ParseToken = %q %q %v", prefix, secret, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("ParseToken(%q) err = %v", tc.in, err)
			}
		})
	}
}
