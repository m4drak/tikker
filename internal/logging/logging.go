// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package logging builds the process logger on top of log/slog and provides
// helpers that keep personal data out of log lines (SPEC §7).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Supported output formats.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// ParseLevel converts a level name (debug, info, warn, error; any letter
// case) into a slog level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", s)
	}
}

// New returns a logger that writes records at or above level to w in the
// given format (FormatJSON or FormatText). The application logs to stdout
// (SPEC §2.2), so w is normally os.Stdout.
func New(w io.Writer, format string, level slog.Level) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatJSON:
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case FormatText:
		return slog.New(slog.NewTextHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q (want json or text)", format)
	}
}
