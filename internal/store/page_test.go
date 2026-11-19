// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package store

import "testing"

func TestPageRequestNormalized(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{-1, DefaultPageLimit},
		{0, DefaultPageLimit},
		{1, 1},
		{100, 100},
		{MaxPageLimit, MaxPageLimit},
		{MaxPageLimit + 1, MaxPageLimit},
	}
	for _, tt := range tests {
		got := PageRequest{Cursor: "c", Limit: tt.in}.Normalized()
		if got.Limit != tt.want || got.Cursor != "c" {
			t.Errorf("Normalized(%d) = %+v, want limit %d", tt.in, got, tt.want)
		}
	}
}
