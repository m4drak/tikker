// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package fake

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// decoded is the output of the minimal SMS-DELIVER decoder below.
type decoded struct {
	smsc, sender string
	alnum        bool
	udhi         bool
	dcs          byte
	ts           time.Time
	concat       *Concat
	text         string
}

// decodeDeliver is an independent, minimal SMS-DELIVER decoder written from
// 3GPP TS 23.040 §9.2.2.1 (it shares only the GSM-7 alphabet with the code
// under test).
func decodeDeliver(t *testing.T, pduHex string) decoded {
	t.Helper()
	b, err := hex.DecodeString(pduHex)
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	var d decoded
	i := 0
	semis := func(oct []byte, digits int) string {
		var sb strings.Builder
		for _, o := range oct {
			sb.WriteByte('0' + o&0x0F)
			if o>>4 != 0xF {
				sb.WriteByte('0' + o>>4)
			}
		}
		if out := sb.String(); digits < len(out) {
			return out[:digits]
		}
		return sb.String()
	}
	// SMSC
	if n := int(b[i]); n > 0 {
		toa := b[i+1]
		digits := semis(b[i+2:i+1+n], (n-1)*2)
		if toa == 0x91 {
			digits = "+" + digits
		}
		d.smsc = digits
	}
	i += int(b[i]) + 1
	first := b[i]
	i++
	if first&0x03 != 0 {
		t.Fatalf("not SMS-DELIVER: first octet %02X", first)
	}
	d.udhi = first&0x40 != 0
	// OA
	oaLen := int(b[i])
	toa := b[i+1]
	i += 2
	if toa == 0xD0 {
		d.alnum = true
		n := (oaLen + 1) / 2
		sep := unpack(b[i:i+n], 0, oaLen*4/7)
		d.sender = gsm7Decode(sep)
		i += n
	} else {
		n := (oaLen + 1) / 2
		d.sender = semis(b[i:i+n], oaLen)
		if toa == 0x91 {
			d.sender = "+" + d.sender
		}
		i += n
	}
	i++ // PID
	d.dcs = b[i]
	i++
	ts := b[i : i+7]
	i += 7
	bcd := func(o byte) int { return int(o&0x0F)*10 + int(o>>4) }
	tzq := int(ts[6]&0x07)*10 + int(ts[6]>>4)
	if ts[6]&0x08 != 0 {
		tzq = -tzq
	}
	d.ts = time.Date(2000+bcd(ts[0]), time.Month(bcd(ts[1])), bcd(ts[2]),
		bcd(ts[3]), bcd(ts[4]), bcd(ts[5]), 0, time.FixedZone("", tzq*900))
	udl := int(b[i])
	i++
	ud := b[i:]

	udhLen := 0
	if d.udhi {
		udhLen = int(ud[0]) + 1
		hdr := ud[1:udhLen]
		for j := 0; j < len(hdr); {
			iei, l := hdr[j], int(hdr[j+1])
			body := hdr[j+2 : j+2+l]
			switch iei {
			case 0x00:
				d.concat = &Concat{Ref: uint16(body[0]), Total: int(body[1]), Seq: int(body[2])}
			case 0x08:
				d.concat = &Concat{Ref: uint16(body[0])<<8 | uint16(body[1]), Total: int(body[2]), Seq: int(body[3]), Wide: true}
			}
			j += 2 + l
		}
	}
	if d.dcs&0x0C == 0x08 {
		raw := ud[udhLen:udl]
		if len(raw) != udl-udhLen {
			t.Fatalf("UDL %d inconsistent with %d octets", udl, len(ud))
		}
		units := make([]uint16, 0, len(raw)/2)
		for j := 0; j+1 < len(raw); j += 2 {
			units = append(units, uint16(raw[j])<<8|uint16(raw[j+1]))
		}
		d.text = string(utf16.Decode(units))
	} else {
		startSeptets := (udhLen*8 + 6) / 7
		d.text = gsm7Decode(unpack(ud, startSeptets, udl-startSeptets))
	}
	return d
}

// unpack reads n septets from b starting at septet offset start.
func unpack(b []byte, start, n int) []byte {
	out := make([]byte, n)
	for k := 0; k < n; k++ {
		for bit := 0; bit < 7; bit++ {
			pos := (start+k)*7 + bit
			if b[pos>>3]>>(pos&7)&1 == 1 {
				out[k] |= 1 << bit
			}
		}
	}
	return out
}

