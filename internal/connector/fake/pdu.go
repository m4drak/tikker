// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

// Encoding selects the SMS data coding scheme of a built PDU.
type Encoding int

const (
	// EncodingAuto picks GSM-7 when the text fits it and UCS-2 otherwise.
	EncodingAuto Encoding = iota
	// EncodingGSM7 is the 7-bit default alphabet (DCS 0x00).
	EncodingGSM7
	// EncodingUCS2 is UTF-16BE (DCS 0x08).
	EncodingUCS2
)

// Concat describes a concatenated-SMS information element (TS 23.040
// §9.2.3.24.1 for 8-bit, §9.2.3.24.8 for 16-bit reference numbers).
type Concat struct {
	Ref   uint16
	Total int // number of parts, 1..255
	Seq   int // this part, 1..Total
	Wide  bool
}

func (c Concat) validate() error {
	if c.Total < 1 || c.Total > 255 || c.Seq < 1 || c.Seq > c.Total {
		return fmt.Errorf("fake: invalid concat part %d/%d", c.Seq, c.Total)
	}
	if !c.Wide && c.Ref > 255 {
		return errors.New("fake: 8-bit concat reference above 255")
	}
	return nil
}

// udh returns the complete User Data Header including its length octet.
func (c Concat) udh() []byte {
	if c.Wide {
		return []byte{0x06, 0x08, 0x04, byte(c.Ref >> 8), byte(c.Ref), byte(c.Total), byte(c.Seq)}
	}
	return []byte{0x05, 0x00, 0x03, byte(c.Ref), byte(c.Total), byte(c.Seq)}
}

// PDUParams are the SMS-DELIVER header fields.
type PDUParams struct {
	// SMSC is the service centre number ("+48..."); empty encodes "no SMSC"
	// (a single 00 octet).
	SMSC string
	// Sender is a phone number ("+48511222333", "511222333") or an
	// alphanumeric originator (up to 11 GSM-7 characters).
	Sender string
	// Time is the service-centre timestamp; its zone offset is encoded.
	// Zero means 2026-01-01 00:00:00 UTC.
	Time     time.Time
	Encoding Encoding
	// Concat, when set, adds a concatenation UDH; text is then that part only.
	Concat *Concat
	// Class0 sets the "flash SMS" message class (DCS bit 4 + class 0).
	Class0 bool
}

// PDUPart is one built SMS-DELIVER together with the text it carries.
type PDUPart struct {
	PDU  string
	Text string
}

