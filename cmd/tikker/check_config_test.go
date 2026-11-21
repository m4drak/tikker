// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateConfigEnv points the data directory at a temp dir and clears the
// variables that would otherwise change the outcome on a developer machine.
func isolateConfigEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TIKKER_DATA_DIR", dir)
	// t.Setenv restores the original value at cleanup; Unsetenv removes the
	// variable for the duration of the test. An empty value would count as
	// set and would override the default.
	for _, k := range []string{"TIKKER_CONFIG", "TIKKER_LISTEN", "TIKKER_SECRET_KEY", "TIKKER_ADMIN_PASSWORD", "TIKKER_LOG_LEVEL", "TIKKER_LOG_FORMAT", "TIKKER_INBOX_POLL_INTERVAL", "TIKKER_RETRY_MAX_ATTEMPTS"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCheckConfig(t *testing.T) {
	validKey := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	const secretKeyPlain = "plain-secret-value-that-must-not-print"

	tests := []struct {
		name       string
		args       func(t *testing.T) []string // built after the environment is isolated
		env        map[string]string
		wantCode   int
		wantOut    []string
		wantOutNot []string
		wantErrHas []string
	}{
		{
			name:     "defaults are valid",
			args:     func(*testing.T) []string { return []string{"--check-config"} },
			wantCode: 0,
			wantOut:  []string{`listen: ":8080"`, `data_dir: `, `retry.max_attempts: 5`},
		},
		{
			name:     "environment overrides defaults",
			args:     func(*testing.T) []string { return []string{"--check-config"} },
			env:      map[string]string{"TIKKER_LISTEN": "127.0.0.1:9191"},
			wantCode: 0,
			wantOut:  []string{`listen: "127.0.0.1:9191"`},
		},
		{
			name: "yaml file is read via --config",
			args: func(t *testing.T) []string {
				path := writeConfig(t, "listen: \":7777\"\nlog_format: text\n")
				return []string{"--config", path, "--check-config"}
			},
			wantCode: 0,
			wantOut:  []string{`listen: ":7777"`, `log_format: "text"`},
		},
		{
			name: "secrets are redacted in output",
			args: func(*testing.T) []string { return []string{"--check-config"} },
			env: map[string]string{
				"TIKKER_SECRET_KEY":     validKey,
				"TIKKER_ADMIN_PASSWORD": secretKeyPlain,
			},
			wantCode:   0,
			wantOut:    []string{"[REDACTED]"},
			wantOutNot: []string{validKey, secretKeyPlain},
		},
		{
			name:       "invalid value exits with config error and names the setting",
			args:       func(*testing.T) []string { return []string{"--check-config"} },
			env:        map[string]string{"TIKKER_LOG_LEVEL": "trace"},
			wantCode:   2,
			wantErrHas: []string{"invalid configuration", "log_level (TIKKER_LOG_LEVEL)"},
		},
		{
			name: "several problems are all listed",
			args: func(*testing.T) []string { return []string{"--check-config"} },
			env: map[string]string{
				"TIKKER_LISTEN":              "nope",
				"TIKKER_INBOX_POLL_INTERVAL": "-5s",
				"TIKKER_RETRY_MAX_ATTEMPTS":  "0",
			},
			wantCode:   2,
			wantErrHas: []string{"listen (TIKKER_LISTEN)", "inbox_poll_interval", "retry.max_attempts"},
		},
		{
			name:       "bad secret key does not echo the value",
			args:       func(*testing.T) []string { return []string{"--check-config"} },
			env:        map[string]string{"TIKKER_SECRET_KEY": "bad-key-material!!"},
			wantCode:   2,
			wantErrHas: []string{"secret_key (TIKKER_SECRET_KEY)"},
		},
		{
			name: "unknown yaml key is an error",
			args: func(t *testing.T) []string {
				return []string{"--config", writeConfig(t, "lisen: \":1\"\n"), "--check-config"}
			},
			wantCode:   2,
			wantErrHas: []string{"lisen"},
		},
		{
			name: "missing explicit config file is an error",
			args: func(t *testing.T) []string {
				return []string{"--config", filepath.Join(t.TempDir(), "absent.yaml"), "--check-config"}
			},
			wantCode:   2,
			wantErrHas: []string{"config file"},
		},
		{
			name:       "check-config takes no command",
			args:       func(*testing.T) []string { return []string{"--check-config", "version"} },
			wantCode:   2,
			wantErrHas: []string{"usage: tikker"},
		},
		{
			name:       "unknown flag",
			args:       func(*testing.T) []string { return []string{"--frobnicate"} },
			wantCode:   2,
			wantErrHas: []string{"usage: tikker", "frobnicate"},
		},
		{
			name:     "help prints usage to stdout",
			args:     func(*testing.T) []string { return []string{"--help"} },
			wantCode: 0,
			wantOut:  []string{"usage: tikker"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfigEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			var stdout, stderr bytes.Buffer
			code := run(tt.args(t), &stdout, &stderr)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			for _, s := range tt.wantOut {
				if !strings.Contains(stdout.String(), s) {
					t.Errorf("stdout %q does not contain %q", stdout.String(), s)
				}
			}
			for _, s := range tt.wantOutNot {
				if strings.Contains(stdout.String(), s) {
					t.Errorf("stdout leaks %q", s)
				}
			}
			for _, s := range tt.wantErrHas {
				if !strings.Contains(stderr.String(), s) {
					t.Errorf("stderr %q does not contain %q", stderr.String(), s)
				}
			}
		})
	}
}

func TestErrorLinesFlattensNestedErrors(t *testing.T) {
	isolateConfigEnv(t)
	t.Setenv("TIKKER_LISTEN", "bad")
	t.Setenv("TIKKER_LOG_FORMAT", "xml")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--check-config"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	lines := strings.Count(stderr.String(), "\n  - ")
	if lines != 2 {
		t.Errorf("got %d problem lines, want 2; stderr:\n%s", lines, stderr.String())
	}
}
