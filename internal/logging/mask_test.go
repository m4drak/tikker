// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package logging

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestMaskPhone(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "polish E.164 (SPEC example)", in: "+48601234567", want: "+48*****4567"},
		{name: "separators are dropped", in: "+48 601-234-567", want: "+48*****4567"},
		{name: "parentheses and dots", in: "+48 (601) 234.567", want: "+48*****4567"},
		{name: "national format with trunk zero", in: "0601234567", want: "06****4567"},
		{name: "ten digits without plus", in: "1234567890", want: "12****7890"},
		{name: "US number", in: "+14155550123", want: "+14*****0123"},
		{name: "short number keeps two digits", in: "123456789", want: "*******89"},
		{name: "five digits", in: "12345", want: "***45"},
		{name: "service code fully masked", in: "112", want: "***"},
		{name: "four digits fully masked", in: "1234", want: "****"},
		{name: "alphanumeric sender keeps ends", in: "MikroTik", want: "M******k"},
		{name: "two letter sender fully masked", in: "AB", want: "**"},
		{name: "one char fully masked", in: "X", want: "*"},
		{name: "plus with letters", in: "+INFO", want: "+I**O"},
		{name: "empty", in: "", want: ""},
		{name: "blank", in: "   ", want: ""},
		{name: "unicode sender", in: "Żółć", want: "Ż**ć"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaskPhone(tt.in); got != tt.want {
				t.Errorf("MaskPhone(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMaskPhoneNeverLeaksWholeNumber(t *testing.T) {
	inputs := []string{
		"+48601234567", "0601234567", "123456789", "12345", "1234",
		"112", "MikroTik", "AB", "+48 601 234 567",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			got := MaskPhone(in)
			if got == in {
				t.Fatalf("MaskPhone(%q) returned the input unchanged", in)
			}
			if !strings.Contains(got, "*") {
				t.Errorf("MaskPhone(%q) = %q contains no mask", in, got)
			}
		})
	}
}

func TestPhoneRedaction(t *testing.T) {
	const number = "+48601234567"
	const masked = "+48*****4567"

	tests := []struct {
		name string
		arg  any
	}{
		{name: "Phone value via %v", arg: Phone(number)},
		{name: "Phone pointer via %v", arg: &[]Phone{number}[0]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := fmt.Sprintf("%v", tt.arg)
			if strings.Contains(out, number) {
				t.Errorf("formatted output %q leaks the number", out)
			}
		})
	}

	t.Run("slog record shows masked value", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		logger.Info("sms", slog.Any("peer", Phone(number)))
		out := buf.String()
		if strings.Contains(out, number) {
			t.Errorf("log %q leaks the number", out)
		}
		if !strings.Contains(out, masked) {
			t.Errorf("log %q does not contain %q", out, masked)
		}
	})
}
