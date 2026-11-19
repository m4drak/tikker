// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package gsm

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/domain"
)

func TestAnalyzeBoundaries(t *testing.T) {
	a := func(n int) string { return strings.Repeat("a", n) }
	pl := func(n int) string { return strings.Repeat("ą", n) }
	tests := []struct {
		name      string
		text      string
		enc       domain.Encoding
		units     int
		segs      int
		remaining int
		perSeg    int
	}{
		{"empty", "", domain.EncodingGSM7, 0, 0, 160, 160},
		{"one char", "a", domain.EncodingGSM7, 1, 1, 159, 160},
		{"gsm 160", a(160), domain.EncodingGSM7, 160, 1, 0, 160},
		{"gsm 161", a(161), domain.EncodingGSM7, 161, 2, 153*2 - 161, 153},
		{"gsm 306", a(306), domain.EncodingGSM7, 306, 2, 0, 153},
		{"gsm 307", a(307), domain.EncodingGSM7, 307, 3, 153 - 1, 153},
		{"gsm 153 stays single", a(153), domain.EncodingGSM7, 153, 1, 7, 160},
		{"gsm 154 stays single", a(154), domain.EncodingGSM7, 154, 1, 6, 160},
		{"ext counts twice", "€", domain.EncodingGSM7, 2, 1, 158, 160},
		{"ext all", "€[]{}^\\|~", domain.EncodingGSM7, 18, 1, 142, 160},
		{"ext at 159+1 -> 161", a(159) + "€", domain.EncodingGSM7, 161, 2, 153*2 - 161, 153},
		{"ext fits exactly 160", a(158) + "€", domain.EncodingGSM7, 160, 1, 0, 160},
		{"ext straddles 153", a(152) + "€" + a(10), domain.EncodingGSM7, 164, 2, 153 - 12, 153},
		{"polish is ucs2", "ą", domain.EncodingUCS2, 1, 1, 69, 70},
		{"ucs 70", pl(70), domain.EncodingUCS2, 70, 1, 0, 70},
		{"ucs 71", pl(71), domain.EncodingUCS2, 71, 2, 134 - 71, 67},
		{"ucs 134", pl(134), domain.EncodingUCS2, 134, 2, 0, 67},
		{"ucs 135", pl(135), domain.EncodingUCS2, 135, 3, 67 - 1, 67},
		{"emoji two units", "😀", domain.EncodingUCS2, 2, 1, 68, 70},
		{"emoji at the end of 70", pl(69) + "😀", domain.EncodingUCS2, 71, 2, 134 - 71, 67},
		{"emoji across 67 boundary", pl(66) + "😀" + pl(10), domain.EncodingUCS2, 78, 2, 67 - 12, 67},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := Analyze(tt.text)
			if in.Encoding != tt.enc || in.Units != tt.units || in.Segments != tt.segs {
				t.Fatalf("got enc=%s units=%d segs=%d, want %s %d %d", in.Encoding, in.Units, in.Segments, tt.enc, tt.units, tt.segs)
			}
			if in.Remaining != tt.remaining {
				t.Errorf("Remaining = %d, want %d", in.Remaining, tt.remaining)
			}
			if in.PerSegment != tt.perSeg {
				t.Errorf("PerSegment = %d, want %d", in.PerSegment, tt.perSeg)
			}
			if in.Chars != utf8.RuneCountInString(tt.text) {
				t.Errorf("Chars = %d", in.Chars)
			}
		})
	}
}

func TestAnalyzeNonGSM(t *testing.T) {
	in := Analyze("zażółć żółć 😀")
	want := []rune{'ż', 'ó', 'ł', 'ć', '😀'}
	if string(in.NonGSM) != string(want) {
		t.Errorf("NonGSM = %q, want %q", string(in.NonGSM), string(want))
	}
	if Analyze("ok").NonGSM != nil {
		t.Error("NonGSM for GSM text must be empty")
	}
	if got := Analyze("a\xffb"); got.Encoding != domain.EncodingUCS2 || got.Chars != 3 {
		t.Errorf("invalid UTF-8: %+v", got)
	}
}

