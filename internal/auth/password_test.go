// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newTestHasher(t *testing.T, p Params, conc int) *Hasher {
	t.Helper()
	h, err := NewHasher(p, conc, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDefaultParams(t *testing.T) {
	// OWASP Password Storage Cheat Sheet, argon2id option 1.
	if DefaultParams.Memory != 19456 || DefaultParams.Time != 2 || DefaultParams.Threads != 1 ||
		DefaultParams.SaltLen != 16 || DefaultParams.KeyLen != 32 {
		t.Fatalf("DefaultParams = %+v", DefaultParams)
	}
	h := newTestHasher(t, Params{}, 0) // zero value selects the defaults
	if h.Params() != DefaultParams || cap(h.sem) != DefaultHashConcurrency {
		t.Fatalf("params %+v, concurrency %d", h.Params(), cap(h.sem))
	}
	enc, err := h.Hash(context.Background(), goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)
	if !re.MatchString(enc) {
		t.Fatalf("PHC string %q", enc)
	}
	if ok, rehash, err := h.Verify(context.Background(), goodPassword, enc); !ok || rehash || err != nil {
		t.Fatalf("Verify = %v %v %v", ok, rehash, err)
	}
}

func TestHashVerify(t *testing.T) {
	ctx := context.Background()
	h := newTestHasher(t, testParams, 1)
	enc, err := h.Hash(ctx, goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	enc2, _ := h.Hash(ctx, goodPassword)
	if enc == enc2 {
		t.Fatal("same salt twice")
	}
	stronger := newTestHasher(t, Params{Memory: 128, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}, 1)

	tests := []struct {
		name       string
		h          *Hasher
		password   string
		ok, rehash bool
	}{
		{"match", h, goodPassword, true, false},
		{"wrong", h, goodPassword + "!", false, false},
		{"empty", h, "", false, false},
		{"case", h, strings.ToUpper(goodPassword), false, false},
		{"match with new params", stronger, goodPassword, true, true},
		{"wrong with new params", stronger, "nope", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, rehash, err := tc.h.Verify(ctx, tc.password, enc)
			if err != nil || ok != tc.ok || rehash != tc.rehash {
				t.Fatalf("Verify = %v %v %v, want %v %v", ok, rehash, err, tc.ok, tc.rehash)
			}
		})
	}
	if err := h.VerifyDummy(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	h := newTestHasher(t, testParams, 1)
	good, _ := h.Hash(context.Background(), "x")
	parts := strings.Split(good, "$")
	with := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "$")
	}
	tests := []struct{ name, enc string }{
		{"empty", ""},
		{"bcrypt", "$2a$10$abcdefghijklmnopqrstuuJ1Zr5Q6m3hC7wK1x6v0w8Yp2s4l9Q3a"},
		{"argon2i", with(1, "argon2i")},
		{"version", with(2, "v=16")},
		{"params count", with(3, "m=64,t=1")},
		{"params order", with(3, "t=1,m=64,p=1")},
		{"param not number", with(3, "m=x,t=1,p=1")},
		{"param no equals", with(3, "m64,t=1,p=1")},
		{"threads overflow", with(3, "m=64,t=1,p=300")},
		{"memory too large", with(3, "m=99999999,t=1,p=1")},
		{"time zero", with(3, "m=64,t=0,p=1")},
		{"memory below 8p", with(3, "m=8,t=1,p=4")},
		{"bad salt", with(4, "!!!")},
		{"short salt", with(4, "AAAA")},
		{"bad key", with(5, "%%%")},
		{"short key", with(5, "AAAA")},
		{"extra part", good + "$x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, _, err := h.Verify(context.Background(), "x", tc.enc)
			if ok || !errors.Is(err, ErrMalformedHash) {
				t.Fatalf("Verify(%q) = %v, %v", tc.enc, ok, err)
			}
		})
	}
}

func TestNewHasherErrors(t *testing.T) {
	bad := []Params{
		{Memory: 64, Time: 0, Threads: 1, SaltLen: 16, KeyLen: 32},
		{Memory: 64, Time: 1, Threads: 0, SaltLen: 16, KeyLen: 32},
		{Memory: 4, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
		{Memory: 64, Time: 1, Threads: 1, SaltLen: 4, KeyLen: 32},
		{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 8},
		{Memory: 1 << 30, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
	}
	for _, p := range bad {
		if _, err := NewHasher(p, 1, nil); err == nil {
			t.Errorf("NewHasher(%+v) accepted", p)
		}
	}
	if _, err := NewHasher(testParams, 1, failReader{}); err == nil {
		t.Error("NewHasher with failing entropy accepted")
	}
	h := newTestHasher(t, testParams, 1)
	h.rand = failReader{}
	if _, err := h.Hash(context.Background(), "x"); err == nil {
		t.Error("Hash with failing entropy succeeded")
	}
}

func TestHashConcurrencyLimit(t *testing.T) {
	h := newTestHasher(t, testParams, 1)
	h.sem <- struct{}{} // occupy the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "x"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Hash while saturated = %v", err)
	}
	enc := "$argon2id$v=19$m=64,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := h.Verify(ctx, "x", enc); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Verify while saturated = %v", err)
	}
	<-h.sem
	if _, err := h.Hash(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name, pw string
		code     string // "" = valid
	}{
		{"ok", "twelve chars", ""},
		{"ok unicode", "zażółć gęślą", ""},
		{"short", "eleven char", CodeOutOfRange},
		{"short multibyte", "ąąąąąąąąąąą", CodeOutOfRange}, // 11 runes, 22 bytes
		{"empty", "", CodeOutOfRange},
		{"too long", strings.Repeat("a", MaxPasswordLen+1), CodeOutOfRange},
		{"max", strings.Repeat("a", MaxPasswordLen), ""},
		{"invalid utf8", "abcdefghijkl\xff", CodeInvalidFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword("/password", tc.pw)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || !errors.Is(err, ErrValidation) || len(ve.Errors) != 1 ||
				ve.Errors[0].Code != tc.code || ve.Errors[0].Field != "/password" {
				t.Fatalf("err = %#v", err)
			}
			if strings.Contains(err.Error(), tc.pw) && tc.pw != "" {
				t.Fatal("error message contains the password")
			}
		})
	}
}