// BuildDeliverPDU builds a hex SMS-DELIVER TPDU preceded by the SMSC field,
// the format found in RouterOS /tool sms inbox "pdu" (ASSUMPTION(T00): the
// real field includes the SMSC prefix).
//
// Layout per 3GPP TS 23.040 §9.2.2.1: SMSC | first octet | OA | PID | DCS |
// SCTS (7 octets) | UDL | UD. GSM-7 user data is packed per TS 23.038
// §6.1.2.1, with fill bits after a UDH so that text starts on a septet
// boundary.
func BuildDeliverPDU(p PDUParams, text string) (string, error) {
	septets, isGSM := gsm7Encode(text)
	enc := p.Encoding
	if enc == EncodingAuto {
		enc = EncodingUCS2
		if isGSM {
			enc = EncodingGSM7
		}
	}
	if enc == EncodingGSM7 && !isGSM {
		return "", errors.New("fake: text is not representable in GSM-7")
	}
	var udh []byte
	if p.Concat != nil {
		if err := p.Concat.validate(); err != nil {
			return "", err
		}
		udh = p.Concat.udh()
	}

	var ud []byte
	var udl int
	dcs := byte(0x00)
	if enc == EncodingGSM7 {
		var start int
		ud, start = packSeptets(udh, septets)
		udl = start + len(septets)
		if udl > 160 {
			return "", fmt.Errorf("fake: %d septets exceed one SMS", udl)
		}
	} else {
		ud = append(ud, udh...)
		for _, u := range utf16.Encode([]rune(text)) {
			ud = append(ud, byte(u>>8), byte(u))
		}
		udl = len(ud)
		if udl > 140 {
			return "", fmt.Errorf("fake: %d octets exceed one SMS", udl)
		}
		dcs = 0x08
	}
	if p.Class0 {
		dcs |= 0x10
	}

	smsc, err := encodeSMSC(p.SMSC)
	if err != nil {
		return "", err
	}
	oaLen, oaType, oa, err := encodeAddress(p.Sender)
	if err != nil {
		return "", err
	}
	first := byte(0x04) // SMS-DELIVER, no more messages to send
	if udh != nil {
		first |= 0x40 // UDHI
	}
	ts := p.Time
	if ts.IsZero() {
		ts = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	b := append([]byte{}, smsc...)
	b = append(b, first, oaLen, oaType)
	b = append(b, oa...)
	b = append(b, 0x00, dcs) // PID, DCS
	b = append(b, scts(ts)...)
	b = append(b, byte(udl))
	b = append(b, ud...)
	return strings.ToUpper(hex.EncodeToString(b)), nil
}

// BuildDeliverPDUs builds the SMS-DELIVER PDUs for text, splitting it into
// concatenated parts when it does not fit one SMS (160 septets / 70 UCS-2
// units). ref and wide set the concatenation reference number and its width.
// A text that fits one SMS yields one part without UDH. The encoding is
// chosen once for the whole text, as handsets do.
func BuildDeliverPDUs(p PDUParams, text string, ref uint16, wide bool) ([]PDUPart, error) {
	if p.Encoding == EncodingAuto {
		p.Encoding = EncodingUCS2
		if IsGSM7(text) {
			p.Encoding = EncodingGSM7
		}
	}
	p.Concat = nil
	chunks, err := splitText(text, p.Encoding, wide)
	if err != nil {
		return nil, err
	}
	parts := make([]PDUPart, 0, len(chunks))
	for i, ch := range chunks {
		pp := p
		if len(chunks) > 1 {
			pp.Concat = &Concat{Ref: ref, Total: len(chunks), Seq: i + 1, Wide: wide}
		}
		pdu, err := BuildDeliverPDU(pp, ch)
		if err != nil {
			return nil, err
		}
		parts = append(parts, PDUPart{PDU: pdu, Text: ch})
	}
	return parts, nil
}

// splitText cuts text into chunks that each fit one SMS in enc.
func splitText(text string, enc Encoding, wide bool) ([]string, error) {
	udhLen := 6
	if wide {
		udhLen = 7
	}
	var limit, single int
	cost := func(r rune) int { return len(utf16.Encode([]rune{r})) }
	if enc == EncodingGSM7 {
		n, ok := GSM7Length(text)
		if !ok {
			return nil, errors.New("fake: text is not representable in GSM-7")
		}
		if n <= 160 {
			return []string{text}, nil
		}
		limit = 160 - (udhLen*8+6)/7 // septets left after the UDH
		cost = func(r rune) int {
			if _, ext := gsmExt[r]; ext {
				return 2
			}
			return 1
		}
	} else {
		single = len(utf16.Encode([]rune(text)))
		if single <= 70 {
			return []string{text}, nil
		}
		limit = (140 - udhLen) / 2 // UTF-16 units per part
	}
	var chunks []string
	var cur strings.Builder
	used := 0
	for _, r := range text {
		c := cost(r)
		if used+c > limit {
			chunks = append(chunks, cur.String())
			cur.Reset()
			used = 0
		}
		cur.WriteRune(r)
		used += c
	}
	chunks = append(chunks, cur.String())
	if len(chunks) > 255 {
		return nil, errors.New("fake: text needs more than 255 SMS parts")
	}
	return chunks, nil
}

// packSeptets packs a UDH (may be nil) followed by septets into user-data
// octets. start is the septet offset where the text begins (UDH length in
// septets, rounded up); it is added to the text length to give the UDL.
func packSeptets(udh []byte, septets []byte) (ud []byte, start int) {
	start = (len(udh)*8 + 6) / 7
	ud = make([]byte, ((start+len(septets))*7+7)/8)
	copy(ud, udh)
	pos := start * 7
	for _, s := range septets {
		for i := 0; i < 7; i++ {
			if s>>i&1 == 1 {
				ud[pos>>3] |= 1 << (pos & 7)
			}
			pos++
		}
	}
	return ud, start
}

// swapDigits encodes decimal digits as semi-octets (low nibble first), padding
// an odd count with 0xF.
func swapDigits(d string) ([]byte, error) {
	for i := 0; i < len(d); i++ {
		if d[i] < '0' || d[i] > '9' {
			return nil, fmt.Errorf("fake: %q is not a digit", d[i])
		}
	}
	out := make([]byte, 0, (len(d)+1)/2)
	for i := 0; i < len(d); i += 2 {
		lo := d[i] - '0'
		hi := byte(0xF)
		if i+1 < len(d) {
			hi = d[i+1] - '0'
		}
		out = append(out, hi<<4|lo)
	}
	return out, nil
}

// encodeSMSC returns the SMSC field including its length octet.
func encodeSMSC(s string) ([]byte, error) {
	if s == "" {
		return []byte{0x00}, nil
	}
	toa := byte(0x81)
	if strings.HasPrefix(s, "+") {
		toa = 0x91
		s = s[1:]
	}
	d, err := swapDigits(s)
	if err != nil {
		return nil, err
	}
	return append([]byte{byte(1 + len(d)), toa}, d...), nil
}

// encodeAddress encodes the originator address: length (digits, or useful
// semi-octets for alphanumeric), type-of-address and the address octets.
func encodeAddress(s string) (length, toa byte, data []byte, err error) {
	if s == "" {
		return 0, 0, nil, errors.New("fake: empty sender")
	}
	digits := strings.TrimPrefix(s, "+")
	if _, derr := swapDigits(digits); derr == nil && digits != "" {
		if len(digits) > 20 {
			return 0, 0, nil, errors.New("fake: sender longer than 20 digits")
		}
		toa = 0x81
		if strings.HasPrefix(s, "+") {
			toa = 0x91
		}
		data, _ = swapDigits(digits)
		return byte(len(digits)), toa, data, nil
	}
	septets, ok := gsm7Encode(s)
	if !ok || len(septets) > 11 {
		return 0, 0, nil, errors.New("fake: invalid alphanumeric sender")
	}
	data, _ = packSeptets(nil, septets)
	return byte((len(septets)*7 + 3) / 4), 0xD0, data, nil
}

// scts encodes a service-centre timestamp (TS 23.040 §9.2.3.11): seven
// octets of swapped-nibble BCD, the last being the zone offset in quarters of
// an hour with bit 3 as the sign.
func scts(t time.Time) []byte {
	bcd := func(v int) byte { return byte(v%10)<<4 | byte(v/10) }
	_, off := t.Zone()
	q := off / 900
	neg := q < 0
	if neg {
		q = -q
	}
	tz := bcd(q)
	if neg {
		tz |= 0x08
	}
	return []byte{
		bcd(t.Year() % 100), bcd(int(t.Month())), bcd(t.Day()),
		bcd(t.Hour()), bcd(t.Minute()), bcd(t.Second()), tz,
	}
}
