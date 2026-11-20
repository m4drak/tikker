// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/m4drak/tikker/internal/domain"
)

const (
	idempotencyHeader = "Idempotency-Key"
	maxIdempotencyKey = 255
)

// idempotencyKey reads and validates the Idempotency-Key header ("" when
// absent): 1..255 printable ASCII characters (parameter IdempotencyKey).
func idempotencyKey(r *http.Request) (string, error) {
	k := r.Header.Get(idempotencyHeader)
	if k == "" {
		return "", nil
	}
	bad := func(detail string) error {
		return malformed("Invalid Idempotency-Key header.",
			fieldError{Field: idempotencyHeader, Code: codeInvalidFormat, Detail: detail})
	}
	if len(k) > maxIdempotencyKey {
		return "", bad("Must be at most 255 characters.")
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return "", bad("Must consist of printable ASCII characters without spaces.")
		}
	}
	return k, nil
}

// requestHash is the digest the store compares for one Idempotency-Key: the
// SHA-256 of the operation name and the canonical form of the JSON body
// (members sorted, insignificant whitespace removed), so member order and
// formatting do not matter. The operation is part of it, so one key cannot
// be reused between POST /messages and POST /messages/batch.
func requestHash(op string, body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v) // maps are marshalled with sorted keys
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(op))
	h.Write([]byte{0})
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// inflight tracks Idempotency-Keys whose first request is still being
// processed. The store makes the check-and-insert atomic, so a concurrent
// duplicate could never create messages twice; this only gives the client
// the 409 "idempotency-key-in-use" of the contract instead of a silent wait.
type inflight struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

func newInflight() *inflight { return &inflight{keys: make(map[string]struct{})} }

// acquire reserves (actor, key); false when it is already reserved.
func (f *inflight) acquire(actor domain.Actor, key string) bool {
	k := actor.String() + "\x00" + key
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, busy := f.keys[k]; busy {
		return false
	}
	f.keys[k] = struct{}{}
	return true
}

func (f *inflight) release(actor domain.Actor, key string) {
	f.mu.Lock()
	delete(f.keys, actor.String()+"\x00"+key)
	f.mu.Unlock()
}
