// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package pdu

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Test vectors.
//
// The structure follows 3GPP TS 23.040 §9.2.2.1 (SMS-DELIVER), the DCS
// TS 23.038 §4 and the UDH §9.2.3.24.1. The first PDU is the widely
// reproduced textbook example ("hellohello", septets E8329BFD4697D9EC37).
// The others were generated with an independent encoder (a short Python
// script written for T14, not the Go code under test) from the texts shown
// in the comments: sender +48600100200, SMSC +48601000000, timestamp
// 2026-11-19 12:34:56 at +01:00 (TZ octet 04 = 4 quarter hours).
const (
	// Textbook example: sender "+27831..." style address with TON/NPI C8
	// (network specific), SCTS 99-03-29 15:16:59 +00:00, text "hellohello".
	vecTextbook = "07917283010010F5040BC87238880900F10000993092516195800AE8329BFD4697D9EC37"

	vecGSM        = "07918406010000F0040B918406100002F00000621191214365400FC8329BFD066DCA1B1E7EE3DB5000" // "Hello €[x]^", with SMSC
	vecGSMNoSMSC  = "040B918406100002F00000621191214365400FC8329BFD066DCA1B1E7EE3DB5000"
	vecUCS2       = "07918406010000F0040B918406100002F000086211912143654022005A0061017C00F301420107002000670119015B006C01050020006A0061017A0144"
	vecEmoji      = "07918406010000F0040B918406100002F00008621191214365400C004800690020D83DDE000021"
	vecAlnum      = "07918406010000F0040BD0CDBC30EC5E0300006211912143654009CB3759078AC96634"
	vecUDH1       = "07918406010000F0440B918406100002F000006211912143654016050003CC0201A061391DF4769741F4329ECE0201"
	vecUDH2       = "07918406010000F0440B918406100002F000006211912143654010050003CC0202E061391D44BFBF5D"
	vecUCS16A     = "07918406010000F0440B918406100002F00008621191214365401506080412340201005A0061017C00F3014201070020"
	vecUCS16B     = "07918406010000F0440B918406100002F0000862119121436540110608041234020200670119015B006C0105"
	vecSMSCPrefix = "07918406010000F0"
)

func mustDecode(t *testing.T, h string, f Format) SMS {
	t.Helper()
	s, err := Decode(h, f)
	if err != nil {
		t.Fatalf("Decode(%s): %v", h, err)
	}
	return s
}

func TestDecodeTextbook(t *testing.T) {
	s := mustDecode(t, vecTextbook, FormatAuto)
	if s.Text != "hellohello" || s.Alphabet != AlphabetGSM7 || s.Class != -1 || s.Concat != nil {
		t.Fatalf("%+v", s)
	}
	if s.SMSC != "+27381000015" {
		t.Errorf("SMSC = %q", s.SMSC)
	}
	if s.Sender != "27838890001" || s.SenderKind != AddressUnknown {
		t.Errorf("sender = %q kind %d", s.Sender, s.SenderKind)
	}
	// SCTS "99 30 92 51 61 95 80": year digits are 2-digit, read as 20yy.
	want := time.Date(2099, 3, 29, 15, 16, 59, 0, time.FixedZone("", 2*3600))
	if !s.Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want %v", s.Timestamp, want)
	}
}