func TestSegmentsNeverSplitsCharacters(t *testing.T) {
	// "€" at positions where it would straddle the 153 boundary.
	for pad := 150; pad <= 153; pad++ {
		s := strings.Repeat("a", pad) + "€€" + strings.Repeat("b", 10)
		segs := Segments(s, domain.EncodingGSM7)
		if strings.Join(segs, "") != s {
			t.Fatalf("pad %d: parts do not join to input", pad)
		}
		c := NewCounter(domain.EncodingGSM7)
		for i, p := range segs {
			pc := NewCounter(domain.EncodingGSM7)
			pc.AddString(p)
			if pc.Units() > GSMMulti {
				t.Errorf("pad %d part %d: %d units", pad, i, pc.Units())
			}
			c.AddString(p)
		}
		if len(segs) != Analyze(s).Segments {
			t.Errorf("pad %d: %d parts, Analyze says %d", pad, len(segs), Analyze(s).Segments)
		}
	}
	// Emoji at the UCS-2 boundary: 66 BMP chars + emoji must move whole to part 2.
	s := strings.Repeat("ą", 66) + "😀" + "xxxxxxxxxx"
	segs := Segments(s, domain.EncodingUCS2)
	if len(segs) != 2 || segs[0] != strings.Repeat("ą", 66) || segs[1] != "😀xxxxxxxxxx" {
		t.Errorf("emoji boundary: %q", segs)
	}
	if got := Segments("", domain.EncodingGSM7); got != nil {
		t.Errorf("empty: %v", got)
	}
	if got := Segments("short", domain.EncodingGSM7); len(got) != 1 || got[0] != "short" {
		t.Errorf("short: %v", got)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	all := string(basic[:0x1B]) + string(basic[0x1C:]) + "\f^{}\\[~]|€"
	sept, ok := Encode(all)
	if !ok {
		t.Fatal("Encode failed on the full alphabet")
	}
	if got := Decode(sept); got != all {
		t.Errorf("round trip: %q != %q", got, all)
	}
	if _, ok := Encode("zażółć"); ok {
		t.Error("Encode must fail on non-GSM text")
	}
	if !IsGSM7("Hello €[]{}^\\|~") || IsGSM7("ł") || IsGSM7("\xff") {
		t.Error("IsGSM7")
	}
}

func TestDecodeEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"trailing escape dropped", []byte{0x41, 0x1B}, "A"},
		{"unknown extension shows basic char", []byte{0x1B, 0x41}, "A"},
		{"escape escape is space", []byte{0x1B, 0x1B}, " "},
		{"high bit ignored", []byte{0xC1}, "A"},
		{"euro", []byte{0x1B, 0x65}, "€"},
	}
	for _, tt := range tests {
		if got := Decode(tt.in); got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}
}

func TestPackKnownVectors(t *testing.T) {
	// "hello" -> E8329BFD06 and "hellohello" -> E8329BFD4697D9EC37 are the
	// textbook examples for septet packing (TS 23.038 §6.1.2.1.1).
	tests := []struct {
		text string
		hex  string
	}{
		{"hello", "e8329bfd06"},
		{"hellohello", "e8329bfd4697d9ec37"},
	}
	for _, tt := range tests {
		sept, _ := Encode(tt.text)
		got := Pack(sept, 0)
		if !bytes.Equal(got, mustHex(t, tt.hex)) {
			t.Errorf("Pack(%q) = %x, want %s", tt.text, got, tt.hex)
		}
		if back := Unpack(got, len(sept), 0); !bytes.Equal(back, sept) {
			t.Errorf("Unpack mismatch for %q", tt.text)
		}
	}
	// With fill bits: one fill bit shifts everything by one bit.
	sept, _ := Encode("a")
	if got := Pack(sept, 1); !bytes.Equal(got, []byte{0xC2}) {
		t.Errorf("Pack fill = %x", got)
	}
	if got := Unpack([]byte{0xC2}, 1, 1); !bytes.Equal(got, sept) {
		t.Errorf("Unpack skip = %x", got)
	}
	if got := Unpack([]byte{0x01}, 5, 0); len(got) != 1 {
		t.Errorf("short data should yield fewer septets, got %d", len(got))
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := range b {
		var v byte
		for _, c := range []byte(s[2*i : 2*i+2]) {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= c - '0'
			case c >= 'a' && c <= 'f':
				v |= c - 'a' + 10
			}
		}
		b[i] = v
	}
	return b
}

