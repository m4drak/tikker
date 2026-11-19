// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package gsm

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Substitute is the character that replaces what cannot be transliterated.
const Substitute = '?'

// special maps characters that Unicode decomposition does not reduce (or
// reduces badly) to GSM-7 text. Characters that already are in GSM-7 are
// never looked up here.
var special = map[rune]string{
	// Letters without a decomposition.
	'ł': "l", 'Ł': "L", 'đ': "d", 'Đ': "D", 'ð': "d", 'Ð': "D", 'þ': "th", 'Þ': "Th",
	'œ': "oe", 'Œ': "OE", 'ı': "i", 'ĸ': "k", 'ŋ': "n", 'Ŋ': "N", 'ħ': "h", 'Ħ': "H",
	'ŧ': "t", 'Ŧ': "T", 'ſ': "s",
	// Typographic quotes and primes.
	'‘': "'", '’': "'", '‚': ",", '‛': "'", '′': "'", 'ʼ': "'",
	'“': "\"", '”': "\"", '„': "\"", '‟': "\"", '″': "\"",
	'«': "\"", '»': "\"", '‹': "<", '›': ">", '`': "'", '´': "'",
	// Dashes, ellipsis, bullets.
	'‐': "-", '‑': "-", '‒': "-", '–': "-", '—': "-", '―': "-",
	'−': "-", '\u00AD': "",
	'…': "...", '•': "*", '·': ".", '‣': ">", '●': "*",
	// Spaces and invisible characters.
	'\u00A0': " ", '\u1680': " ", '\u2000': " ", '\u2001': " ", '\u2002': " ", '\u2003': " ",
	'\u2004': " ", '\u2005': " ", '\u2006': " ", '\u2007': " ", '\u2008': " ", '\u2009': " ",
	'\u200A': " ", '\u202F': " ", '\u205F': " ", '\u3000': " ", '\t': " ",
	'\u200B': "", '\u200C': "", '\u200D': "", '\u2060': "", '\uFEFF': "", '\u200E': "", '\u200F': "",
	'\u2028': "\n", '\u2029': "\n", '\v': "\n",
	// Symbols.
	'©': "(c)", '®': "(R)", '™': "(TM)", '×': "x", '÷': "/",
	'°': "o", '±': "+/-", '≤': "<=", '≥': ">=",
	'≠': "!=", '→': "->", '←': "<-", '¼': "1/4", '½': "1/2", '¾': "3/4",
	'¢': "c", '²': "2", '³': "3", '¹': "1",
	'ª': "a", 'º': "o", 'µ': "u",
	'¦': "|", '∕': "/", '⁄': "/", '／': "/",
}

// Replacement reports characters that [Transliterate] turned into
// [Substitute].
type Replacement struct {
	Char  rune // the original character
	Count int  // how many times it occurred
}

// Result is the outcome of [Transliterate].
type Result struct {
	Text string
	// Changed is true when Text differs from the input.
	Changed bool
	// Substituted lists, in order of first appearance, the characters that
	// could not be mapped and were replaced by [Substitute].
	Substituted []Replacement
}

// Transliterate rewrites s into text that is encodable in GSM-7. Characters
// that already are GSM-7 stay as they are (so "é" and "ü" survive). Other
// characters are mapped by, in order, the table of typographic characters
// and special letters, then Unicode canonical decomposition with the
// combining marks removed (ą->a, ó->o, ś->s, ż->z, é stays é). Whatever is
// left (emoji, CJK, ...) becomes '?' and is reported in Result.Substituted.
//
// Invalid UTF-8 bytes count as one unmappable character U+FFFD each.
func Transliterate(s string) Result {
	if IsGSM7(s) {
		return Result{Text: s}
	}
	var sb strings.Builder
	sb.Grow(len(s))
	var subs []Replacement
	idx := map[rune]int{}
	for _, r := range s {
		if InGSM(r) {
			sb.WriteRune(r)
			continue
		}
		if m, ok := mapRune(r); ok {
			sb.WriteString(m)
			continue
		}
		sb.WriteRune(Substitute)
		if i, ok := idx[r]; ok {
			subs[i].Count++
		} else {
			idx[r] = len(subs)
			subs = append(subs, Replacement{Char: r, Count: 1})
		}
	}
	out := sb.String()
	return Result{Text: out, Changed: out != s, Substituted: subs}
}

// mapRune maps one non-GSM character to GSM-7 text.
func mapRune(r rune) (string, bool) {
	if m, ok := special[r]; ok && IsGSM7(m) {
		return m, true
	}
	if r == unicode.ReplacementChar {
		return "", false
	}
	// Decompose (NFKD also folds compatibility forms such as ﬁ and ①), drop
	// combining marks, and accept the result if all of it is GSM-7.
	var sb strings.Builder
	for _, d := range norm.NFKD.String(string(r)) {
		if unicode.Is(unicode.Mn, d) {
			continue
		}
		sb.WriteRune(d)
	}
	out := sb.String()
	if out == "" || out == string(r) || !IsGSM7(out) {
		// An empty result means r was only a combining mark: drop it.
		if out == "" && unicode.Is(unicode.Mn, r) {
			return "", true
		}
		return "", false
	}
	return out, true
}
