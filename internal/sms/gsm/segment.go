// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package gsm

import (
	"github.com/m4drak/tikker/internal/domain"
)

// Segment capacities in units.
const (
	GSMSingle = 160 // one GSM-7 SMS without UDH
	GSMMulti  = 153 // per part of a concatenated GSM-7 SMS (UDH uses 7 septets)
	UCSSingle = 70  // one UCS-2 SMS without UDH
	UCSMulti  = 67  // per part of a concatenated UCS-2 SMS
)

// Limits returns the unit capacity of a lone SMS and of one part of a
// concatenated SMS for enc.
func Limits(enc domain.Encoding) (single, multi int) {
	if enc == domain.EncodingUCS2 {
		return UCSSingle, UCSMulti
	}
	return GSMSingle, GSMMulti
}

// Units returns the capacity cost of r in enc. For GSM-7 the result is only
// meaningful for characters that [InGSM] accepts.
func Units(enc domain.Encoding, r rune) int {
	if enc == domain.EncodingUCS2 {
		if r > 0xFFFF {
			return 2
		}
		return 1
	}
	if IsExtension(r) {
		return 2
	}
	return 1
}

// Counter counts segments incrementally. Create it with [NewCounter]. It is
// a plain value: copy it to try an addition.
type Counter struct {
	single, multi int
	enc           domain.Encoding
	total         int // all units so far
	segs          int // segments under the multi-part packing
	cur           int // units in the last multi-part segment
}

// NewCounter returns an empty Counter for enc.
func NewCounter(enc domain.Encoding) Counter {
	s, m := Limits(enc)
	return Counter{single: s, multi: m, enc: enc}
}

// Add appends r.
func (c *Counter) Add(r rune) {
	u := Units(c.enc, r)
	c.total += u
	if c.segs == 0 || c.cur+u > c.multi {
		c.segs++
		c.cur = 0
	}
	c.cur += u
}

// AddString appends every character of s.
func (c *Counter) AddString(s string) {
	for _, r := range s {
		c.Add(r)
	}
}

// Units is the total number of units added.
func (c Counter) Units() int { return c.total }

// Segments is the number of SMS needed: 0 for no text, 1 when everything
// fits a lone SMS, otherwise the number of concatenated parts.
func (c Counter) Segments() int {
	switch {
	case c.total == 0:
		return 0
	case c.total <= c.single:
		return 1
	default:
		return c.segs
	}
}

// Remaining is the number of units still free in the current (last) segment.
func (c Counter) Remaining() int {
	if c.total <= c.single {
		return c.single - c.total
	}
	return c.multi - c.cur
}

// Info is the result of [Analyze].
type Info struct {
	Encoding domain.Encoding
	Chars    int // Unicode code points
	Units    int // septets (GSM-7, extensions count twice) or UTF-16 units
	Segments int // 0 for empty text
	// PerSegment is the capacity of one segment at the current length:
	// the lone-SMS limit while the text fits one SMS, else the
	// concatenated-part limit.
	PerSegment int
	// Remaining is the number of units left in the current segment.
	Remaining int
	// NonGSM lists, without repetition and in order of appearance, the
	// characters that force UCS-2. Empty for GSM-7 text.
	NonGSM []rune
}

// Analyze computes the encoding and segment count of s. Invalid UTF-8 counts
// as U+FFFD.
func Analyze(s string) Info {
	enc := domain.EncodingGSM7
	var nonGSM []rune
	seen := map[rune]bool{}
	chars := 0
	for _, r := range s {
		chars++
		if !InGSM(r) {
			enc = domain.EncodingUCS2
			if !seen[r] {
				seen[r] = true
				nonGSM = append(nonGSM, r)
			}
		}
	}
	c := NewCounter(enc)
	c.AddString(s)
	per := c.single
	if c.total > c.single {
		per = c.multi
	}
	return Info{
		Encoding: enc, Chars: chars, Units: c.total, Segments: c.Segments(),
		PerSegment: per, Remaining: c.Remaining(), NonGSM: nonGSM,
	}
}

// Segments splits s into the texts of its concatenated parts, never cutting
// a character (escape pair or surrogate pair) between parts. Text that fits
// one SMS is returned as a single element; empty text yields nil. Joining
// the result gives s.
func Segments(s string, enc domain.Encoding) []string {
	if s == "" {
		return nil
	}
	c := NewCounter(enc)
	c.AddString(s)
	if c.total <= c.single {
		return []string{s}
	}
	var out []string
	start, cur := 0, 0
	for i, r := range s {
		u := Units(enc, r)
		if cur+u > c.multi {
			out = append(out, s[start:i])
			start, cur = i, 0
		}
		cur += u
	}
	return append(out, s[start:])
}
