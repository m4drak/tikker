// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sms/gsm"
)

// SubmitPDU is one SMS-SUBMIT of an outbound message, ready for a modem in
// PDU mode (AT+CMGF=0): the agent writes "AT+CMGS=<Length>" followed by PDU
// and Ctrl-Z.
type SubmitPDU struct {
	// PDU is the hex TPDU preceded by a zero-length SMSC field ("00"), so
	// the modem uses the SMSC stored on the SIM.
	PDU string `json:"pdu"`
	// Length is the TPDU length in octets, without the SMSC field, as
	// AT+CMGS expects it.
	Length int `json:"length"`
}

// SMS-SUBMIT header values (3GPP TS 23.040 §9.2.2.2).
const (
	mtiSubmit     = 0x01 // TP-MTI = SMS-SUBMIT
	vpfRelative   = 0x10 // TP-VPF = relative format
	udhiFlag      = 0x40 // TP-UDHI: user data starts with a header
	validity4Days = 0xAA // TP-VP relative: 4 days
	dcsGSM7       = 0x00
	dcsUCS2       = 0x08
	toaIntl       = 0x91 // type of address: international, ISDN
	toaUnknown    = 0x81 // type of address: unknown, ISDN
	udhConcatLen  = 6    // UDHL + IEI 00 + IEDL + ref + total + seq
	maxAddrDigits = 20
)

var (
	errBadRecipient = errors.New("recipient is not a phone number")
	errNotGSM7      = errors.New("text contains characters outside GSM-7")
	errTooManyParts = errors.New("too many segments")
)

// BuildSubmitPDUs encodes body for "to" as one SMS-SUBMIT per segment, with
// a concatenation header (8-bit reference ref) when there is more than one.
// Segment boundaries follow internal/sms/gsm, the same split the sending
// layer counted.
func BuildSubmitPDUs(to, body string, enc domain.Encoding, ref byte) ([]SubmitPDU, error) {
	addr, err := encodeAddress(to)
	if err != nil {
		return nil, err
	}
	if enc == "" {
		enc = domain.EncodingGSM7
	}
	var dcs byte
	switch enc {
	case domain.EncodingGSM7:
		dcs = dcsGSM7
		if !gsm.IsGSM7(body) {
			return nil, errNotGSM7
		}
	case domain.EncodingUCS2:
		dcs = dcsUCS2
	default:
		return nil, fmt.Errorf("unsupported encoding %q", enc)
	}
	parts := gsm.Segments(body, enc)
	if len(parts) == 0 {
		parts = []string{""}
	}
	if len(parts) > 255 {
		return nil, errTooManyParts
	}
	out := make([]SubmitPDU, 0, len(parts))
	for i, text := range parts {
		var udh []byte
		if len(parts) > 1 {
			udh = []byte{0x05, 0x00, 0x03, ref, byte(len(parts)), byte(i + 1)}
		}
		udl, ud := userData(text, enc, udh)
		first := byte(mtiSubmit | vpfRelative)
		if udh != nil {
			first |= udhiFlag
		}
		tpdu := make([]byte, 0, 8+len(addr)+len(ud))
		tpdu = append(tpdu, first, 0x00) // TP-MR: let the modem assign it
		tpdu = append(tpdu, addr...)
		tpdu = append(tpdu, 0x00, dcs, validity4Days, byte(udl)) // PID, DCS, VP, UDL
		tpdu = append(tpdu, ud...)
		out = append(out, SubmitPDU{PDU: "00" + strings.ToUpper(hex.EncodeToString(tpdu)), Length: len(tpdu)})
	}
	return out, nil
}

// userData returns TP-UDL and TP-UD for one segment.
func userData(text string, enc domain.Encoding, udh []byte) (int, []byte) {
	if enc == domain.EncodingUCS2 {
		ud := append([]byte(nil), udh...)
		for _, u := range utf16.Encode([]rune(text)) {
			ud = append(ud, byte(u>>8), byte(u))
		}
		return len(ud), ud
	}
	septets, _ := gsm.Encode(text) // checked by the caller
	if udh == nil {
		return len(septets), gsm.Pack(septets, 0)
	}
	// The text starts on a septet boundary after the header (TS 23.040
	// §9.2.3.24): fill bits, and UDL counts the header in septets.
	fill := (7 - (len(udh)*8)%7) % 7
	headerSeptets := (len(udh)*8 + fill) / 7
	ud := append(append([]byte(nil), udh...), gsm.Pack(septets, fill)...)
	return headerSeptets + len(septets), ud
}

// encodeAddress encodes TP-DA: digit count, type of address and the digits
// in swapped-nibble BCD. A leading "+" selects the international type.
func encodeAddress(to string) ([]byte, error) {
	s := strings.TrimSpace(to)
	toa := byte(toaUnknown)
	if strings.HasPrefix(s, "+") {
		toa = toaIntl
		s = s[1:]
	}
	if s == "" || len(s) > maxAddrDigits {
		return nil, errBadRecipient
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return nil, errBadRecipient
		}
	}
	out := []byte{byte(len(s)), toa}
	for i := 0; i < len(s); i += 2 {
		lo := s[i] - '0'
		hi := byte(0x0F)
		if i+1 < len(s) {
			hi = s[i+1] - '0'
		}
		out = append(out, hi<<4|lo)
	}
	return out, nil
}
