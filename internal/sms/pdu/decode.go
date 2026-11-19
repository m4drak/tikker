// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package pdu decodes SMS-DELIVER PDUs (3GPP TS 23.040) as reported by the
// device inbox and reassembles concatenated messages.
//
// RouterOS decodes everything as GSM-7 and hands out every part of a
// multi-part SMS separately (docs/research/routeros-sms.md, W8/W9), so
// tikker reads the "pdu" field itself: DCS selects GSM-7, 8-bit or UCS-2,
// the UDH carries the concatenation info, the address is decoded with its
// type (international, national, alphanumeric).
//
// Nothing here keeps state: [Assemble] takes all parts the caller holds and
// returns what is complete and what is still waiting, so the inbox poller
// (T16) owns persistence.
package pdu

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/sms/gsm"
)

// Errors; test with errors.Is.
var (
	// ErrMalformed: the PDU is not a well-formed SMS-DELIVER.
	ErrMalformed = errors.New("pdu: malformed")
	// ErrNotDeliver: a well-formed PDU of another type (status report,
	// submit, ...).
	ErrNotDeliver = errors.New("pdu: not an SMS-DELIVER")
	// ErrUnsupported: a feature that is not implemented (compressed text).
	ErrUnsupported = errors.New("pdu: unsupported feature")
)

// maxHexLen bounds the input: an SMS PDU is at most ~180 octets; allow generous
// slack for odd SMSC data but refuse megabytes.
const maxHexLen = 1024

// Format says whether the PDU starts with the SMSC information field.
type Format int

// Format values.
const (
	// FormatAuto tries both layouts, see [Decode].
	FormatAuto Format = iota
	// FormatWithSMSC: the PDU starts with the SMSC length octet, as in AT+CMGL / +CMGR text of a modem.
	FormatWithSMSC
	// FormatWithoutSMSC: the PDU starts at the first octet (TP-MTI etc.).
	FormatWithoutSMSC
)

// Alphabet is the character set of the user data (TP-DCS).
type Alphabet int

// Alphabet values.
const (
	AlphabetGSM7  Alphabet = iota // GSM 7-bit default alphabet
	AlphabetData8                 // 8-bit data (decoded best-effort as UTF-8 or Latin-1)
	AlphabetUCS2                  // UTF-16BE, surrogate pairs allowed
)

// String returns "gsm7", "8bit" or "ucs2".
func (a Alphabet) String() string {
	switch a {
	case AlphabetGSM7:
		return "gsm7"
	case AlphabetData8:
		return "8bit"
	case AlphabetUCS2:
		return "ucs2"
	}
	return "unknown"
}

// AddressKind is the type of number of the originating address.
type AddressKind int

// AddressKind values.
const (
	AddressUnknown       AddressKind = iota // unknown or other types; digits as sent
	AddressInternational                    // Sender starts with "+"
	AddressNational                         // digits without country code
	AddressAlphanumeric                     // Sender is text, e.g. "MyBank"
)

// Concat is the concatenation info of a UDH (TS 23.040 §9.2.3.24.1).
type Concat struct {
	Ref   uint16 // reference number (8 or 16 bit)
	Seq   int    // 1-based position
	Total int
}

// SMS is one decoded SMS-DELIVER.
type SMS struct {
	SMSC       string // service centre address, empty if absent
	Sender     string // E.164-like ("+48...") for international, text for alphanumeric
	SenderKind AddressKind
	// Timestamp is the service-centre time stamp with its offset; the zero
	// time if the field was not a valid date.
	Timestamp time.Time
	Alphabet  Alphabet
	// Class is the message class 0..3, or -1 when the DCS carries none.
	Class int
	// Text is the decoded user data of this part alone.
	Text string
	// Concat is non-nil for parts of a concatenated message.
	Concat *Concat

	raw []byte // user data without UDH: septets (GSM-7) or octets
}

// Decode decodes a hex string (case-insensitive, whitespace ignored) as an
// SMS-DELIVER.
//
// ASSUMPTION(T00): the content of RouterOS's "pdu" inbox field is not yet
// measured (docs/research/routeros-sms.md W8). It may be the bare TPDU or
// the full modem PDU with the SMSC information field in front. FormatAuto
// resolves it structurally: it tries "with SMSC" and "without SMSC" and
// takes the first layout whose length fields account for every octet
// exactly; if none does, it falls back to layouts that tolerate trailing
// octets. A PDU that is valid under both readings is read with SMSC. T00
// should fix the format, after which callers pass FormatWithSMSC or
// FormatWithoutSMSC.
func Decode(hexPDU string, f Format) (SMS, error) {
	b, err := fromHex(hexPDU)
	if err != nil {
		return SMS{}, err
	}
	type attempt struct {
		withSMSC, strict bool
	}
	var attempts []attempt
	switch f {
	case FormatWithSMSC:
		attempts = []attempt{{true, false}}
	case FormatWithoutSMSC:
		attempts = []attempt{{false, false}}
	default:
		attempts = []attempt{{true, true}, {false, true}, {true, false}, {false, false}}
	}
	var first error
	for _, a := range attempts {
		s, err := parse(b, a.withSMSC, a.strict)
		if err == nil {
			return s, nil
		}
		if first == nil {
			first = err
		}
	}
	return SMS{}, first
}