func TestGSMTable(t *testing.T) {
	if len(gsmBasicRunes) != 128 {
		t.Fatalf("basic table has %d entries", len(gsmBasicRunes))
	}
	for r, b := range gsmExt {
		if got := gsm7Decode([]byte{gsmEscape, b}); got != string(r) {
			t.Errorf("ext %q decodes to %q", r, got)
		}
	}
	// Spot checks against TS 23.038 Table 6.2.1.
	for r, want := range map[rune]byte{'@': 0x00, '£': 0x01, '$': 0x02, 'Δ': 0x10, 'É': 0x1F, ' ': 0x20, 'A': 0x41, 'Ä': 0x5B, '¿': 0x60, 'z': 0x7A, 'à': 0x7F} {
		if got := gsmBasicIndex[r]; got != want {
			t.Errorf("%q = %#02x, want %#02x", r, got, want)
		}
	}
	if n, ok := GSM7Length("a€b"); !ok || n != 4 {
		t.Errorf("GSM7Length(a€b) = %d, %v", n, ok)
	}
	if IsGSM7("zażółć") || !IsGSM7("zazolc ^{}") {
		t.Error("IsGSM7 wrong")
	}
	if got := gsm7Decode([]byte{gsmEscape, 0x7F, 0x41}); got != "àA" {
		t.Errorf("unknown escape decoded to %q", got)
	}
}

// Vectors: GSM-7 packing of "hellohello" is the classic example
// E8329BFD4697D9EC37 (TS 23.038 §6.1.2.1 algorithm; widely reproduced, e.g.
// Wikipedia "SMS" and the PDUspy documentation). SMSC +27381000015 encoding
// 07917283010010F5 is the TS 23.040 style example used throughout the
// "SMS PDU mode" tutorials. The concatenation UDH 050003CC0201 is the example
// from TS 23.040 §9.2.3.24.1.
func TestKnownVectors(t *testing.T) {
	sep, _ := gsm7Encode("hellohello")
	ud, start := packSeptets(nil, sep)
	if start != 0 || strings.ToUpper(hex.EncodeToString(ud)) != "E8329BFD4697D9EC37" {
		t.Errorf("hellohello packs to %X (start %d)", ud, start)
	}
	smsc, err := encodeSMSC("+27381000015")
	if err != nil || strings.ToUpper(hex.EncodeToString(smsc)) != "07917283010010F5" {
		t.Errorf("SMSC = %X, %v", smsc, err)
	}
	if got := hex.EncodeToString((Concat{Ref: 0xCC, Total: 2, Seq: 1}).udh()); got != "050003cc0201" {
		t.Errorf("udh = %s", got)
	}
	if got := hex.EncodeToString((Concat{Ref: 0x1234, Total: 3, Seq: 2, Wide: true}).udh()); got != "06080412340302" {
		t.Errorf("wide udh = %s", got)
	}
	// SCTS 2026-11-19 12:34:56 +01:00 -> 62 11 91 21 43 65 40 (swapped BCD, tz = 4 quarters).
	ts := time.Date(2026, 11, 19, 12, 34, 56, 0, time.FixedZone("", 3600))
	if got := hex.EncodeToString(scts(ts)); got != "62119121436540" {
		t.Errorf("scts = %s", got)
	}
	// Negative zone -05:30 = -22 quarters: tens digit 2 with sign bit 3 set, units 2.
	neg := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("", -(5*3600+1800)))
	if got := hex.EncodeToString(scts(neg)); got != "6210203040502a" {
		// tz octet: units 2 in high nibble, tens 2 | 0x08 = 0xA in low nibble.
		t.Errorf("negative-zone scts = %s", got)
	}

	// Full PDU for "hellohello" from +46708251358, assembled by hand from the
	// layout in TS 23.040 §9.2.2.1.
	pdu, err := BuildDeliverPDU(PDUParams{
		SMSC: "+27381000015", Sender: "+46708251358",
		Time: ts,
	}, "hellohello")
	if err != nil {
		t.Fatal(err)
	}
	want := "07917283010010F5" + "04" + "0B" + "91" + "6407281553F8" + "00" + "00" + "62119121436540" + "0A" + "E8329BFD4697D9EC37"
	if pdu != want {
		t.Errorf("pdu\n got %s\nwant %s", pdu, want)
	}
}

