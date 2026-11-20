// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package connector

import (
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

func TestCapabilities(t *testing.T) {
	routerOS := Capabilities{
		Encodings:          []domain.Encoding{domain.EncodingGSM7},
		MaxSegments:        1,
		MaxConcurrentSends: 1,
		SendTimeout:        65 * time.Second,
		InboundPDU:         true,
	}
	full := Capabilities{
		Encodings:     []domain.Encoding{domain.EncodingGSM7, domain.EncodingUCS2},
		MaxSegments:   3,
		Concatenation: true,
	}
	tests := []struct {
		name     string
		caps     Capabilities
		enc      domain.Encoding
		segments int
		want     bool
	}{
		{"routeros gsm7 single", routerOS, domain.EncodingGSM7, 1, true},
		{"routeros gsm7 long", routerOS, domain.EncodingGSM7, 2, false},
		{"routeros ucs2", routerOS, domain.EncodingUCS2, 1, false},
		{"zero segments", routerOS, domain.EncodingGSM7, 0, false},
		{"full ucs2 multipart", full, domain.EncodingUCS2, 3, true},
		{"full over limit", full, domain.EncodingUCS2, 4, false},
		{"no capabilities", Capabilities{}, domain.EncodingGSM7, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.caps.Fits(tt.enc, tt.segments); got != tt.want {
				t.Fatalf("Fits(%s, %d) = %v, want %v", tt.enc, tt.segments, got, tt.want)
			}
		})
	}
	if routerOS.SupportsEncoding(domain.EncodingUCS2) || !full.SupportsEncoding(domain.EncodingUCS2) {
		t.Fatal("SupportsEncoding mismatch")
	}
}
