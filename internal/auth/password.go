// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id cost parameters.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // passes
	Threads uint8  // lanes
	SaltLen uint32 // bytes
	KeyLen  uint32 // bytes
}

// DefaultParams follow the first configuration recommended by the OWASP
// Password Storage Cheat Sheet for argon2id: 19 MiB of memory, 2 passes,
// 1 lane. It is the cheapest OWASP option in memory per unit of attacker
// cost that still uses at least 2 passes; on a router the memory per
// hash matters more than CPU time, and [DefaultHashConcurrency] bounds the
// peak at about 38 MiB. A 16-byte salt and a 32-byte key are the values
// RFC 9106 recommends.
var DefaultParams = Params{Memory: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

// DefaultHashConcurrency is how many argon2id computations may run at once.
// Further logins wait (bounded by the request context), so a burst of login
// attempts cannot exhaust the memory of a router.
const DefaultHashConcurrency = 2

// Password length limits. The minimum matches SPEC §8 (TIKKER_ADMIN_PASSWORD)
// and the API contract; the maximum bounds hashing work and matches the
// contract's maxLength.
const (
	MinPasswordLen = 12   // characters (runes)
	MaxPasswordLen = 1024 // bytes
)

// Upper bounds accepted when parsing a stored hash, so that a corrupted or
// malicious row cannot make verification allocate gigabytes.
const (
	maxMemory  = 256 * 1024 // KiB
	maxTime    = 16
	maxThreads = 16
	minKeyLen  = 16
	maxKeyLen  = 64
	minSaltLen = 8
	maxSaltLen = 64
)

var b64 = base64.RawStdEncoding

// Hasher hashes and verifies passwords with argon2id, encoding the result
// in the PHC string format $argon2id$v=19$m=…,t=…,p=…$salt$hash. It is safe
// for concurrent use.
type Hasher struct {
	params Params
	sem    chan struct{}
	rand   io.Reader
	dummy  string // hash of a random password, for timing equalisation
}

// NewHasher returns a hasher with params (zero value: [DefaultParams]) that
// runs at most concurrency computations at once (<= 0:
// [DefaultHashConcurrency]). It computes one hash to prepare
// [Hasher.VerifyDummy].
func NewHasher(params Params, concurrency int, rnd io.Reader) (*Hasher, error) {
	if params == (Params{}) {
		params = DefaultParams
	}
	if err := params.validate(); err != nil {
		return nil, err
	}
	if concurrency <= 0 {
		concurrency = DefaultHashConcurrency
	}
	if rnd == nil {
		rnd = rand.Reader
	}
	h := &Hasher{params: params, sem: make(chan struct{}, concurrency), rand: rnd}
	pw := make([]byte, 32)
	if _, err := io.ReadFull(rnd, pw); err != nil {
		return nil, fmt.Errorf("auth: random dummy password: %w", err)
	}
	dummy, err := h.Hash(context.Background(), b64.EncodeToString(pw))
	if err != nil {
		return nil, err
	}
	h.dummy = dummy
	return h, nil
}

func (p Params) validate() error {
	switch {
	case p.Memory < 8*uint32(p.Threads) || p.Memory > maxMemory,
		p.Time < 1 || p.Time > maxTime,
		p.Threads < 1 || p.Threads > maxThreads,
		p.KeyLen < minKeyLen || p.KeyLen > maxKeyLen,
		p.SaltLen < minSaltLen || p.SaltLen > maxSaltLen:
		return fmt.Errorf("auth: argon2id parameters out of range: %+v", p)
	}
	return nil
}

// String returns the PHC parameter segment, e.g. "m=19456,t=2,p=1".
func (p Params) String() string {
	return fmt.Sprintf("m=%d,t=%d,p=%d", p.Memory, p.Time, p.Threads)
}

// Params returns the parameters used for new hashes.
func (h *Hasher) Params() Params { return h.params }

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("auth: waiting for password hashing: %w", ctx.Err())
	}
}

func (h *Hasher) release() { <-h.sem }

func (h *Hasher) key(ctx context.Context, password, salt []byte, p Params) ([]byte, error) {
	if err := h.acquire(ctx); err != nil {
		return nil, err
	}
	defer h.release()
	return argon2.IDKey(password, salt, p.Time, p.Memory, p.Threads, p.KeyLen), nil
}

// Hash returns the PHC string of password. It does not check the password
// policy; see [ValidatePassword].
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := io.ReadFull(h.rand, salt); err != nil {
		return "", fmt.Errorf("auth: random salt: %w", err)
	}
	key, err := h.key(ctx, []byte(password), salt, h.params)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$%s$%s$%s", argon2.Version, h.params, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether password matches the PHC string encoded. The key
// comparison runs in constant time. rehash is true when the password
// matched but encoded uses other parameters than the hasher, so the caller
// should store a fresh hash. A malformed encoded yields [ErrMalformedHash].
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (ok, rehash bool, err error) {
	p, salt, want, err := parsePHC(encoded)
	if err != nil {
		return false, false, err
	}
	got, err := h.key(ctx, []byte(password), salt, p)
	if err != nil {
		return false, false, err
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, p != h.params, nil
}

// VerifyDummy spends the same effort as verifying a real password and
// always reports false. Login uses it for unknown usernames so that the
// response time does not reveal whether an account exists.
func (h *Hasher) VerifyDummy(ctx context.Context, password string) error {
	_, _, err := h.Verify(ctx, password, h.dummy)
	return err
}

// parsePHC decodes $argon2id$v=19$m=M,t=T,p=P$salt$hash.
func parsePHC(s string) (Params, []byte, []byte, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return Params{}, nil, nil, ErrMalformedHash
	}
	var p Params
	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return Params{}, nil, nil, ErrMalformedHash
	}
	for i, want := range []string{"m", "t", "p"} {
		k, v, ok := strings.Cut(kv[i], "=")
		if !ok || k != want {
			return Params{}, nil, nil, ErrMalformedHash
		}
		bits := 32
		if k == "p" {
			bits = 8
		}
		n, err := strconv.ParseUint(v, 10, bits)
		if err != nil {
			return Params{}, nil, nil, ErrMalformedHash
		}
		switch k {
		case "m":
			p.Memory = uint32(n) //nolint:gosec // ParseUint bounded n to 32 bits
		case "t":
			p.Time = uint32(n) //nolint:gosec // ParseUint bounded n to 32 bits
		default:
			p.Threads = uint8(n) //nolint:gosec // ParseUint bounded n to 8 bits
		}
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key)) //nolint:gosec // bounded by validate below
	if p.validate() != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	return p, salt, key, nil
}

// ValidatePassword checks the password policy: valid UTF-8, at least
// [MinPasswordLen] characters and at most [MaxPasswordLen] bytes. field is
// the JSON pointer reported in the error.
func ValidatePassword(field, password string) error {
	var v validation
	switch {
	case !utf8.ValidString(password):
		v.add(field, CodeInvalidFormat, "password must be valid UTF-8")
	case utf8.RuneCountInString(password) < MinPasswordLen:
		v.add(field, CodeOutOfRange, "password must be at least %d characters", MinPasswordLen)
	case len(password) > MaxPasswordLen:
		v.add(field, CodeOutOfRange, "password must be at most %d bytes", MaxPasswordLen)
	}
	return v.err()
}
