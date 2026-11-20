// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

// GSM 03.38 / 3GPP TS 23.038 §6.2.1 default alphabet. Index = septet value.
// Position 0x1B is the escape to the extension table and is never a character.
const gsmBasic = "@£$¥èéùìòÇ\nØø\rÅå" +
	"Δ_ΦΓΛΩΠΨΣΘΞ\x1bÆæßÉ" +
	" !\"#¤%&'()*+,-./0123456789:;<=>?" +
	"¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§" +
	"¿abcdefghijklmnopqrstuvwxyzäöñüà"

const gsmEscape = 0x1B

// gsmExt is the extension table (TS 23.038 §6.2.1.1): character -> septet
// following the escape. Each of these costs two septets.
var gsmExt = map[rune]byte{
	'\f': 0x0A, '^': 0x14, '{': 0x28, '}': 0x29, '\\': 0x2F,
	'[': 0x3C, '~': 0x3D, ']': 0x3E, '|': 0x40, '€': 0x65,
}

var (
	gsmBasicRunes = []rune(gsmBasic)
	gsmBasicIndex = func() map[rune]byte {
		m := make(map[rune]byte, 128)
		for i, r := range gsmBasicRunes {
			if i != gsmEscape {
				m[r] = byte(i)
			}
		}
		return m
	}()
	gsmExtRev = func() map[byte]rune {
		m := make(map[byte]rune, len(gsmExt))
		for r, b := range gsmExt {
			m[b] = r
		}
		return m
	}()
)

// gsm7Encode converts s to septets (extension characters become two septets).
// ok is false if s contains a character outside GSM-7.
func gsm7Encode(s string) (septets []byte, ok bool) {
	for _, r := range s {
		if b, found := gsmBasicIndex[r]; found {
			septets = append(septets, b)
		} else if b, found := gsmExt[r]; found {
			septets = append(septets, gsmEscape, b)
		} else {
			return nil, false
		}
	}
	return septets, true
}

// gsm7Decode converts septets back to text. Unknown extension codes decode as
// the basic-table character, as TS 23.038 prescribes.
func gsm7Decode(septets []byte) string {
	out := make([]rune, 0, len(septets))
	for i := 0; i < len(septets); i++ {
		b := septets[i] & 0x7F
		if b == gsmEscape && i+1 < len(septets) {
			i++
			if r, found := gsmExtRev[septets[i]&0x7F]; found {
				out = append(out, r)
				continue
			}
			b = septets[i] & 0x7F
		}
		out = append(out, gsmBasicRunes[b])
	}
	return string(out)
}

// GSM7Length returns the number of septets s occupies (extension characters
// count twice). ok is false if s is not representable in GSM-7.
func GSM7Length(s string) (n int, ok bool) {
	sep, ok := gsm7Encode(s)
	return len(sep), ok
}

// IsGSM7 reports whether every character of s is in the GSM-7 default
// alphabet or its extension table.
func IsGSM7(s string) bool {
	_, ok := gsm7Encode(s)
	return ok
}