func fromHex(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
	if len(s) > maxHexLen {
		return nil, fmt.Errorf("%w: %d hex digits, limit %d", ErrMalformed, len(s), maxHexLen)
	}
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("%w: odd number of hex digits", ErrMalformed)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: not hex", ErrMalformed)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrMalformed)
	}
	return b, nil
}

// reader is a bounds-checked cursor; the first overrun is remembered.
type reader struct {
	b   []byte
	pos int
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil || n < 0 || r.pos+n > len(r.b) {
		if r.err == nil {
			r.err = fmt.Errorf("%w: truncated", ErrMalformed)
		}
		return nil
	}
	out := r.b[r.pos : r.pos+n]
	r.pos += n
	return out
}

func (r *reader) u8() byte {
	if p := r.take(1); p != nil {
		return p[0]
	}
	return 0
}

func (r *reader) rest() []byte { return r.b[r.pos:] }

func parse(b []byte, withSMSC, strict bool) (SMS, error) {
	r := &reader{b: b}
	var s SMS
	s.Class = -1

	if withSMSC {
		l := int(r.u8())
		if l > 12 {
			return SMS{}, fmt.Errorf("%w: SMSC length %d", ErrMalformed, l)
		}
		if l > 0 {
			info := r.take(l)
			if info != nil {
				s.SMSC = decodeAddress(info[0], info[1:], 2*(l-1)).text
			}
		}
	}

	first := r.u8()
	if r.err != nil {
		return SMS{}, r.err
	}
	if first&0x03 != 0 {
		return SMS{}, fmt.Errorf("%w: TP-MTI %d", ErrNotDeliver, first&0x03)
	}
	udhi := first&0x40 != 0

	oaDigits := int(r.u8())
	oaType := r.u8()
	if r.err != nil {
		return SMS{}, r.err
	}
	if oaDigits > 20 {
		return SMS{}, fmt.Errorf("%w: address length %d", ErrMalformed, oaDigits)
	}
	oaBytes := r.take((oaDigits + 1) / 2)
	_ = r.u8() // TP-PID
	dcs := r.u8()
	scts := r.take(7)
	udl := int(r.u8())
	if r.err != nil {
		return SMS{}, r.err
	}
	addr := decodeAddress(oaType, oaBytes, oaDigits)
	s.Sender, s.SenderKind = addr.text, addr.kind
	s.Timestamp = parseSCTS(scts)

	alpha, class, err := parseDCS(dcs)
	if err != nil {
		return SMS{}, err
	}
	s.Alphabet, s.Class = alpha, class

	ud := r.rest()
	need := udl
	if alpha == AlphabetGSM7 {
		need = (udl*7 + 7) / 8
	}
	if len(ud) < need || (strict && len(ud) != need) {
		return SMS{}, fmt.Errorf("%w: user data is %d octets, header says %d", ErrMalformed, len(ud), need)
	}
	ud = ud[:need]

	hdrOctets := 0
	if udhi {
		if len(ud) == 0 {
			return SMS{}, fmt.Errorf("%w: UDHI set without user data", ErrMalformed)
		}
		hdrOctets = int(ud[0]) + 1
		if hdrOctets > len(ud) {
			return SMS{}, fmt.Errorf("%w: UDH longer than user data", ErrMalformed)
		}
		c, err := parseUDH(ud[1:hdrOctets])
		if err != nil {
			return SMS{}, err
		}
		s.Concat = c
	}

	switch alpha {
	case AlphabetGSM7:
		// The header is padded to a septet boundary; the text starts at the
		// septet after it (TS 23.040 §9.2.3.24 / TS 23.038 §6.2.1.2.2).
		skip := (hdrOctets*8 + 6) / 7
		if skip > udl {
			return SMS{}, fmt.Errorf("%w: UDH longer than UDL", ErrMalformed)
		}
		s.raw = gsm.Unpack(ud, udl, 0)[skip:]
	default:
		s.raw = append([]byte(nil), ud[hdrOctets:]...)
	}
	s.Text = decodeText(alpha, s.raw)
	return s, nil
}

// parseUDH walks the information elements and extracts concatenation info
// (IE 0x00 with 8-bit, IE 0x08 with 16-bit reference). Other elements (port
// addressing, ...) are skipped. An inconsistent concatenation element is
// ignored: the part is then delivered as a message of its own rather than
// lost.
func parseUDH(h []byte) (*Concat, error) {
	var out *Concat
	for i := 0; i < len(h); {
		if i+2 > len(h) {
			return nil, fmt.Errorf("%w: UDH element truncated", ErrMalformed)
		}
		id, l := h[i], int(h[i+1])
		if i+2+l > len(h) {
			return nil, fmt.Errorf("%w: UDH element overruns header", ErrMalformed)
		}
		d := h[i+2 : i+2+l]
		var c Concat
		switch {
		case id == 0x00 && l == 3:
			c = Concat{Ref: uint16(d[0]), Total: int(d[1]), Seq: int(d[2])}
		case id == 0x08 && l == 4:
			c = Concat{Ref: uint16(d[0])<<8 | uint16(d[1]), Total: int(d[2]), Seq: int(d[3])}
		}
		if (id == 0x00 || id == 0x08) && c.Total >= 1 && c.Seq >= 1 && c.Seq <= c.Total && out == nil {
			cc := c
			out = &cc
		}
		i += 2 + l
	}
	return out, nil
}