func TestDecodeVectors(t *testing.T) {
	ts := time.Date(2026, 11, 19, 12, 34, 56, 0, time.FixedZone("", 3600))
	tests := []struct {
		name   string
		hex    string
		format Format
		text   string
		alpha  Alphabet
		sender string
		kind   AddressKind
		concat *Concat
	}{
		{"gsm with smsc", vecGSM, FormatWithSMSC, "Hello €[x]^", AlphabetGSM7, "+48600100200", AddressInternational, nil},
		{"gsm auto", vecGSM, FormatAuto, "Hello €[x]^", AlphabetGSM7, "+48600100200", AddressInternational, nil},
		{"gsm without smsc", vecGSMNoSMSC, FormatWithoutSMSC, "Hello €[x]^", AlphabetGSM7, "+48600100200", AddressInternational, nil},
		{"gsm without smsc auto", vecGSMNoSMSC, FormatAuto, "Hello €[x]^", AlphabetGSM7, "+48600100200", AddressInternational, nil},
		{"lowercase hex with spaces", strings.ToLower(vecGSM[:20]) + " " + strings.ToLower(vecGSM[20:]), FormatAuto, "Hello €[x]^", AlphabetGSM7, "+48600100200", AddressInternational, nil},
		{"ucs2 polish", vecUCS2, FormatAuto, "Zażółć gęślą jaźń", AlphabetUCS2, "+48600100200", AddressInternational, nil},
		{"ucs2 emoji surrogates", vecEmoji, FormatAuto, "Hi \U0001F600!", AlphabetUCS2, "+48600100200", AddressInternational, nil},
		{"alphanumeric sender", vecAlnum, FormatAuto, "Kod: 1234", AlphabetGSM7, "MyBank", AddressAlphanumeric, nil},
		{"gsm udh 8-bit ref", vecUDH1, FormatAuto, "Part one text, ", AlphabetGSM7, "+48600100200", AddressInternational, &Concat{Ref: 0xCC, Seq: 1, Total: 2}},
		{"gsm udh part 2", vecUDH2, FormatAuto, "part two.", AlphabetGSM7, "+48600100200", AddressInternational, &Concat{Ref: 0xCC, Seq: 2, Total: 2}},
		{"ucs2 udh 16-bit ref", vecUCS16A, FormatAuto, "Zażółć ", AlphabetUCS2, "+48600100200", AddressInternational, &Concat{Ref: 0x1234, Seq: 1, Total: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustDecode(t, tt.hex, tt.format)
			if s.Text != tt.text || s.Alphabet != tt.alpha || s.Sender != tt.sender || s.SenderKind != tt.kind {
				t.Fatalf("got text=%q alpha=%v sender=%q kind=%v", s.Text, s.Alphabet, s.Sender, s.SenderKind)
			}
			if (s.Concat == nil) != (tt.concat == nil) || (tt.concat != nil && *s.Concat != *tt.concat) {
				t.Errorf("concat = %+v, want %+v", s.Concat, tt.concat)
			}
			if !s.Timestamp.Equal(ts) {
				t.Errorf("timestamp = %v, want %v", s.Timestamp, ts)
			}
			if _, off := s.Timestamp.Zone(); off != 3600 {
				t.Errorf("zone offset %d", off)
			}
			if tt.format != FormatWithoutSMSC && !strings.Contains(tt.name, "without") && s.SMSC != "+48601000000" {
				t.Errorf("SMSC = %q", s.SMSC)
			}
		})
	}
}

