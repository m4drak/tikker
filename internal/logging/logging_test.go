// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    slog.Level
		wantErr bool
	}{
		{name: "debug", in: "debug", want: slog.LevelDebug},
		{name: "info", in: "info", want: slog.LevelInfo},
		{name: "warn mixed case", in: " Warn ", want: slog.LevelWarn},
		{name: "error upper case", in: "ERROR", want: slog.LevelError},
		{name: "empty is unknown", in: "", wantErr: true},
		{name: "unknown level", in: "verbose", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLevel(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseLevel(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name      string
		format    string
		level     slog.Level
		wantErr   bool
		wantInLog string // substring expected for an info record
		wantDebug bool   // whether a debug record should be written
	}{
		{name: "json info", format: FormatJSON, level: slog.LevelInfo, wantInLog: `"msg":"hello"`},
		{name: "text info", format: FormatText, level: slog.LevelInfo, wantInLog: `msg=hello`},
		{name: "json upper case", format: "JSON", level: slog.LevelInfo, wantInLog: `"msg":"hello"`},
		{name: "debug filtered at info", format: FormatJSON, level: slog.LevelInfo, wantDebug: false},
		{name: "debug written at debug", format: FormatText, level: slog.LevelDebug, wantDebug: true},
		{name: "unknown format", format: "xml", level: slog.LevelInfo, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := New(&buf, tt.format, tt.level)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			logger.Info("hello")
			logger.Debug("details")

			out := buf.String()
			if tt.wantInLog != "" && !strings.Contains(out, tt.wantInLog) {
				t.Errorf("output %q does not contain %q", out, tt.wantInLog)
			}
			if got := strings.Contains(out, "details"); got != tt.wantDebug {
				t.Errorf("debug record written = %v, want %v (output %q)", got, tt.wantDebug, out)
			}
		})
	}
}

func TestNewJSONIsValidJSON(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(&buf, FormatJSON, slog.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("sent", PhoneAttr("peer", "+48601234567"))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("record is not JSON: %v (%q)", err, buf.String())
	}
	if got := rec["peer"]; got != "+48*****4567" {
		t.Errorf("peer = %v, want masked number", got)
	}
}