type address struct {
	text string
	kind AddressKind
}

// decodeAddress decodes an address of n semi-octets (digits, or for the
// alphanumeric type the count of useful semi-octets) with type octet typ.
func decodeAddress(typ byte, data []byte, n int) address {
	ton := (typ >> 4) & 0x07
	if ton == 0x05 { // alphanumeric, GSM 7-bit packed
		septets := n * 4 / 7
		return address{gsm.Decode(gsm.Unpack(data, septets, 0)), AddressAlphanumeric}
	}
	var sb strings.Builder
	digits := 0
loop:
	for _, o := range data {
		for _, nib := range [2]byte{o & 0x0F, o >> 4} {
			if digits >= n {
				break loop
			}
			digits++
			switch {
			case nib <= 9:
				sb.WriteByte('0' + nib)
			case nib == 0xA:
				sb.WriteByte('*')
			case nib == 0xB:
				sb.WriteByte('#')
			case nib == 0xC:
				sb.WriteByte('a')
			case nib == 0xD:
				sb.WriteByte('b')
			case nib == 0xE:
				sb.WriteByte('c')
			default: // 0xF filler
				break loop
			}
		}
	}
	switch ton {
	case 0x01:
		return address{"+" + sb.String(), AddressInternational}
	case 0x02:
		return address{sb.String(), AddressNational}
	}
	return address{sb.String(), AddressUnknown}
}

// parseSCTS decodes the 7-octet service centre time stamp (TS 23.040
// §9.2.3.11). The time zone is in quarter hours, sign in bit 3 of the last
// octet. Returns the zero time when any field is invalid.
func parseSCTS(b []byte) time.Time {
	if len(b) != 7 {
		return time.Time{}
	}
	var d [6]int
	for i := range d {
		lo, hi := int(b[i]&0x0F), int(b[i]>>4)
		if lo > 9 || hi > 9 {
			return time.Time{}
		}
		d[i] = lo*10 + hi
	}
	tz := b[6]
	neg := tz&0x08 != 0
	tz &^= 0x08
	lo, hi := int(tz&0x0F), int(tz>>4)
	if lo > 9 || hi > 9 {
		return time.Time{}
	}
	off := (lo*10 + hi) * 15 * 60
	if neg {
		off = -off
	}
	t := time.Date(2000+d[0], time.Month(d[1]), d[2], d[3], d[4], d[5], 0, time.FixedZone("", off))
	if t.Year() != 2000+d[0] || int(t.Month()) != d[1] || t.Day() != d[2] ||
		t.Hour() != d[3] || t.Minute() != d[4] || t.Second() != d[5] {
		return time.Time{} // e.g. month 13 or day 31 in a 30-day month
	}
	return t
}

// parseDCS interprets TP-DCS (TS 23.038 §4).
func parseDCS(dcs byte) (a Alphabet, class int, err error) {
	class = -1
	switch {
	case dcs&0x80 == 0: // coding groups 00xx and 01xx: general data coding
		if dcs&0x20 != 0 {
			return 0, -1, fmt.Errorf("%w: compressed text (DCS %#02x)", ErrUnsupported, dcs)
		}
		switch (dcs >> 2) & 0x03 {
		case 1:
			a = AlphabetData8
		case 2:
			a = AlphabetUCS2
		}
		if dcs&0x10 != 0 {
			class = int(dcs & 0x03)
		}
	case dcs>>4 == 0xE: // message waiting indication, store: UCS-2
		a = AlphabetUCS2
	case dcs>>4 == 0xF: // data coding / message class
		if dcs&0x04 != 0 {
			a = AlphabetData8
		}
		class = int(dcs & 0x03)
	}
	// Groups 0xC and 0xD (message waiting, GSM-7) and the reserved groups
	// 0x8-0xB are read as the default alphabet.
	return a, class, nil
}

func decodeText(a Alphabet, raw []byte) string {
	switch a {
	case AlphabetUCS2:
		if len(raw)%2 == 1 {
			raw = raw[:len(raw)-1]
		}
		u := make([]uint16, len(raw)/2)
		for i := range u {
			u[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
		}
		return string(utf16.Decode(u))
	case AlphabetData8:
		if utf8.Valid(raw) {
			return string(raw)
		}
		r := make([]rune, len(raw)) // ISO 8859-1
		for i, c := range raw {
			r[i] = rune(c)
		}
		return string(r)
	}
	return gsm.Decode(raw)
}
