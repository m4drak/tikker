// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestServeAndTokenCreate runs `tikker serve`, creates a token with
// `tikker token create` while it runs, calls the API with it and stops the
// server by cancelling its context (what SIGTERM does).
func TestServeAndTokenCreate(t *testing.T) {
	isolateConfigEnv(t)
	addr := freeAddr(t)
	t.Setenv("TIKKER_LISTEN", addr)
	t.Setenv("TIKKER_ADMIN_PASSWORD", "admin-password-123")
	t.Setenv("TIKKER_LOG_FORMAT", "text")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var serveOut, serveErr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runServe(ctx, "", &serveOut, &serveErr) }()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	get := func(path, token string) int {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := hc.Do(req)
		if err != nil {
			return 0
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	deadline := time.Now().Add(20 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for get("/readyz", "") != http.StatusOK {
		select {
		case code := <-done:
			t.Fatalf("serve exited with %d: %s %s", code, serveOut.String(), serveErr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server not ready")
		}
		<-tick.C
	}

	var tokOut, tokErr bytes.Buffer
	if code := run([]string{"token", "create", "--name", "test", "--scope", "sms:read"}, &tokOut, &tokErr); code != exitOK {
		t.Fatalf("token create = %d: %s", code, tokErr.String())
	}
	token := strings.TrimSpace(tokOut.String())
	if !strings.HasPrefix(token, "tkr_") {
		t.Fatalf("token = %q", token)
	}
	if code := get("/api/v1/messages", token); code != http.StatusOK {
		t.Errorf("GET /api/v1/messages = %d", code)
	}
	if code := get("/api/v1/messages", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous GET = %d", code)
	}

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("serve exit code = %d: %s", code, serveErr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
	if out := serveOut.String(); !strings.Contains(out, "tikker stopped") || strings.Contains(out, "admin-password-123") || strings.Contains(out, token) {
		t.Errorf("serve logs:\n%s", out)
	}
}

func TestServeAndTokenErrors(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantErrHas string
	}{
		{name: "serve with invalid config", args: []string{"serve"}, env: map[string]string{"TIKKER_LISTEN": "nope"}, wantCode: exitConfig, wantErrHas: "TIKKER_LISTEN"},
		{name: "serve with extra arguments", args: []string{"serve", "now"}, wantCode: exitConfig, wantErrHas: "usage: tikker"},
		{name: "token without subcommand", args: []string{"token"}, wantCode: exitConfig, wantErrHas: "usage: tikker"},
		{name: "token unknown subcommand", args: []string{"token", "delete"}, wantCode: exitConfig, wantErrHas: "usage: tikker"},
		{name: "token unknown scope", args: []string{"token", "create", "--scope", "root"}, wantCode: exitConfig, wantErrHas: "scope"},
		{name: "token unknown flag", args: []string{"token", "create", "--owner", "x"}, wantCode: exitConfig, wantErrHas: "owner"},
		{name: "token before first start", args: []string{"token", "create"}, wantCode: exitFailure, wantErrHas: "tikker serve"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfigEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErrHas) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErrHas)
			}
		})
	}
}