func TestBuildDeliverRoundTrip(t *testing.T) {
	tz := time.FixedZone("", 2*3600)
	when := time.Date(2026, 11, 19, 7, 8, 9, 0, tz)
	cases := []struct {
		name   string
		p      PDUParams
		text   string
		dcs    byte
		sender string
	}{
		{"auto ucs2", PDUParams{Sender: "+48511222333"}, "Cześć? nie: Czesc! €5 [ok]", 0x08, "+48511222333"}, // not GSM -> auto UCS-2
		{"gsm7 plain", PDUParams{Sender: "+48511222333", SMSC: "+48790998250"}, "Kod: 123456 {ok} ^~|", 0x00, "+48511222333"},
		{"national", PDUParams{Sender: "511222333"}, "x", 0x00, "511222333"},
		{"odd digits", PDUParams{Sender: "+4851122233"}, "odd", 0x00, "+4851122233"},
		{"alnum", PDUParams{Sender: "mBank"}, "Haslo", 0x00, "mBank"},
		{"ucs2", PDUParams{Sender: "+48511222333", Encoding: EncodingUCS2}, "ascii in ucs2", 0x08, "+48511222333"},
		{"emoji", PDUParams{Sender: "+48511222333"}, "hi \U0001F600 żółć", 0x08, "+48511222333"},
		{"class0", PDUParams{Sender: "+48511222333", Class0: true}, "flash", 0x10, "+48511222333"},
		{"empty", PDUParams{Sender: "+48511222333"}, "", 0x00, "+48511222333"},
		{"max gsm7", PDUParams{Sender: "+48511222333"}, strings.Repeat("a", 160), 0x00, "+48511222333"},
		{"max ucs2", PDUParams{Sender: "+48511222333"}, strings.Repeat("ż", 70), 0x08, "+48511222333"},
		{"concat8 gsm7", PDUParams{Sender: "+48511222333", Concat: &Concat{Ref: 7, Total: 2, Seq: 2}}, "second part", 0x00, "+48511222333"},
		{"concat16 ucs2", PDUParams{Sender: "+48511222333", Concat: &Concat{Ref: 0xBEEF, Total: 3, Seq: 1, Wide: true}}, "część pierwsza", 0x08, "+48511222333"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.p.Time = when
			pdu, err := BuildDeliverPDU(c.p, c.text)
			if err != nil {
				t.Fatal(err)
			}
			d := decodeDeliver(t, pdu)
			if d.text != c.text {
				t.Errorf("text = %q, want %q", d.text, c.text)
			}
			if d.sender != c.sender {
				t.Errorf("sender = %q, want %q", d.sender, c.sender)
			}
			if !d.ts.Equal(when) {
				t.Errorf("ts = %v, want %v", d.ts, when)
			}
			if _, off := d.ts.Zone(); off != 2*3600 {
				t.Errorf("tz offset %d", off)
			}
			if c.p.SMSC != "" && d.smsc != c.p.SMSC {
				t.Errorf("smsc = %q", d.smsc)
			}
			if c.p.Concat != nil {
				if d.concat == nil || *d.concat != *c.p.Concat || !d.udhi {
					t.Errorf("concat = %+v udhi=%v", d.concat, d.udhi)
				}
			} else if d.udhi {
				t.Error("unexpected UDH")
			}
			wantDCS := c.dcs
			if c.name == "auto ucs2" || c.name == "emoji" {
				wantDCS = 0x08
			}
			if c.name == "class0" {
				wantDCS = 0x10
			}
			if d.dcs != wantDCS {
				t.Errorf("dcs = %#02x, want %#02x", d.dcs, wantDCS)
			}
		})
	}
}

