// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package version

import "testing"

func TestString(t *testing.T) {
	tests := []struct {
		name    string
		version string
		commit  string
		date    string
		want    string
	}{
		{
			name:    "defaults for local build",
			version: "dev",
			commit:  "none",
			date:    "unknown",
			want:    "tikker dev (commit none, built unknown)",
		},
		{
			name:    "injected release values",
			version: "v0.1.0",
			commit:  "abc1234",
			date:    "2026-11-19T10:00:00Z",
			want:    "tikker v0.1.0 (commit abc1234, built 2026-11-19T10:00:00Z)",
		},
	}

	origVersion, origCommit, origDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origVersion, origCommit, origDate })

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Version, Commit, Date = tt.version, tt.commit, tt.date
			if got := String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
