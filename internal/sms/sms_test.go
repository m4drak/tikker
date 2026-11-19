// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sms

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sms/gsm"
	"github.com/m4drak/tikker/internal/sms/phone"
)

var (
	// routerOS mirrors the Capabilities documented in internal/connector:
	// GSM-7 only, one SMS per send.
	routerOS = connector.Capabilities{
		Encodings:   []domain.Encoding{domain.EncodingGSM7},
		MaxSegments: 1,
	}
	// concat is a device that sends UCS-2 and concatenated SMS.
	concat = connector.Capabilities{
		Encodings:     []domain.Encoding{domain.EncodingGSM7, domain.EncodingUCS2},
		MaxSegments:   3,
		Concatenation: true,
	}
)

func TestPlanRouterOS(t *testing.T) {
	a := func(n int) string { return strings.Repeat("a", n) }
	tests := []struct {
		name     string
		body     string
		opts     Options
		wantErr  error
		code     string
		parts    int
		segments int
		body2    string // expected Result.Body when non-empty
		translit bool
		trunc    bool
	}{
		{name: "160 fits", body: a(160), parts: 1, segments: 1},
		{name: "161 rejected", body: a(161), wantErr: ErrTooLong, code: "too_many_segments"},
		{name: "ext at 159+1 rejected", body: a(159) + "€", wantErr: ErrTooLong, code: "too_many_segments"},
		{name: "ext fits exactly", body: a(158) + "€", parts: 1, segments: 1},
		{name: "polish rejected without transliterate", body: "zażółć", wantErr: ErrUnsupportedEncoding, code: "invalid_format"},
		{name: "polish transliterated", body: "zażółć", opts: Options{Transliterate: true}, parts: 1, segments: 1, body2: "zazolc", translit: true},
		{name: "161 truncate", body: a(161), opts: Options{Strategy: StrategyTruncate}, parts: 1, segments: 1, body2: a(160), trunc: true},
		{name: "ext truncate keeps escape pair whole", body: a(159) + "€", opts: Options{Strategy: StrategyTruncate}, parts: 1, segments: 1, body2: a(159), trunc: true},
		{name: "161 split", body: a(161), opts: Options{Strategy: StrategySplit, MaxSegments: 3}, parts: 2, segments: 2},
		{name: "split over max", body: a(400), opts: Options{Strategy: StrategySplit, MaxSegments: 2}, wantErr: ErrTooLong, code: "too_many_segments"},
		{name: "split unlimited", body: a(400), opts: Options{Strategy: StrategySplit}, parts: 3, segments: 3},
		{name: "empty", body: "", wantErr: ErrEmptyBody, code: "invalid_format"},
		{name: "only zero width", body: "\u200b", opts: Options{Transliterate: true}, wantErr: ErrEmptyBody, code: "invalid_format"},
		{name: "invalid utf8", body: "a\xff", wantErr: ErrInvalidText, code: "invalid_format"},
		{name: "bad strategy", body: "x", opts: Options{Strategy: "nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Plan(tt.body, routerOS, tt.opts)
			if tt.name == "bad strategy" {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if c := ErrorCode(err); c != tt.code {
					t.Errorf("ErrorCode = %q, want %q", c, tt.code)
				}
				return
			}
			if len(got.Parts) != tt.parts || got.Segments != tt.segments {
				t.Fatalf("parts=%d segments=%d, want %d %d", len(got.Parts), got.Segments, tt.parts, tt.segments)
			}
			if tt.body2 != "" && got.Body != tt.body2 {
				t.Errorf("Body = %q, want %q", got.Body, tt.body2)
			}
			if got.Transliterated != tt.translit || got.Truncated != tt.trunc {
				t.Errorf("translit=%v trunc=%v", got.Transliterated, got.Truncated)
			}
			if got.Encoding != domain.EncodingGSM7 {
				t.Errorf("Encoding = %s", got.Encoding)
			}
			for _, p := range got.Parts {
				if !routerOS.Fits(got.Encoding, p.Segments) {
					t.Errorf("part does not fit device: %+v", p)
				}
			}
		})
	}
}

func TestPlanSplitPrefixes(t *testing.T) {
	body := strings.Repeat("słowo ", 60) // 360 chars -> several parts
	got, err := Plan(body, routerOS, Options{Transliterate: true, Strategy: StrategySplit, MaxSegments: 5})
	if err != nil {
		t.Fatal(err)
	}
	n := len(got.Parts)
	if n < 3 {
		t.Fatalf("expected several parts, got %d", n)
	}
	var joined strings.Builder
	re := regexp.MustCompile(`^\((\d+)/(\d+)\) `)
	for i, p := range got.Parts {
		m := re.FindStringSubmatch(p.Text)
		if m == nil || m[1] != fmt.Sprint(i+1) || m[2] != fmt.Sprint(n) {
			t.Fatalf("part %d prefix: %q", i, p.Text)
		}
		if p.Index != i+1 || p.Of != n || p.Segments != 1 {
			t.Errorf("part meta: %+v", p)
		}
		if gsm.Analyze(p.Text).Units > 160 {
			t.Errorf("part %d too long", i)
		}
		joined.WriteString(p.Text[len(m[0]):])
	}
	if joined.String() != got.Body {
		t.Error("parts without prefixes do not join to Body")
	}
	// Parts should break after whitespace when possible.
	for i, p := range got.Parts[:n-1] {
		if !strings.HasSuffix(p.Text, " ") {
			t.Errorf("part %d does not end at a word boundary: %q", i, p.Text[len(p.Text)-8:])
		}
	}
}