func TestBuildDeliverErrors(t *testing.T) {
	ok := PDUParams{Sender: "+48511222333"}
	bad := []struct {
		name string
		p    PDUParams
		text string
	}{
		{"empty sender", PDUParams{}, "x"},
		{"long digits", PDUParams{Sender: "+123456789012345678901"}, "x"},
		{"alnum too long", PDUParams{Sender: "abcdefghijkl"}, "x"},
		{"alnum non-gsm", PDUParams{Sender: "żółw"}, "x"},
		{"bad smsc", PDUParams{Sender: "+1", SMSC: "+48x"}, "x"},
		{"forced gsm7", PDUParams{Sender: "+1", Encoding: EncodingGSM7}, "ż"},
		{"too long gsm7", ok, strings.Repeat("a", 161)},
		{"too long ucs2", PDUParams{Sender: "+1", Encoding: EncodingUCS2}, strings.Repeat("a", 71)},
		{"bad concat", PDUParams{Sender: "+1", Concat: &Concat{Ref: 1, Total: 2, Seq: 3}}, "x"},
		{"wide ref in 8-bit", PDUParams{Sender: "+1", Concat: &Concat{Ref: 300, Total: 2, Seq: 1}}, "x"},
		{"gsm7+udh overflow", PDUParams{Sender: "+1", Concat: &Concat{Ref: 1, Total: 2, Seq: 1}}, strings.Repeat("a", 154)},
	}
	for _, c := range bad {
		if _, err := BuildDeliverPDU(c.p, c.text); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
	if _, err := BuildDeliverPDUs(PDUParams{Sender: "+1", Encoding: EncodingGSM7}, "ż", 1, false); err == nil {
		t.Error("BuildDeliverPDUs accepted non-GSM text forced to GSM-7")
	}
	if _, err := BuildDeliverPDUs(PDUParams{Sender: "+1"}, strings.Repeat("a", 153*256), 1, false); err == nil {
		t.Error("expected error for >255 parts")
	}
}

func TestBuildDeliverPDUsSplit(t *testing.T) {
	p := PDUParams{Sender: "+48511222333", Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	long := map[string]string{
		"gsm7": strings.Repeat("abcdefghij", 40) + "€{}", // 403 septets
		"ucs2": strings.Repeat("zażółć gęślą jaźń ", 20) + "\U0001F600\U0001F600",
		// a pair of extension characters straddling the part boundary
		"gsm7 ext edge": strings.Repeat("a", 152) + "€" + strings.Repeat("b", 10),
		// a surrogate pair straddling the boundary
		"ucs2 emoji edge": strings.Repeat("ż", 66) + "\U0001F600" + strings.Repeat("ż", 10),
	}
	for name, text := range long {
		for _, wide := range []bool{false, true} {
			parts, err := BuildDeliverPDUs(p, text, 0x42, wide)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(parts) < 2 {
				t.Fatalf("%s: %d parts", name, len(parts))
			}
			var sb strings.Builder
			for i, pt := range parts {
				d := decodeDeliver(t, pt.PDU)
				if d.concat == nil || d.concat.Seq != i+1 || d.concat.Total != len(parts) || d.concat.Ref != 0x42 || d.concat.Wide != wide {
					t.Fatalf("%s part %d: concat %+v", name, i, d.concat)
				}
				if d.text != pt.Text {
					t.Fatalf("%s part %d: decoded %q != %q", name, i, d.text, pt.Text)
				}
				sb.WriteString(d.text)
			}
			if sb.String() != text {
				t.Errorf("%s wide=%v: reassembled text differs", name, wide)
			}
		}
	}
	// 8-bit GSM-7 parts carry 153 septets, 16-bit 152; UCS-2 67/66 units.
	parts, _ := BuildDeliverPDUs(p, strings.Repeat("a", 400), 1, false)
	if len(parts[0].Text) != 153 {
		t.Errorf("gsm7 part size %d", len(parts[0].Text))
	}
	parts, _ = BuildDeliverPDUs(p, strings.Repeat("a", 400), 1, true)
	if len(parts[0].Text) != 152 {
		t.Errorf("gsm7 wide part size %d", len(parts[0].Text))
	}
	parts, _ = BuildDeliverPDUs(p, strings.Repeat("ż", 200), 1, false)
	if len([]rune(parts[0].Text)) != 67 {
		t.Errorf("ucs2 part size %d", len([]rune(parts[0].Text)))
	}
	// Short text stays a single part without UDH.
	parts, _ = BuildDeliverPDUs(p, "short", 1, false)
	if len(parts) != 1 || decodeDeliver(t, parts[0].PDU).udhi {
		t.Error("short text should be one part without UDH")
	}
}
