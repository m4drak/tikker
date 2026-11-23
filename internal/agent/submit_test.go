// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sms/gsm"
)

// tpdu decodes the hex PDU and strips the empty SMSC field.
func tpdu(t *testing.T, p SubmitPDU) []byte {
	t.Helper()
	if !strings.HasPrefix(p.PDU, "00") {
		t.Fatalf("PDU %s does not start with an empty SMSC field", p.PDU)
	}
	b, err := hex.DecodeString(p.PDU[2:])
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != p.Length {
		t.Fatalf("Length = %d, TPDU has %d octets", p.Length, len(b))
	}
	return b
}

func TestBuildSubmitPDUsKnownVector(t *testing.T) {
	// The classic AT+CMGS example: "hellohello" to +46708251358, relative
	// validity 4 days.
	got, err := BuildSubmitPDUs("+46708251358", "hellohello", domain.EncodingGSM7, 0)
	if err != nil {
		t.Fatal(err)
	}
	const want = "0011000B916407281553F80000AA0AE8329BFD4697D9EC37"
	if len(got) != 1 || got[0].PDU != want || got[0].Length != 23 {
		t.Fatalf("got %+v, want %s (23)", got, want)
	}
}

func TestBuildSubmitPDUs(t *testing.T) {
	long := strings.Repeat("abcdefghij", 20)      // 200 GSM-7 chars: 2 parts
	pl := strings.Repeat("zażółć gęślą jaźń ", 5) // 90 UCS-2 chars: 2 parts
	tests := []struct {
		name  string
		to    string
		body  string
		enc   domain.Encoding
		parts int
		dcs   byte
		toa   byte
	}{
		{"gsm7 single national", "600100200", "Hi", domain.EncodingGSM7, 1, dcsGSM7, toaUnknown},
		{"gsm7 default encoding", "+48600100200", "Hi", "", 1, dcsGSM7, toaIntl},
		{"gsm7 concatenated", "+48600100200", long, domain.EncodingGSM7, 2, dcsGSM7, toaIntl},
		{"ucs2 single", "+48600100200", "Zażółć", domain.EncodingUCS2, 1, dcsUCS2, toaIntl},
		{"ucs2 concatenated", "+48600100200", pl, domain.EncodingUCS2, 2, dcsUCS2, toaIntl},
		{"empty body", "+48600100200", "", domain.EncodingGSM7, 1, dcsGSM7, toaIntl},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pdus, err := BuildSubmitPDUs(tt.to, tt.body, tt.enc, 0x42)
			if err != nil {
				t.Fatal(err)
			}
			if len(pdus) != tt.parts {
				t.Fatalf("%d parts, want %d", len(pdus), tt.parts)
			}
			var text strings.Builder
			for i, p := range pdus {
				b := tpdu(t, p)
				multi := tt.parts > 1
				if got := b[0]&udhiFlag != 0; got != multi {
					t.Fatalf("UDHI = %v", got)
				}
				digits := int(b[2])
				if b[3] != tt.toa {
					t.Fatalf("TOA = %02X", b[3])
				}
				off := 4 + (digits+1)/2
				if b[off+1] != tt.dcs {
					t.Fatalf("DCS = %02X", b[off+1])
				}
				udl := int(b[off+3])
				ud := b[off+4:]
				skip := 0
				if multi {
					want := []byte{0x05, 0x00, 0x03, 0x42, byte(tt.parts), byte(i + 1)}
					if string(ud[:6]) != string(want) {
						t.Fatalf("UDH = % X", ud[:6])
					}
					skip = 6
				}
				if tt.dcs == dcsUCS2 {
					if udl != len(ud) {
						t.Fatalf("UDL %d for %d octets", udl, len(ud))
					}
					var u16 []uint16
					for j := skip; j+1 < len(ud); j += 2 {
						u16 = append(u16, uint16(ud[j])<<8|uint16(ud[j+1]))
					}
					text.WriteString(string(utf16.Decode(u16)))
					continue
				}
				fill, hdr := 0, 0
				if multi {
					fill, hdr = 1, 7
				}
				text.WriteString(gsm.Decode(gsm.Unpack(ud[skip:], udl-hdr, fill)))
			}
			if text.String() != tt.body {
				t.Fatalf("decoded %q, want %q", text.String(), tt.body)
			}
		})
	}
}

func TestBuildSubmitPDUsErrors(t *testing.T) {
	tests := []struct {
		name string
		to   string
		body string
		enc  domain.Encoding
	}{
		{"alphanumeric recipient", "Bank", "x", domain.EncodingGSM7},
		{"empty recipient", "+", "x", domain.EncodingGSM7},
		{"too many digits", "+123456789012345678901", "x", domain.EncodingGSM7},
		{"non gsm text as gsm7", "+48600100200", "zażółć", domain.EncodingGSM7},
		{"unknown encoding", "+48600100200", "x", domain.Encoding("latin1")},
		{"too many parts", "+48600100200", strings.Repeat("x", 153*256), domain.EncodingGSM7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BuildSubmitPDUs(tt.to, tt.body, tt.enc, 0); err == nil {
				t.Fatal("no error")
			}
		})
	}
}
