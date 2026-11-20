// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package i18n

import (
	"strings"
	"testing"
)

func mustLoad(t *testing.T) *Bundle {
	t.Helper()
	b, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLoadAndKeys(t *testing.T) {
	b := mustLoad(t)
	for _, lang := range Languages {
		if !b.Supported(lang) {
			t.Errorf("%s not supported", lang)
		}
	}
	if b.Supported("de") {
		t.Error("de supported")
	}
	if len(b.Keys()) < 30 {
		t.Errorf("only %d keys", len(b.Keys()))
	}
	for _, k := range b.Keys() {
		if strings.TrimSpace(b.T("pl", k)) == "" || strings.TrimSpace(b.T("en", k)) == "" {
			t.Errorf("empty translation for %s", k)
		}
		// Both languages use the same fmt verbs.
		if strings.Count(b.T("pl", k), "%") != strings.Count(b.T("en", k), "%") {
			t.Errorf("%s: different number of verbs in pl and en", k)
		}
	}
}

func TestT(t *testing.T) {
	b := mustLoad(t)
	tests := []struct {
		lang, key string
		args      []any
		want      string
	}{
		{"pl", "nav.dashboard", nil, "Pulpit"},
		{"en", "nav.dashboard", nil, "Dashboard"},
		{"de", "nav.dashboard", nil, "Dashboard"}, // unknown language: default
		{"en", "no.such.key", nil, "no.such.key"},
		{"en", "login.rate_limited", []any{7}, "Too many sign-in attempts. Try again in 7 s."},
		{"pl", "login.rate_limited", []any{7}, "Zbyt wiele prób logowania. Spróbuj ponownie za 7 s."},
	}
	for _, tt := range tests {
		if got := b.T(tt.lang, tt.key, tt.args...); got != tt.want {
			t.Errorf("T(%s, %s) = %q, want %q", tt.lang, tt.key, got, tt.want)
		}
	}
	if !b.Has("nav.audit") || b.Has("nope") {
		t.Error("Has")
	}
}

func TestMatch(t *testing.T) {
	b := mustLoad(t)
	tests := []struct{ header, want string }{
		{"", Default},
		{"pl", "pl"},
		{"pl-PL,pl;q=0.9,en;q=0.8", "pl"},
		{"en-US,en;q=0.9,pl;q=0.8", "en"},
		{"de-DE,de;q=0.9,pl;q=0.5", "pl"},
		{"de,fr", Default},
		{"en;q=0.3, pl;q=0.7", "pl"},
		{"pl;q=0, en", "en"},
		{"PL", "pl"},
		{"*", Default},
		{"pl;q=abc", "pl"},
		{";;;,,,", Default},
	}
	for _, tt := range tests {
		if got := b.Match(tt.header); got != tt.want {
			t.Errorf("Match(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestCheckDetectsMissingKey(t *testing.T) {
	b := mustLoad(t)
	delete(b.msgs["pl"], "nav.audit")
	if err := b.check(); err == nil || !strings.Contains(err.Error(), "nav.audit") {
		t.Errorf("check = %v", err)
	}
	b = mustLoad(t)
	b.msgs["pl"]["extra.key"] = "x"
	if err := b.check(); err == nil || !strings.Contains(err.Error(), "extra.key") {
		t.Errorf("check = %v", err)
	}
}
