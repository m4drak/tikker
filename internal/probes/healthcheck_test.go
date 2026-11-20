// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package probes

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHealthcheckURL(t *testing.T) {
	tests := []struct {
		name    string
		listen  string
		live    bool
		want    string
		wantErr bool
	}{
		{name: "empty host", listen: ":8080", want: "http://127.0.0.1:8080/readyz"},
		{name: "ipv4 wildcard", listen: "0.0.0.0:8080", want: "http://127.0.0.1:8080/readyz"},
		{name: "ipv6 wildcard", listen: "[::]:8080", want: "http://127.0.0.1:8080/readyz"},
		{name: "ipv6 loopback kept", listen: "[::1]:8080", want: "http://[::1]:8080/readyz"},
		{name: "specific ipv4 kept", listen: "10.0.0.5:80", want: "http://10.0.0.5:80/readyz"},
		{name: "hostname kept", listen: "localhost:9000", want: "http://localhost:9000/readyz"},
		{name: "live uses healthz", listen: ":8080", live: true, want: "http://127.0.0.1:8080/healthz"},
		{name: "surrounding space trimmed", listen: " :8080 ", want: "http://127.0.0.1:8080/readyz"},
		{name: "missing port", listen: "0.0.0.0", wantErr: true},
		{name: "non-numeric port", listen: ":http", wantErr: true},
		{name: "port zero", listen: ":0", wantErr: true},
		{name: "port too large", listen: ":70000", wantErr: true},
		{name: "empty", listen: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HealthcheckURL(tt.listen, tt.live)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("url = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		case "/unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not_ready","checks":{"database":"fail"}}`))
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL + "/ok"
	closed.Close()

	tests := []struct {
		name    string
		url     string
		wantErr string
	}{
		{name: "200 is healthy", url: srv.URL + "/ok"},
		{name: "503 is unhealthy", url: srv.URL + "/unavailable", wantErr: "status 503"},
		{name: "404 is unhealthy", url: srv.URL + "/missing", wantErr: "status 404"},
		{name: "redirect followed to 200", url: srv.URL + "/redirect"},
		{name: "no server", url: closedURL, wantErr: "GET " + closedURL},
		{name: "bad url", url: "http://[::1", wantErr: "building request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := Healthcheck(ctx, srv.Client(), tt.url)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Healthcheck: unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "unavailable") && strings.Contains(err.Error(), "database") {
				t.Errorf("error leaks response body: %v", err)
			}
		})
	}
}

func TestHealthcheckContextExpired(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := Healthcheck(ctx, srv.Client(), srv.URL); err == nil {
		t.Fatal("Healthcheck against a stalled server: want error")
	}
}

func TestHealthcheckAgainstLocalListener(t *testing.T) {
	// A wildcard listen address resolves to loopback, so a server bound to
	// 127.0.0.1 is reachable through the URL that HealthcheckURL returns.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" {
				w.WriteHeader(http.StatusOK)
			}
		})},
	}
	srv.Start()
	defer srv.Close()

	listen := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
	target, err := HealthcheckURL(listen, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := Healthcheck(ctx, srv.Client(), target); err != nil {
		t.Fatalf("Healthcheck(%s): %v", target, err)
	}
}
