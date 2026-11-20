// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
)

// API token format: "tkr_<prefix>_<secret>", both parts over [A-Za-z0-9].
//
// The prefix is public (shown in the UI and token lists) and only locates
// the row; 8 characters (~47.6 bits) make accidental collisions negligible
// for any realistic number of tokens, and a collision on creation is simply
// retried. The secret has 32 characters, ~190.5 bits of entropy, well above
// the 128 bits needed against online and offline guessing, so a single
// unsalted SHA-256 is an adequate verifier (no password stretching needed).
const (
	TokenMarker    = "tkr_"
	TokenPrefixLen = 8
	TokenSecretLen = 32
	tokenLen       = len(TokenMarker) + TokenPrefixLen + 1 + TokenSecretLen
)

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomAlnum returns n characters drawn uniformly from [A-Za-z0-9] using
// rejection sampling (bytes >= 248 are discarded, so there is no modulo
// bias).
func randomAlnum(r io.Reader, n int) (string, error) {
	const limit = 256 - 256%len(alnum) // 248
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/4+8)
	for len(out) < n {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("auth: random token: %w", err)
		}
		for _, b := range buf {
			if int(b) < limit {
				out = append(out, alnum[int(b)%len(alnum)])
				if len(out) == n {
					break
				}
			}
		}
	}
	return string(out), nil
}

// newToken generates a token and returns the full string (shown once), the
// public prefix and the SHA-256 of the secret (stored).
func newToken(r io.Reader) (full, prefix string, hash []byte, err error) {
	prefix, err = randomAlnum(r, TokenPrefixLen)
	if err != nil {
		return "", "", nil, err
	}
	secret, err := randomAlnum(r, TokenSecretLen)
	if err != nil {
		return "", "", nil, err
	}
	return TokenMarker + prefix + "_" + secret, prefix, hashSecret(secret), nil
}

// hashSecret returns the stored verifier of a token secret.
func hashSecret(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// ParseToken splits "tkr_<prefix>_<secret>" and checks lengths and
// alphabet. Any deviation yields [ErrInvalidToken].
func ParseToken(s string) (prefix, secret string, err error) {
	if len(s) != tokenLen || !strings.HasPrefix(s, TokenMarker) {
		return "", "", ErrInvalidToken
	}
	rest := s[len(TokenMarker):]
	prefix, secret = rest[:TokenPrefixLen], rest[TokenPrefixLen+1:]
	if rest[TokenPrefixLen] != '_' || !isAlnum(prefix) || !isAlnum(secret) {
		return "", "", ErrInvalidToken
	}
	return prefix, secret, nil
}

func isAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