func TestPlanSplitTenPlusParts(t *testing.T) {
	// More than 9 parts: the prefix grows from "(1/9) " to "(1/10) ".
	body := strings.Repeat("x", 1500)
	got, err := Plan(body, routerOS, Options{Strategy: StrategySplit})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Parts) < 10 {
		t.Fatalf("parts = %d", len(got.Parts))
	}
	for _, p := range got.Parts {
		if gsm.Analyze(p.Text).Units > 160 || !strings.HasSuffix(strings.SplitN(p.Text, " ", 2)[0], fmt.Sprintf("/%d)", len(got.Parts))) {
			t.Errorf("bad part %q", p.Text[:12])
		}
	}
}

func TestPlanSplitUCS2(t *testing.T) {
	body := strings.Repeat("ą", 200)
	got, err := Plan(body, connector.Capabilities{
		Encodings: []domain.Encoding{domain.EncodingUCS2}, MaxSegments: 1,
	}, Options{Strategy: StrategySplit})
	if err != nil {
		t.Fatal(err)
	}
	if got.Encoding != domain.EncodingUCS2 || len(got.Parts) != 4 {
		t.Fatalf("enc=%s parts=%d", got.Encoding, len(got.Parts))
	}
	for _, p := range got.Parts {
		if gsm.Analyze(p.Text).Units > 70 {
			t.Errorf("part over 70 units: %q", p.Text)
		}
	}
}

func TestPlanSplitEmojiNotCut(t *testing.T) {
	body := strings.Repeat("😀", 100)
	got, err := Plan(body, connector.Capabilities{
		Encodings: []domain.Encoding{domain.EncodingUCS2}, MaxSegments: 1,
	}, Options{Strategy: StrategySplit})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Parts {
		if !utf8.ValidString(p.Text) || strings.ContainsRune(p.Text, utf8.RuneError) {
			t.Fatalf("broken part %q", p.Text)
		}
	}
}

func TestPlanConcatenatingDevice(t *testing.T) {
	a := func(n int) string { return strings.Repeat("a", n) }
	tests := []struct {
		name     string
		body     string
		opts     Options
		err      error
		enc      domain.Encoding
		parts    int
		segments int
	}{
		{"single", "hello", Options{}, nil, domain.EncodingGSM7, 1, 1},
		{"153*2", a(306), Options{}, nil, domain.EncodingGSM7, 1, 2},
		{"three", a(459), Options{}, nil, domain.EncodingGSM7, 1, 3},
		{"four rejected", a(460), Options{}, ErrTooLong, "", 0, 0},
		{"polish stays ucs2", "zażółć", Options{}, nil, domain.EncodingUCS2, 1, 1},
		{"polish 71", strings.Repeat("ą", 71), Options{}, nil, domain.EncodingUCS2, 1, 2},
		{"user max 2 rejects 3", a(459), Options{MaxSegments: 2}, ErrTooLong, "", 0, 0},
		{"user max 2 truncates", a(459), Options{MaxSegments: 2, Strategy: StrategyTruncate}, nil, domain.EncodingGSM7, 1, 2},
		{"user max cannot be split away", a(459), Options{MaxSegments: 2, Strategy: StrategySplit}, ErrTooLong, "", 0, 0},
		{"too long for device, split into concatenated parts", a(1000), Options{Strategy: StrategySplit}, nil, domain.EncodingGSM7, 3, 3 + 3 + 1 + 0},
		{"transliterate even if ucs2 possible", "zażółć", Options{Transliterate: true}, nil, domain.EncodingGSM7, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Plan(tt.body, concat, tt.opts)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if err != nil {
				return
			}
			if got.Encoding != tt.enc || len(got.Parts) != tt.parts {
				t.Fatalf("enc=%s parts=%d", got.Encoding, len(got.Parts))
			}
			if tt.name != "too long for device, split into concatenated parts" && got.Segments != tt.segments {
				t.Errorf("segments = %d, want %d", got.Segments, tt.segments)
			}
			for _, p := range got.Parts {
				if !concat.Fits(got.Encoding, p.Segments) {
					t.Errorf("part does not fit: %+v", p)
				}
			}
		})
	}
}