func TestTransliterate(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		changed bool
		subs    string
	}{
		{"ascii untouched", "Hello", "Hello", false, ""},
		{"polish lower", "ąćęłńóśźż", "acelnoszz", true, ""},
		{"polish upper", "ĄĆĘŁŃÓŚŹŻ", "ACELNOSZZ", true, ""},
		{"sentence", "Zażółć gęślą jaźń", "Zazolc gesla jazn", true, ""},
		{"gsm letters kept", "é ü ñ Ø ß", "é ü ñ Ø ß", false, ""},
		{"mixed keeps gsm accents", "Café żółw", "Café zolw", true, ""},
		{"curly quotes", "‘a’ “b” „c”", "'a' \"b\" \"c\"", true, ""},
		{"dashes", "a–b—c", "a-b-c", true, ""},
		{"ellipsis", "wait…", "wait...", true, ""},
		{"nbsp and thin space", "a\u00a0b\u2009c", "a b c", true, ""},
		{"zero width dropped", "a\u200bb\ufeffc", "abc", true, ""},
		{"other latin", "čšžřěů", "cszreu", true, ""},
		{"turkish", "İstanbul ğış", "Istanbul gis", true, ""},
		{"ligature", "ﬁle", "file", true, ""},
		{"copyright etc", "© ® ™", "(c) (R) (TM)", true, ""},
		{"emoji", "ok 😀", "ok ?", true, "😀"},
		{"cjk counted", "日本日", "???", true, "日本"},
		{"combining mark alone dropped", "á", "a", true, ""},
		{"tab to space", "a\tb", "a b", true, ""},
		{"invalid utf8", "a\xffb", "a?b", true, "�"},
		{"empty", "", "", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Transliterate(tt.in)
			if got.Text != tt.want || got.Changed != tt.changed {
				t.Fatalf("got %q changed=%v, want %q %v", got.Text, got.Changed, tt.want, tt.changed)
			}
			var subs []rune
			for _, s := range got.Substituted {
				subs = append(subs, s.Char)
			}
			if string(subs) != tt.subs {
				t.Errorf("Substituted = %q, want %q", string(subs), tt.subs)
			}
			if !IsGSM7(got.Text) {
				t.Errorf("result %q is not GSM-7", got.Text)
			}
		})
	}
	r := Transliterate("😀😀x😀日")
	if len(r.Substituted) != 2 || r.Substituted[0].Count != 3 || r.Substituted[1].Count != 1 {
		t.Errorf("counts: %+v", r.Substituted)
	}
}

func TestTransliterateAlwaysGSM7(t *testing.T) {
	// Every code point below U+3000 must come out as GSM-7.
	for r := rune(0); r < 0x3000; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		if out := Transliterate(string(r)).Text; !IsGSM7(out) {
			t.Fatalf("U+%04X -> %q is not GSM-7", r, out)
		}
	}
}

func TestUnits(t *testing.T) {
	if Units(domain.EncodingGSM7, '€') != 2 || Units(domain.EncodingGSM7, 'a') != 1 {
		t.Error("gsm units")
	}
	if Units(domain.EncodingUCS2, '😀') != 2 || Units(domain.EncodingUCS2, '€') != 1 {
		t.Error("ucs2 units")
	}
	if s, m := Limits(domain.EncodingUCS2); s != 70 || m != 67 {
		t.Error("ucs2 limits")
	}
}

func FuzzSegments(f *testing.F) {
	for _, s := range []string{
		"", "a", strings.Repeat("a", 161), strings.Repeat("€", 100), "zażółć", "😀",
		strings.Repeat("ą", 66) + "😀", strings.Repeat("a", 152) + "€", "\xff\xfe",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return
		}
		info := Analyze(s)
		enc := info.Encoding
		parts := Segments(s, enc)
		if strings.Join(parts, "") != s {
			t.Fatalf("parts do not join to input: %q", parts)
		}
		if len(parts) != info.Segments {
			t.Fatalf("%d parts, Analyze says %d segments", len(parts), info.Segments)
		}
		single, multi := Limits(enc)
		limit := multi
		if len(parts) == 1 {
			limit = single
		}
		for _, p := range parts {
			c := NewCounter(enc)
			c.AddString(p)
			if c.Units() > limit || c.Units() == 0 {
				t.Fatalf("part with %d units, limit %d", c.Units(), limit)
			}
			if !utf8.ValidString(p) {
				t.Fatalf("part is not valid UTF-8")
			}
		}
		tr := Transliterate(s)
		if !IsGSM7(tr.Text) {
			t.Fatalf("transliteration not GSM-7: %q", tr.Text)
		}
		if enc == domain.EncodingGSM7 {
			sept, ok := Encode(s)
			if !ok || Decode(sept) != s || len(sept) != info.Units {
				t.Fatalf("GSM-7 round trip failed")
			}
			if got := Decode(Unpack(Pack(sept, 0), len(sept), 0)); got != s {
				t.Fatalf("pack round trip failed")
			}
		}
	})
}
