// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package logging

import (
	"log/slog"
	"strings"
	"unicode"
)

// Masking parameters. A digit-only number with at least minFullDigits digits
// keeps its two leading digits (the country code of most numbers) and its
// last four digits. A shorter number with at least minShortDigits digits
// keeps only its last two digits. Shorter numbers (service codes) are masked
// completely.
const (
	maskedPrefixDigits = 2
	maskedSuffixDigits = 4
	shortSuffixDigits  = 2
	minFullDigits      = 10
	minShortDigits     = 5
	maskRune           = '*'
)

// MaskPhone hides the middle of a phone number or sender name for logs:
//
//	MaskPhone("+48601234567") == "+48*****4567"
//
// Digit-only input is masked by digit count; spaces, dashes, dots and
// parentheses are ignored and dropped from the result. Input that contains
// letters, such as an alphanumeric sender, keeps only its first and last
// character.
func MaskPhone(s string) string {
	body := strings.TrimSpace(s)
	if body == "" {
		return ""
	}

	plus := ""
	if strings.HasPrefix(body, "+") {
		plus = "+"
		body = body[1:]
	}

	if isNumeric(body) {
		if digits := onlyDigits(body); digits != "" {
			return plus + maskDigits(digits)
		}
	}
	return plus + maskText(body)
}

// PhoneAttr returns a slog attribute holding the masked number.
func PhoneAttr(key, number string) slog.Attr {
	return slog.String(key, MaskPhone(number))
}

// Phone is a phone number or sender name that masks itself when logged.
// Use it as a slog value, for example slog.Any("peer", logging.Phone(p)).
type Phone string

// LogValue implements slog.LogValuer.
func (p Phone) LogValue() slog.Value {
	return slog.StringValue(MaskPhone(string(p)))
}

// String returns the masked form, so that fmt output is safe too.
func (p Phone) String() string {
	return MaskPhone(string(p))
}

func maskDigits(digits string) string {
	n := len(digits)
	prefix, suffix := 0, shortSuffixDigits
	switch {
	case n < minShortDigits:
		return strings.Repeat(string(maskRune), n)
	case n >= minFullDigits:
		prefix, suffix = maskedPrefixDigits, maskedSuffixDigits
	}
	return digits[:prefix] + strings.Repeat(string(maskRune), n-prefix-suffix) + digits[n-suffix:]
}

func maskText(s string) string {
	r := []rune(s)
	if len(r) <= 2 {
		return strings.Repeat(string(maskRune), len(r))
	}
	return string(r[0]) + strings.Repeat(string(maskRune), len(r)-2) + string(r[len(r)-1])
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// isNumeric reports whether s consists only of ASCII digits and the usual
// phone-number separators.
func isNumeric(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c == ' ', c == '-', c == '(', c == ')', c == '.':
		case unicode.IsSpace(c):
		default:
			return false
		}
	}
	return true
}
