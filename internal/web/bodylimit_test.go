// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLimitPanelBody(t *testing.T) {
	var readErr error
	h := limitPanelBody(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"within limit", maxFormBytes, false},
		{"over limit", maxFormBytes + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(strings.Repeat("a", tt.size)))
			h.ServeHTTP(httptest.NewRecorder(), req)
			if (readErr != nil) != tt.wantErr {
				t.Fatalf("read error = %v, want error %v", readErr, tt.wantErr)
			}
		})
	}
}
