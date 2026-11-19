// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"crypto/rand"
	"io"
	"sync"

	"github.com/oklog/ulid/v2"

	"github.com/m4drak/tikker/internal/clock"
)

// ID identifies every persisted entity. It is a ULID in its canonical
// 26-character Crockford base32 form: lexicographic order equals creation
// order, which the store uses for cursor pagination.
//
// The empty ID means "no reference" (for example a message not yet assigned
// to a channel).
type ID string

// String returns the ID as a string.
func (id ID) String() string { return string(id) }

// IsZero reports whether the ID is empty.
func (id ID) IsZero() bool { return id == "" }

// Valid reports whether the ID is a well-formed ULID.
func (id ID) Valid() bool {
	_, err := ulid.ParseStrict(string(id))
	return err == nil
}

// ParseID validates s and returns it as an ID. It accepts only canonical
// (upper-case) ULIDs so that equal IDs are byte-for-byte equal in the store.
func ParseID(s string) (ID, error) {
	u, err := ulid.ParseStrict(s)
	if err != nil {
		return "", &ParseError{Type: "id", Value: s}
	}
	if u.String() != s {
		return "", &ParseError{Type: "id", Value: s}
	}
	return ID(s), nil
}

// IDGenerator creates new identifiers.
type IDGenerator interface {
	NewID() ID
}

// ULIDGenerator is the production [IDGenerator]. IDs generated within the same
// millisecond are monotonically increasing. It is safe for concurrent use.
type ULIDGenerator struct {
	clk     clock.Clock
	mu      sync.Mutex
	entropy io.Reader
}

// NewULIDGenerator returns a generator that timestamps IDs with clk and draws
// randomness from crypto/rand.
func NewULIDGenerator(clk clock.Clock) *ULIDGenerator {
	return &ULIDGenerator{clk: clk, entropy: ulid.Monotonic(rand.Reader, 0)}
}

// NewID returns a fresh ULID.
func (g *ULIDGenerator) NewID() ID {
	g.mu.Lock()
	defer g.mu.Unlock()
	// MustNew panics only if the entropy source fails or the monotonic
	// counter overflows within one millisecond (2^80 IDs); both are fatal.
	return ID(ulid.MustNew(ulid.Timestamp(g.clk.Now()), g.entropy).String())
}