func TestDecodeErrors(t *testing.T) {
	// Replace the first-octet MTI with SMS-SUBMIT (01) and status report (10).
	submit := strings.Replace(vecGSM, "040B91", "050B91", 1)
	report := strings.Replace(vecGSM, "040B91", "060B91", 1)
	tests := []struct {
		name string
		hex  string
		f    Format
		want error
	}{
		{"empty", "", FormatAuto, ErrMalformed},
		{"odd", "070", FormatAuto, ErrMalformed},
		{"not hex", "zz", FormatAuto, ErrMalformed},
		{"too long", strings.Repeat("00", 600), FormatAuto, ErrMalformed},
		{"truncated", vecGSM[:30], FormatAuto, ErrMalformed},
		{"truncated user data", vecGSM[:len(vecGSM)-4], FormatWithSMSC, ErrMalformed},
		{"submit", submit, FormatWithSMSC, ErrNotDeliver},
		{"status report", report, FormatWithSMSC, ErrNotDeliver},
		{"smsc too long", "FF" + vecGSM[2:], FormatWithSMSC, ErrMalformed},
		{"compressed", strings.Replace(vecGSM, "F0000062", "F0002062", 1), FormatWithSMSC, ErrUnsupported},
		{"udhi without data", "00" + "44" + "0B918406100002F0" + "0000" + "62119121436540" + "00", FormatWithSMSC, ErrMalformed},
		{"udh longer than data", strings.Replace(vecUDH1, "16050003CC0201", "16FF0003CC0201", 1), FormatWithSMSC, ErrMalformed},
		{"udh element overruns", strings.Replace(vecUDH1, "16050003CC0201", "16050003CC0209", 1), FormatWithSMSC, nil},
		{"address too long", strings.Replace(vecGSM, "040B91", "04FF91", 1), FormatWithSMSC, ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(tt.hex, tt.f)
			if tt.want == nil {
				return // must merely not panic
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUDHElementErrors(t *testing.T) {
	tests := []struct {
		name string
		udh  []byte
		bad  bool
		c    *Concat
	}{
		{"truncated header of element", []byte{0x00}, true, nil},
		{"element overruns", []byte{0x00, 0x05, 1}, true, nil},
		{"port element skipped", []byte{0x05, 0x04, 0x0B, 0x84, 0x23, 0xF0}, false, nil},
		{"concat 8", []byte{0x00, 0x03, 7, 3, 2}, false, &Concat{Ref: 7, Total: 3, Seq: 2}},
		{"concat 16", []byte{0x08, 0x04, 1, 2, 3, 3}, false, &Concat{Ref: 0x0102, Total: 3, Seq: 3}},
		{"seq beyond total ignored", []byte{0x00, 0x03, 7, 2, 3}, false, nil},
		{"zero seq ignored", []byte{0x00, 0x03, 7, 2, 0}, false, nil},
		{"wrong length ignored", []byte{0x00, 0x02, 7, 2}, false, nil},
		{"first concat wins", []byte{0x00, 0x03, 1, 2, 1, 0x00, 0x03, 9, 2, 2}, false, &Concat{Ref: 1, Total: 2, Seq: 1}},
	}
	for _, tt := range tests {
		c, err := parseUDH(tt.udh)
		if (err != nil) != tt.bad {
			t.Errorf("%s: err = %v", tt.name, err)
		}
		if (c == nil) != (tt.c == nil) || (c != nil && *c != *tt.c) {
			t.Errorf("%s: concat = %+v, want %+v", tt.name, c, tt.c)
		}
	}
}

func TestParseSCTS(t *testing.T) {
	tests := []struct {
		name string
		b    []byte
		want time.Time
	}{
		{"positive tz", []byte{0x62, 0x11, 0x91, 0x21, 0x43, 0x65, 0x40}, time.Date(2026, 11, 19, 12, 34, 56, 0, time.FixedZone("", 3600))},
		{"negative tz", []byte{0x62, 0x11, 0x91, 0x21, 0x43, 0x65, 0x58}, time.Date(2026, 11, 19, 12, 34, 56, 0, time.FixedZone("", -4500))},
		{"month 13", []byte{0x62, 0x31, 0x91, 0x21, 0x43, 0x65, 0x40}, time.Time{}},
		{"feb 30", []byte{0x62, 0x20, 0x03, 0x21, 0x43, 0x65, 0x40}, time.Time{}},
		{"non-bcd digit", []byte{0x6A, 0x11, 0x91, 0x21, 0x43, 0x65, 0x40}, time.Time{}},
		{"non-bcd tz", []byte{0x62, 0x11, 0x91, 0x21, 0x43, 0x65, 0xA0}, time.Time{}},
		{"short", []byte{1, 2}, time.Time{}},
	}
	for _, tt := range tests {
		got := parseSCTS(tt.b)
		if !got.Equal(tt.want) {
			t.Errorf("%s: %v want %v", tt.name, got, tt.want)
		}
		if !tt.want.IsZero() {
			if _, o1 := got.Zone(); true {
				_, o2 := tt.want.Zone()
				if o1 != o2 {
					t.Errorf("%s: offset %d want %d", tt.name, o1, o2)
				}
			}
		}
	}
}

func TestParseDCS(t *testing.T) {
	tests := []struct {
		dcs   byte
		alpha Alphabet
		class int
		err   bool
	}{
		{0x00, AlphabetGSM7, -1, false},
		{0x04, AlphabetData8, -1, false},
		{0x08, AlphabetUCS2, -1, false},
		{0x0C, AlphabetGSM7, -1, false}, // reserved alphabet -> default
		{0x10, AlphabetGSM7, 0, false},  // class 0 (flash)
		{0x11, AlphabetGSM7, 1, false},
		{0x18, AlphabetUCS2, 0, false},
		{0x1A, AlphabetUCS2, 2, false},
		{0x20, 0, -1, true}, // compressed
		{0x48, AlphabetUCS2, -1, false},
		{0x60, 0, -1, true},
		{0xC0, AlphabetGSM7, -1, false},
		{0xD0, AlphabetGSM7, -1, false},
		{0xE0, AlphabetUCS2, -1, false},
		{0xF0, AlphabetGSM7, 0, false},
		{0xF4, AlphabetData8, 0, false},
		{0xF5, AlphabetData8, 1, false},
		{0x90, AlphabetGSM7, -1, false}, // reserved group
	}
	for _, tt := range tests {
		a, c, err := parseDCS(tt.dcs)
		if (err != nil) != tt.err || (err == nil && (a != tt.alpha || c != tt.class)) {
			t.Errorf("DCS %#02x: %v %d %v", tt.dcs, a, c, err)
		}
	}
	if AlphabetGSM7.String() != "gsm7" || AlphabetData8.String() != "8bit" || AlphabetUCS2.String() != "ucs2" || Alphabet(9).String() != "unknown" {
		t.Error("Alphabet.String")
	}
}

func TestDecodeText8Bit(t *testing.T) {
	if got := decodeText(AlphabetData8, []byte("héllo")); got != "héllo" {
		t.Errorf("utf8: %q", got)
	}
	if got := decodeText(AlphabetData8, []byte{'h', 0xE9}); got != "hé" {
		t.Errorf("latin1: %q", got)
	}
	if got := decodeText(AlphabetUCS2, []byte{0x00, 0x41, 0x00}); got != "A" {
		t.Errorf("odd ucs2: %q", got)
	}
	if got := decodeText(AlphabetUCS2, []byte{0xD8, 0x3D}); got != "�" {
		t.Errorf("lone surrogate: %q", got)
	}
}

func TestAddressDecoding(t *testing.T) {
	tests := []struct {
		name string
		typ  byte
		data []byte
		n    int
		want string
		kind AddressKind
	}{
		{"international", 0x91, []byte{0x84, 0x06, 0x10, 0x00, 0x02, 0xF0}, 11, "+48600100200", AddressInternational},
		{"national", 0xA1, []byte{0x21, 0x43}, 4, "1234", AddressNational},
		{"unknown", 0x81, []byte{0x21, 0xF3}, 3, "123", AddressUnknown},
		{"star hash", 0x81, []byte{0xBA, 0xDC}, 4, "*#ab", AddressUnknown},
		{"filler stops", 0x81, []byte{0x21, 0xFF}, 4, "12", AddressUnknown},
		{"empty", 0x81, nil, 0, "", AddressUnknown},
	}
	for _, tt := range tests {
		got := decodeAddress(tt.typ, tt.data, tt.n)
		if got.text != tt.want || got.kind != tt.kind {
			t.Errorf("%s: %+v", tt.name, got)
		}
	}
}

func item(t *testing.T, id, h string) Item {
	return Item{ID: id, SMS: mustDecode(t, h, FormatAuto)}
}

func TestAssemble(t *testing.T) {
	t.Run("single passes through", func(t *testing.T) {
		r := Assemble([]Item{item(t, "a", vecGSM), item(t, "b", vecUCS2)})
		if len(r.Complete) != 2 || len(r.Pending) != 0 || r.Complete[0].Text != "Hello €[x]^" || r.Complete[1].Text != "Zażółć gęślą jaźń" {
			t.Fatalf("%+v", r)
		}
		if r.Complete[0].Parts != 1 || r.Complete[0].PartIDs[0] != "a" {
			t.Errorf("%+v", r.Complete[0])
		}
	})
	t.Run("two parts in order", func(t *testing.T) {
		r := Assemble([]Item{item(t, "1", vecUDH1), item(t, "2", vecUDH2)})
		if len(r.Complete) != 1 || len(r.Pending) != 0 {
			t.Fatalf("%+v", r)
		}
		m := r.Complete[0]
		if m.Text != "Part one text, part two." || m.Parts != 2 || strings.Join(m.PartIDs, ",") != "1,2" {
			t.Errorf("%+v", m)
		}
		if m.Sender != "+48600100200" || m.Timestamp.IsZero() {
			t.Errorf("%+v", m)
		}
	})
	t.Run("reversed arrival", func(t *testing.T) {
		r := Assemble([]Item{item(t, "2", vecUDH2), item(t, "1", vecUDH1)})
		if len(r.Complete) != 1 || r.Complete[0].Text != "Part one text, part two." || strings.Join(r.Complete[0].PartIDs, ",") != "1,2" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("incomplete stays pending, then completes", func(t *testing.T) {
		first := item(t, "1", vecUDH1)
		r := Assemble([]Item{first})
		if len(r.Complete) != 0 || len(r.Pending) != 1 || r.Pending[0].ID != "1" {
			t.Fatalf("%+v", r)
		}
		r = Assemble(append(r.Pending, item(t, "2", vecUDH2)))
		if len(r.Complete) != 1 || len(r.Pending) != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("duplicate part consumed", func(t *testing.T) {
		r := Assemble([]Item{item(t, "1", vecUDH1), item(t, "1b", vecUDH1), item(t, "2", vecUDH2)})
		if len(r.Complete) != 1 || strings.Join(r.Complete[0].PartIDs, ",") != "1,2,1b" || r.Complete[0].Text != "Part one text, part two." {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("different senders do not mix", func(t *testing.T) {
		other := item(t, "x", vecUDH2)
		other.SMS.Sender = "+48111222333"
		r := Assemble([]Item{item(t, "1", vecUDH1), other})
		if len(r.Complete) != 0 || len(r.Pending) != 2 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("16-bit ucs2", func(t *testing.T) {
		r := Assemble([]Item{item(t, "a", vecUCS16A), item(t, "b", vecUCS16B)})
		if len(r.Complete) != 1 || r.Complete[0].Text != "Zażółć gęślą" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("order follows first item", func(t *testing.T) {
		r := Assemble([]Item{item(t, "1", vecUDH1), item(t, "s", vecGSM), item(t, "2", vecUDH2)})
		if len(r.Complete) != 2 || r.Complete[0].Parts != 2 || r.Complete[1].Text != "Hello €[x]^" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("invalid concat info treated as single", func(t *testing.T) {
		r := Assemble([]Item{{ID: "x", SMS: SMS{Text: "hi", Concat: &Concat{Seq: 5, Total: 2}}}})
		if len(r.Complete) != 1 || r.Complete[0].Text != "hi" {
			t.Fatalf("%+v", r)
		}
	})
}

func TestAssembleSplitCharacters(t *testing.T) {
	// A surrogate pair split between two UCS-2 parts and an escape pair
	// split between two GSM-7 parts are rejoined correctly.
	p1 := SMS{Sender: "s", Alphabet: AlphabetUCS2, Concat: &Concat{Ref: 1, Seq: 1, Total: 2}, raw: []byte{0x00, 0x41, 0xD8, 0x3D}}
	p2 := SMS{Sender: "s", Alphabet: AlphabetUCS2, Concat: &Concat{Ref: 1, Seq: 2, Total: 2}, raw: []byte{0xDE, 0x00, 0x00, 0x42}}
	r := Assemble([]Item{{ID: "1", SMS: p1}, {ID: "2", SMS: p2}})
	if len(r.Complete) != 1 || r.Complete[0].Text != "A\U0001F600B" {
		t.Fatalf("%+v", r)
	}
	g1 := SMS{Sender: "s", Alphabet: AlphabetGSM7, Concat: &Concat{Ref: 2, Seq: 1, Total: 2}, raw: []byte{0x41, 0x1B}}
	g2 := SMS{Sender: "s", Alphabet: AlphabetGSM7, Concat: &Concat{Ref: 2, Seq: 2, Total: 2}, raw: []byte{0x65, 0x42}}
	r = Assemble([]Item{{ID: "1", SMS: g1}, {ID: "2", SMS: g2}})
	if len(r.Complete) != 1 || r.Complete[0].Text != "A€B" {
		t.Fatalf("%+v", r)
	}
	// Mixed alphabets are decoded run by run.
	g2.Alphabet = AlphabetUCS2
	g2.raw = []byte{0x00, 0x42}
	g1.raw = []byte{0x41}
	r = Assemble([]Item{{ID: "1", SMS: g1}, {ID: "2", SMS: g2}})
	if len(r.Complete) != 1 || r.Complete[0].Text != "AB" {
		t.Fatalf("%+v", r)
	}
}

func FuzzDecode(f *testing.F) {
	for _, s := range []string{vecTextbook, vecGSM, vecGSMNoSMSC, vecUCS2, vecEmoji, vecAlnum, vecUDH1, vecUDH2, vecUCS16A, vecUCS16B, vecSMSCPrefix, "", "00", "FF"} {
		f.Add(s, uint8(0))
		f.Add(s, uint8(1))
		f.Add(s, uint8(2))
	}
	f.Fuzz(func(t *testing.T, h string, format uint8) {
		s, err := Decode(h, Format(format%3))
		if err != nil {
			return
		}
		if s.Concat != nil && (s.Concat.Seq < 1 || s.Concat.Seq > s.Concat.Total) {
			t.Fatalf("bad concat %+v", s.Concat)
		}
		// Assembling must not panic and must never lose an item.
		items := []Item{{ID: "a", SMS: s}, {ID: "b", SMS: s}}
		r := Assemble(items)
		n := len(r.Pending)
		for _, m := range r.Complete {
			n += len(m.PartIDs)
		}
		if n != 2 {
			t.Fatalf("items lost: %+v", r)
		}
	})
}
