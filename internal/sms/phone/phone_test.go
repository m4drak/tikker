// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package phone

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	pl := Options{DefaultRegion: "PL"}
	plShort := Options{DefaultRegion: "PL", AllowShort: true}
	tests := []struct {
		name string
		in   string
		opts Options
		want string
		kind Kind
		err  error
	}{
		{"pl national", "600100200", pl, "+48600100200", KindE164, nil},
		{"pl spaced", "600 100 200", pl, "+48600100200", KindE164, nil},
		{"pl dashes", "600-100-200", pl, "+48600100200", KindE164, nil},
		{"pl with plus", "+48 600 100 200", pl, "+48600100200", KindE164, nil},
		{"pl with 0048", "0048600100200", pl, "+48600100200", KindE164, nil},
		{"pl 48 prefix no plus", "48600100200", pl, "+48600100200", KindE164, nil},
		{"pl parens", "(+48) 600.100.200", pl, "", "", ErrInvalidNumber},
		{"pl landline", "22 123 45 67", pl, "+48221234567", KindE164, nil},
		{"lowercase region", "600100200", Options{DefaultRegion: "pl"}, "+48600100200", KindE164, nil},
		{"german", "+49 30 901820", pl, "+4930901820", KindE164, nil},
		{"us", "+1 (650) 253-0000", pl, "+16502530000", KindE164, nil},
		{"uk mobile", "+44 7911 123456", pl, "+447911123456", KindE164, nil},
		{"uk national under GB", "07911 123456", Options{DefaultRegion: "GB"}, "+447911123456", KindE164, nil},
		{"no region national", "600100200", Options{}, "", "", ErrInvalidNumber},
		{"no region international", "+48600100200", Options{}, "+48600100200", KindE164, nil},
		{"too short pl", "60010020", pl, "", "", ErrInvalidNumber},
		{"too long", "+48 600 100 200 300", pl, "", "", ErrInvalidNumber},
		{"empty", "", pl, "", "", ErrInvalidNumber},
		{"blank", "   ", pl, "", "", ErrInvalidNumber},
		{"garbage", "abc", pl, "", "", ErrInvalidNumber},
		{"letters in number", "600100200x", pl, "", "", ErrInvalidNumber},
		{"vanity rejected", "1-800-FLOWERS", pl, "", "", ErrInvalidNumber},
		{"plus in the middle", "600+100200", pl, "", "", ErrInvalidNumber},
		{"semicolon", "600100200;", pl, "", "", ErrInvalidNumber},
		{"short code off", "8012", pl, "", "", ErrInvalidNumber},
		{"short code on", "8012", plShort, "8012", KindShort, nil},
		{"112 on", "112", plShort, "112", KindShort, nil},
		{"short 6 digits on", "123456", plShort, "123456", KindShort, nil},
		{"7 digits not short", "1234567", plShort, "", "", ErrInvalidNumber},
		{"8 digits pl typo not short", "60010020", plShort, "", "", ErrInvalidNumber},
		{"2 digits not short", "12", plShort, "", "", ErrInvalidNumber},
		{"short with separators rejected", "80-12", plShort, "", "", ErrInvalidNumber},
		{"alnum on", "MyBank", plShort, "MyBank", KindAlphanumeric, nil},
		{"alnum 11", "ABCDEFGHIJK", plShort, "ABCDEFGHIJK", KindAlphanumeric, nil},
		{"alnum 12 too long", "ABCDEFGHIJKL", plShort, "", "", ErrInvalidNumber},
		{"alnum off", "MyBank", pl, "", "", ErrInvalidNumber},
		{"alnum with space and digits", "Bank 24", plShort, "Bank 24", KindAlphanumeric, nil},
		{"alnum bad char", "Bank<>", plShort, "", "", ErrInvalidNumber},
		{"alnum non-ascii", "Bąnk", plShort, "", "", ErrInvalidNumber},
		{"bad region", "600100200", Options{DefaultRegion: "XX"}, "", "", ErrInvalidRegion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.in, tt.opts)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				return
			}
			if got.Value != tt.want || got.Kind != tt.kind || got.String() != tt.want {
				t.Errorf("got %+v, want %q %s", got, tt.want, tt.kind)
			}
		})
	}
}

func TestErrorDoesNotLeakFullNumber(t *testing.T) {
	_, err := Normalize("+48 600 100 29", Options{DefaultRegion: "PL"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "60010029") {
		t.Errorf("error leaks the number: %v", err)
	}
}

func TestMask(t *testing.T) {
	tests := map[string]string{
		"":              "",
		"1":             "1",
		"1234":          "1***",
		"12345":         "1**45",
		"+48600100200":  "+48******200",
		"MyBank":        "M***nk",
		"+4930901820":   "+49*****820",
		"1234567":       "1****67",
		"12345678":      "123**678",
		"zażółć gęślą ": "zaż*******lą ",
	}
	for in, want := range tests {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzNormalize(f *testing.F) {
	for _, s := range []string{"600100200", "+48 600 100 200", "8012", "MyBank", "", "+", "00", "++48", "(", "\x00"} {
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, s string, short bool) {
		n, err := Normalize(s, Options{DefaultRegion: "PL", AllowShort: short})
		if err != nil {
			return
		}
		if n.Kind == KindE164 && !strings.HasPrefix(n.Value, "+") {
			t.Fatalf("E.164 without plus: %q", n.Value)
		}
		if !short && n.Kind != KindE164 {
			t.Fatalf("short number accepted without the flag: %+v", n)
		}
		// Idempotent.
		again, err := Normalize(n.Value, Options{DefaultRegion: "PL", AllowShort: short})
		if err != nil || again != n {
			t.Fatalf("not idempotent: %+v -> %+v (%v)", n, again, err)
		}
	})
}
