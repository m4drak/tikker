// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package gsm implements the GSM 03.38 (3GPP TS 23.038) 7-bit default
// alphabet with its extension table, septet packing, and the segment
// arithmetic for GSM-7 and UCS-2 messages.
//
// Units are the capacity currency of a segment: one septet in GSM-7 (an
// extension character costs two: ESC plus the code), one UTF-16 code unit in
// UCS-2 (a character outside the BMP, e.g. an emoji, costs two). A character
// is never split between segments.
package gsm

import "unicode/utf8"

const escape = 0x1B

// basic is the GSM 7-bit default alphabet; index = septet value. Index 0x1B
// is the escape to the extension table and is not a character.
var basic = [128]rune{
	'@', '£', '$', '¥', 'è', 'é', 'ù', 'ì', 'ò', 'Ç', '\n', 'Ø', 'ø', '\r', 'Å', 'å',
	'Δ', '_', 'Φ', 'Γ', 'Λ', 'Ω', 'Π', 'Ψ', 'Σ', 'Θ', 'Ξ', utf8.RuneError, 'Æ', 'æ', 'ß', 'É',
	' ', '!', '"', '#', '¤', '%', '&', '\'', '(', ')', '*', '+', ',', '-', '.', '/',
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', ':', ';', '<', '=', '>', '?',
	'¡', 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O',
	'P', 'Q', 'R', 'S', 'T', 'U', 'V', 'W', 'X', 'Y', 'Z', 'Ä', 'Ö', 'Ñ', 'Ü', '§',
	'¿', 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o',
	'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z', 'ä', 'ö', 'ñ', 'ü', 'à',
}

// extension is the GSM 7-bit default extension table: septet after ESC to
// character.
var extension = map[byte]rune{
	0x0A: '\f', 0x14: '^', 0x28: '{', 0x29: '}', 0x2F: '\\',
	0x3C: '[', 0x3D: '~', 0x3E: ']', 0x40: '|', 0x65: '€',
}

var (
	toBasic = func() map[rune]byte {
		m := make(map[rune]byte, 128)
		for i, r := range basic {
			if i != escape {
				m[r] = byte(i)
			}
		}
		return m
	}()
	toExt = func() map[rune]byte {
		m := make(map[rune]byte, len(extension))
		for k, r := range extension {
			m[r] = k
		}
		return m
	}()
)

// InGSM reports whether r is in the default alphabet or its extension table.
func InGSM(r rune) bool {
	if _, ok := toBasic[r]; ok {
		return true
	}
	_, ok := toExt[r]
	return ok
}

// IsExtension reports whether r needs an escape (costs two septets).
func IsExtension(r rune) bool {
	_, ok := toExt[r]
	return ok
}

// IsGSM7 reports whether every character of s is encodable in GSM-7.
// Invalid UTF-8 (which ranges as U+FFFD) is not.
func IsGSM7(s string) bool {
	for _, r := range s {
		if !InGSM(r) {
			return false
		}
	}
	return true
}

// Encode converts s to septets (one per byte, values 0..127), using the
// escape sequence for extension characters. ok is false when s has a
// character outside GSM-7; septets is then nil.
func Encode(s string) (septets []byte, ok bool) {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if b, found := toBasic[r]; found {
			out = append(out, b)
		} else if b, found := toExt[r]; found {
			out = append(out, escape, b)
		} else {
			return nil, false
		}
	}
	return out, true
}

// Decode converts septets to text. An escape followed by a code that is not
// in the extension table is shown as the basic-table character, as TS 23.038
// requires; a trailing escape is dropped. Septet values above 127 use the
// low seven bits.
func Decode(septets []byte) string {
	out := make([]rune, 0, len(septets))
	for i := 0; i < len(septets); i++ {
		b := septets[i] & 0x7F
		if b != escape {
			out = append(out, basic[b])
			continue
		}
		if i+1 >= len(septets) {
			break
		}
		i++
		n := septets[i] & 0x7F
		switch r, ok := extension[n]; {
		case ok:
			out = append(out, r)
		case n == escape:
			out = append(out, ' ') // ESC ESC is reserved; shown as a space
		default:
			out = append(out, basic[n])
		}
	}
	return string(out)
}

// Pack packs septets into octets (TS 23.038 §6.1.2.1.1) after fillBits zero
// bits (0..6), which align the text to a septet boundary behind a UDH.
func Pack(septets []byte, fillBits int) []byte {
	out := make([]byte, (fillBits+7*len(septets)+7)/8)
	bit := fillBits
	for _, s := range septets {
		v := uint16(s&0x7F) << (bit % 8)
		out[bit/8] |= byte(v)
		if bit%8 > 1 {
			out[bit/8+1] |= byte(v >> 8)
		}
		bit += 7
	}
	return out
}

// Unpack extracts count septets from data after skipping skipBits bits.
// It returns fewer septets when data is too short.
func Unpack(data []byte, count, skipBits int) []byte {
	out := make([]byte, 0, count)
	bit := skipBits
	for len(out) < count {
		i := bit / 8
		if bit+7 > 8*len(data) {
			break
		}
		v := uint16(data[i])
		if i+1 < len(data) {
			v |= uint16(data[i+1]) << 8
		}
		out = append(out, byte(v>>(bit%8))&0x7F)
		bit += 7
	}
	return out
}
