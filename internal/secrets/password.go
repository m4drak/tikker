// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package secrets

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

const redacted = "[REDACTED]"

// Password is a plaintext credential. Every formatting path (fmt verbs
// including %#v, slog, JSON, text) yields a redaction instead of the value.
// Use [Password.Reveal] at the single point where the value is needed.
type Password string

// Reveal returns the plaintext.
func (p Password) Reveal() string { return string(p) }

// String returns the redacted form; the empty value stays empty.
func (p Password) String() string {
	if p == "" {
		return ""
	}
	return redacted
}

// GoString implements fmt.GoStringer.
func (p Password) GoString() string { return `secrets.Password("` + p.String() + `")` }

// Format implements fmt.Formatter so every verb redacts.
func (p Password) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, p.String()) }

// LogValue implements slog.LogValuer.
func (p Password) LogValue() slog.Value { return slog.StringValue(p.String()) }

// MarshalJSON implements json.Marshaler: the value never leaves the process.
func (p Password) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }

// MarshalText implements encoding.TextMarshaler (YAML, form encoders).
func (p Password) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// UnmarshalJSON accepts the plaintext from an API request body (password is
// writeOnly in the contract).
func (p *Password) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("secrets: password must be a string: %w", err)
	}
	*p = Password(s)
	return nil
}
