// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/m4drak/tikker/internal/version"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantOut    string
		wantErrHas string
	}{
		{
			name:     "version command",
			args:     []string{"version"},
			wantCode: 0,
			wantOut:  version.String() + "\n",
		},
		{
			name:     "version flag",
			args:     []string{"--version"},
			wantCode: 0,
			wantOut:  version.String() + "\n",
		},
		{
			name:       "no arguments prints usage",
			args:       nil,
			wantCode:   2,
			wantErrHas: "usage: tikker",
		},
		{
			name:       "too many arguments prints usage",
			args:       []string{"version", "extra"},
			wantCode:   2,
			wantErrHas: "usage: tikker",
		},
		{
			name:       "unknown command",
			args:       []string{"frobnicate"},
			wantCode:   2,
			wantErrHas: `unknown command "frobnicate"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if tt.wantOut != "" && stdout.String() != tt.wantOut {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantOut)
			}
			if tt.wantErrHas != "" && !strings.Contains(stderr.String(), tt.wantErrHas) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErrHas)
			}
		})
	}
}
