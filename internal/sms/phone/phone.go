// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package phone normalises recipients and senders to E.164 using
// github.com/nyaruka/phonenumbers (a port of libphonenumber), and, when
// allowed, accepts short codes and alphanumeric sender IDs (SPEC §4.6).
package phone

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nyaruka/phonenumbers"
)

// Errors; test with errors.Is.
var (
	// ErrInvalidNumber: not a valid phone number (API code
	// invalid_phone_number).
	ErrInvalidNumber = errors.New("phone: invalid phone number")
	// ErrInvalidRegion: Options.DefaultRegion is not a known region code.
	// A configuration error, not the caller's input.
	ErrInvalidRegion = errors.New("phone: unknown default region")
)

// Kind classifies a normalised number.
type Kind string

// Kind values.
const (
	// KindE164 is a full international number ("+48600100200").
	KindE164 Kind = "e164"
	// KindShort is a short code such as "8012" or "112": digits only.
	KindShort Kind = "short"
	// KindAlphanumeric is an alphanumeric sender ID such as "MyBank".
	KindAlphanumeric Kind = "alphanumeric"
)

// Short codes are 3 to 6 digits; longer digit strings must be valid numbers,
// so a mistyped national number is not accepted as a short code.
const (
	minShort = 3
	maxShort = 6
	// maxAlnum is the GSM limit for an alphanumeric originating address
	// (11 septets).
	maxAlnum = 11
)

// Options for [Normalize].
type Options struct {
	// DefaultRegion is the ISO 3166-1 alpha-2 region used for numbers
	// without a country prefix, e.g. "PL". Empty means numbers must be
	// international.
	DefaultRegion string
	// AllowShort accepts short codes and alphanumeric sender IDs
	// (SPEC allow_short_numbers).
	AllowShort bool
}

// Number is a normalised number.
type Number struct {
	// Value is "+<digits>" for KindE164, digits for KindShort, the ID for
	// KindAlphanumeric.
	Value string
	Kind  Kind
}

// String returns Value.
func (n Number) String() string { return n.Value }

// Normalize validates raw and converts it to E.164 (or a short code or
// alphanumeric ID if opts.AllowShort). Separators (space, - . ( ) /) are
// ignored. A leading "00" is read as "+".
func Normalize(raw string, opts Options) (Number, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Number{}, fmt.Errorf("%w: empty", ErrInvalidNumber)
	}
	region := strings.ToUpper(strings.TrimSpace(opts.DefaultRegion))
	if region == "" {
		region = "ZZ" // libphonenumber's "unknown region": international only
	} else if _, ok := phonenumbers.GetSupportedRegions()[region]; !ok {
		return Number{}, fmt.Errorf("%w: %q", ErrInvalidRegion, opts.DefaultRegion)
	}

	if hasLetter(raw) {
		// libphonenumber would read "1-800-FLOWERS" as digits; here letters
		// mean an alphanumeric sender ID or an error.
		if opts.AllowShort && validAlnum(raw) {
			return Number{Value: raw, Kind: KindAlphanumeric}, nil
		}
		return Number{}, fmt.Errorf("%w: contains letters", ErrInvalidNumber)
	}
	if !onlyPhoneChars(raw) {
		return Number{}, fmt.Errorf("%w: unexpected characters", ErrInvalidNumber)
	}

	if num, err := phonenumbers.Parse(raw, region); err == nil && phonenumbers.IsValidNumber(num) {
		return Number{Value: phonenumbers.Format(num, phonenumbers.E164), Kind: KindE164}, nil
	}
	if opts.AllowShort {
		if d := digitsOnly(raw); d == strings.TrimSpace(raw) && len(d) >= minShort && len(d) <= maxShort {
			return Number{Value: d, Kind: KindShort}, nil
		}
	}
	return Number{}, fmt.Errorf("%w: %q", ErrInvalidNumber, Mask(raw))
}

// Mask hides the middle of a number for logs and error messages:
// "+48600100200" becomes "+48*****200". Short values are masked entirely
// except for the first character.
func Mask(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		if len(r) == 0 {
			return ""
		}
		return string(r[:1]) + strings.Repeat("*", len(r)-1)
	}
	keepHead := 3
	if len(r) < 8 {
		keepHead = 1
	}
	keepTail := 3
	if len(r) < 8 {
		keepTail = 2
	}
	return string(r[:keepHead]) + strings.Repeat("*", len(r)-keepHead-keepTail) + string(r[len(r)-keepTail:])
}

func hasLetter(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127 {
			return true
		}
	}
	return false
}

// validAlnum reports whether s is an acceptable alphanumeric sender ID:
// 1..11 characters from letters, digits, space and a few punctuation marks.
func validAlnum(s string) bool {
	if len(s) == 0 || len(s) > maxAlnum {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ', r == '.', r == '-', r == '_', r == '&':
		default:
			return false
		}
	}
	return true
}

func onlyPhoneChars(s string) bool {
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9', r == ' ', r == '-', r == '.', r == '(', r == ')', r == '/':
		case r == '+' && i == 0:
		default:
			return false
		}
	}
	return true
}

func digitsOnly(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
