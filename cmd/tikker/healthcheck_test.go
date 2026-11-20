// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// probeServer answers /healthz and /readyz with the given status codes.
func probeServer(t *testing.T, live, ready int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(live)
		case "/readyz":
			w.WriteHeader(ready)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedAddr returns an address with nothing listening on it.
func closedAddr(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.Listener.Addr().String()
	srv.Close()
	return addr
}

func TestRunHealthcheck(t *testing.T) {
	ok := probeServer(t, http.StatusOK, http.StatusOK)
	notReady := probeServer(t, http.StatusOK, http.StatusServiceUnavailable)
	dead := probeServer(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable)

	tests := []struct {
		name       string
		listen     string // TIKKER_LISTEN; "" leaves it unset
		configBody string // written to --config when non-empty
		args       []string
		wantCode   int
		wantErrHas string
	}{
		{name: "ready instance", listen: hostPort(ok), wantCode: 0},
		{name: "not ready is exit 1", listen: hostPort(notReady), wantCode: 1, wantErrHas: "status 503"},
		{name: "live ignores readiness", listen: hostPort(notReady), args: []string{"--live"}, wantCode: 0},
		{name: "live failure is exit 1", listen: hostPort(dead), args: []string{"--live"}, wantCode: 1, wantErrHas: "status 503"},
		{name: "no server is exit 1", listen: closedAddr(t), wantCode: 1, wantErrHas: "GET"},
		{name: "invalid listen is exit 1", listen: "nope", wantCode: 1, wantErrHas: "must be host:port"},
		{name: "listen from config file", configBody: "listen: " + hostPort(ok) + "\n", wantCode: 0},
		{name: "invalid config is exit 1", configBody: "listen: [bad\n", wantCode: 1, wantErrHas: "invalid configuration"},
		{name: "unknown flag is usage error", args: []string{"--bogus"}, wantCode: 2, wantErrHas: "usage: tikker"},
		{name: "extra argument is usage error", args: []string{"extra"}, wantCode: 2, wantErrHas: "usage: tikker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfigEnv(t)
			if tt.listen != "" {
				t.Setenv("TIKKER_LISTEN", tt.listen)
			}
			args := []string{"healthcheck"}
			if tt.configBody != "" {
				args = append([]string{"--config", writeConfig(t, tt.configBody)}, args...)
			}
			args = append(args, tt.args...)

			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if tt.wantErrHas != "" && !strings.Contains(stderr.String(), tt.wantErrHas) {
				t.Errorf("stderr = %q, want containing %q", stderr.String(), tt.wantErrHas)
			}
		})
	}
}

func TestRunHealthcheckHelp(t *testing.T) {
	isolateConfigEnv(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"healthcheck", "-h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "usage: tikker") {
		t.Errorf("stdout = %q, want usage", stdout.String())
	}
}

func TestRunVersionRejectsExtraArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunHealthcheckOnNonLoopbackListen(t *testing.T) {
	// A wildcard listen address must still be probed through loopback.
	srv := probeServer(t, http.StatusOK, http.StatusOK)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	isolateConfigEnv(t)
	t.Setenv("TIKKER_LISTEN", fmt.Sprintf("0.0.0.0:%s", port))

	var stdout, stderr bytes.Buffer
	if code := run([]string{"healthcheck"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
}

// hostPort returns the host:port of a test server.
func hostPort(srv *httptest.Server) string {
	return srv.Listener.Addr().String()
}