func TestPlanEncodingFallbacks(t *testing.T) {
	ucsOnly := connector.Capabilities{Encodings: []domain.Encoding{domain.EncodingUCS2}, MaxSegments: 1}
	got, err := Plan("hello", ucsOnly, Options{})
	if err != nil || got.Encoding != domain.EncodingUCS2 {
		t.Fatalf("GSM text on UCS-2-only device: %+v %v", got, err)
	}
	if _, err := Plan("hello", connector.Capabilities{MaxSegments: 1}, Options{}); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Errorf("no encodings: %v", err)
	}
	// Device that cannot carry even one segment.
	_, err = Plan("hello", connector.Capabilities{Encodings: []domain.Encoding{domain.EncodingGSM7}}, Options{})
	if !errors.Is(err, ErrTooLong) {
		t.Errorf("MaxSegments 0: %v", err)
	}
	for _, s := range []Strategy{StrategySplit, StrategyTruncate} {
		_, err = Plan("hello", connector.Capabilities{Encodings: []domain.Encoding{domain.EncodingGSM7}}, Options{Strategy: s})
		if !errors.Is(err, ErrTooLong) {
			t.Errorf("MaxSegments 0 with %s: %v", s, err)
		}
	}
}

func TestPlanReportsSubstitutions(t *testing.T) {
	got, err := Plan("hi 😀 żółw", routerOS, Options{Transliterate: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "hi ? zolw" || len(got.Substituted) != 1 || got.Substituted[0].Char != '😀' {
		t.Errorf("got %q %+v", got.Body, got.Substituted)
	}
}

func TestParseStrategy(t *testing.T) {
	for in, want := range map[string]Strategy{"": StrategyReject, "reject": StrategyReject, "split": StrategySplit, "truncate": StrategyTruncate} {
		if got, err := ParseStrategy(in); err != nil || got != want {
			t.Errorf("ParseStrategy(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseStrategy("x"); err == nil {
		t.Error("want error")
	}
}

func TestErrorCode(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("w: %w", ErrTooLong), "too_many_segments"},
		{fmt.Errorf("w: %w", phone.ErrInvalidNumber), "invalid_phone_number"},
		{ErrUnsupportedEncoding, "invalid_format"},
		{ErrEmptyBody, "invalid_format"},
		{ErrInvalidText, "invalid_format"},
		{errors.New("other"), ""},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := ErrorCode(tt.err); got != tt.want {
			t.Errorf("ErrorCode(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestAnalyze(t *testing.T) {
	in := Analyze("Zażółć")
	if in.Encoding != domain.EncodingUCS2 || in.Segments != 1 || in.Remaining != 64 {
		t.Errorf("%+v", in)
	}
	in, tr := AnalyzeTransliterated("Zażółć")
	if in.Encoding != domain.EncodingGSM7 || in.Remaining != 154 || tr.Text != "Zazolc" {
		t.Errorf("%+v %+v", in, tr)
	}
}

func FuzzPlan(f *testing.F) {
	for _, s := range []string{"", "hello", strings.Repeat("a", 161), strings.Repeat("€", 90), "zażółć gęślą jaźń " + strings.Repeat("x", 300), "😀😀😀", strings.Repeat("ą", 140)} {
		f.Add(s, true, uint8(1), uint8(3), false)
		f.Add(s, false, uint8(2), uint8(0), true)
	}
	f.Fuzz(func(t *testing.T, body string, translit bool, strategyIdx, maxSeg uint8, concatenates bool) {
		caps := routerOS
		if concatenates {
			caps = concat
		}
		opts := Options{
			Transliterate: translit,
			Strategy:      []Strategy{StrategyReject, StrategySplit, StrategyTruncate}[int(strategyIdx)%3],
			MaxSegments:   int(maxSeg % 6),
		}
		res, err := Plan(body, caps, opts)
		if err != nil {
			return
		}
		if len(res.Parts) == 0 || res.Segments == 0 {
			t.Fatalf("no parts: %+v", res)
		}
		if opts.MaxSegments > 0 && res.Segments > opts.MaxSegments {
			t.Fatalf("segments %d over max %d", res.Segments, opts.MaxSegments)
		}
		total := 0
		var joined strings.Builder
		for i, p := range res.Parts {
			if !utf8.ValidString(p.Text) || p.Index != i+1 || p.Of != len(res.Parts) {
				t.Fatalf("bad part %+v", p)
			}
			c := gsm.NewCounter(res.Encoding)
			c.AddString(p.Text)
			if c.Segments() != p.Segments || !caps.Fits(res.Encoding, p.Segments) {
				t.Fatalf("part does not fit: %+v (counted %d)", p, c.Segments())
			}
			total += p.Segments
			text := p.Text
			if len(res.Parts) > 1 {
				idx := strings.Index(text, ") ")
				text = text[idx+2:]
			}
			joined.WriteString(text)
		}
		if total != res.Segments {
			t.Fatalf("segment sum %d != %d", total, res.Segments)
		}
		if joined.String() != res.Body {
			t.Fatalf("parts do not join to Body")
		}
		if !opts.Transliterate && !res.Truncated && res.Body != body {
			t.Fatalf("body changed without transliteration or truncation")
		}
		if res.Truncated && !strings.HasPrefix(body, res.Body) && opts.Transliterate == false {
			t.Fatalf("truncated body is not a prefix")
		}
	})
}
