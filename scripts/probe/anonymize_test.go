// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestStringMasksIdentifiers(t *testing.T) {
	a := NewAnonymizer()
	cases := []struct{ name, in string }{
		{"plus phone", "dzwoni +48 600 700 800 teraz"},
		{"digits phone", "numer 600700800 koniec"},
		{"ipv4", "router 192.168.88.1 odpowiada"},
		{"public ip", "wan 83.12.34.56"},
		{"ipv6", "adres 2001:db8:85a3::8a2e:370:7334"},
		{"mac", "mac AA:BB:CC:DD:EE:FF ok"},
		{"imei", "imei 356938035643809 x"},
		{"iccid", "iccid 8948010000012345678 x"},
	}
	for _, c := range cases {
		out := a.String(c.in)
		if out == c.in {
			t.Errorf("%s: brak zmiany: %q", c.name, out)
		}
		if !strings.Contains(out, "<") {
			t.Errorf("%s: brak znacznika: %q", c.name, out)
		}
	}
}

func TestStringKeepsHarmlessText(t *testing.T) {
	a := NewAnonymizer()
	for _, s := range []string{
		"2026-11-19 12:30:45+01:00", "7.16.2 (stable)", "12:30:45", "tikker probe ascii 160",
		"Zażółć gęślą jaźń", "uptime 1w2d3h4m5s",
	} {
		if out := a.String(s); out != s {
			t.Errorf("zmieniono niewrażliwy tekst %q -> %q", s, out)
		}
	}
}

func TestConsistentPlaceholders(t *testing.T) {
	a := NewAnonymizer()
	a.AddLiteral("PHONE", "+48 600 700 800")
	x := a.String("do +48600700800")
	y := a.String("od 48600700800 oraz 600700800 i +48 600 700 800")
	if !strings.Contains(x, "<PHONE-1>") {
		t.Fatalf("x = %q", x)
	}
	if strings.Count(y, "<PHONE-1>") < 2 {
		t.Errorf("warianty tego samego numeru powinny mieć ten sam znacznik: %q", y)
	}
	other := a.String("+48 511 222 333")
	if !strings.Contains(other, "<PHONE-2>") {
		t.Errorf("inny numer powinien dostać nowy znacznik: %q", other)
	}
}

func TestLiteralPasswordAndHost(t *testing.T) {
	a := NewAnonymizer()
	a.AddLiteral("PASSWORD", "s3cr3t-Hasło")
	a.AddLiteral("HOST", "router.example.org")
	out := a.String("https://router.example.org/rest haslo s3cr3t-Hasło")
	if strings.Contains(out, "s3cr3t") || strings.Contains(out, "example.org") {
		t.Errorf("wyciek: %q", out)
	}
}

func TestValueByKey(t *testing.T) {
	a := NewAnonymizer()
	in := map[string]any{
		"phone-number": "+48600700800", "imei": "123", "serial-number": "ABC123",
		"password": "x", "secret": "", "message": "treść", "version": "7.16.2",
		"nested": []any{map[string]any{"phone": "+48 600 700 800", "message": "ąę"}},
	}
	out := a.Value("", in, false).(map[string]any)
	if out["phone-number"] != "<PHONE-1>" || out["imei"] != "<IMEI-1>" || out["serial-number"] != "<SERIAL-1>" {
		t.Errorf("klucze tożsamości: %v", out)
	}
	if out["password"] != "<REDACTED>" {
		t.Errorf("hasło: %v", out["password"])
	}
	if out["secret"] != "" {
		t.Errorf("puste sekrety zostają puste: %v", out["secret"])
	}
	if out["message"] != "treść" || out["version"] != "7.16.2" {
		t.Errorf("zmieniono niewrażliwe pola: %v", out)
	}
	nested := out["nested"].([]any)[0].(map[string]any)
	if nested["phone"] != "<PHONE-1>" {
		t.Errorf("ten sam numer w różnych zapisach: %v", nested)
	}
	red := a.Value("", in, true).(map[string]any)
	if m := red["message"].(string); !strings.HasPrefix(m, "<MESSAGE runes=5 bytes=") {
		t.Errorf("redakcja treści: %q", m)
	}
}

func TestBodyJSONAndText(t *testing.T) {
	a := NewAnonymizer()
	v := a.Body([]byte(`{"phone":"+48600700800","n":5}`), false).(map[string]any)
	if v["phone"] != "<PHONE-1>" {
		t.Errorf("%v", v)
	}
	s := a.Body([]byte("<html>10.0.0.1</html>"), false).(string)
	if strings.Contains(s, "10.0.0.1") {
		t.Errorf("%q", s)
	}
	if a.Body(nil, false) != nil {
		t.Error("pusty body powinien dać nil")
	}
}

func TestHeadersMaskAuthorization(t *testing.T) {
	a := NewAnonymizer()
	h := http.Header{}
	h.Set("Authorization", "Basic dGVzdDp0ZXN0")
	h.Set("Set-Cookie", "sid=abc")
	h.Set("X-Forwarded-For", "10.1.2.3")
	h.Set("Content-Type", "application/json")
	out := a.Headers(h)
	if out["Authorization"] != "<REDACTED>" || out["Set-Cookie"] != "<REDACTED>" {
		t.Errorf("%v", out)
	}
	if strings.Contains(out["X-Forwarded-For"], "10.1.2.3") {
		t.Errorf("%v", out)
	}
	if out["Content-Type"] != "application/json" {
		t.Errorf("%v", out)
	}
}

func TestAnalyzeSMS(t *testing.T) {
	cases := []struct {
		name string
		text string
		enc  string
		unit int
		seg  int
	}{
		{"ascii-160", fill(asciiPattern, 160), "gsm7", 160, 1},
		{"ascii-161", fill(asciiPattern, 161), "gsm7", 161, 2},
		{"ascii-320", fill(asciiPattern, 320), "gsm7", 320, 3},
		{"ascii-500", fill(asciiPattern, 500), "gsm7", 500, 4},
		{"pl-70", fill(polishPattern, 70), "ucs2", 70, 1},
		{"pl-71", fill(polishPattern, 71), "ucs2", 71, 2},
		{"ext-80", strings.Repeat("{", 80), "gsm7", 160, 1},
		{"ext-81", strings.Repeat("{", 81), "gsm7", 162, 2},
		{"emoji", "a😀", "ucs2", 3, 1},
		{"empty", "", "gsm7", 0, 0},
	}
	for _, c := range cases {
		got := AnalyzeSMS(c.text)
		if got.Encoding != c.enc || got.Units != c.unit || got.Segments != c.seg {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

func TestSuiteCasesHaveNoLongDigitRuns(t *testing.T) {
	a := NewAnonymizer()
	for _, c := range SuiteCases() {
		if out := a.String(c.Text); out != c.Text {
			t.Errorf("%s: treść testowa byłaby zamaskowana: %q", c.Name, out)
		}
	}
	var names = map[string]bool{}
	for _, c := range SuiteCases() {
		if names[c.Name] {
			t.Errorf("zduplikowana nazwa %s", c.Name)
		}
		names[c.Name] = true
	}
}
