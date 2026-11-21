// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPanelMountedBesideAPI checks the routing of the real server: the web
// panel owns "/", while the API and the probes keep their paths.
func TestPanelMountedBesideAPI(t *testing.T) {
	cfg := testConfig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := New(ctx, Options{Config: cfg, Logger: slog.New(slog.DiscardHandler), Listener: ln, PasswordParams: fastParams})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-runErr; err != nil {
			t.Error(err)
		}
	}()

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	hc := &http.Client{
		Transport: tr, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	base := "http://" + a.Addr().String()
	get := func(path string) *http.Response {
		t.Helper()
		res, err := hc.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res
	}

	if res := get("/healthz"); res.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d", res.StatusCode)
	}
	if res := get("/api/v1/messages"); res.StatusCode != http.StatusUnauthorized ||
		!strings.HasPrefix(res.Header.Get("Content-Type"), "application/problem+json") {
		t.Errorf("/api/v1/messages = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if res := get("/"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login?next=%2F" {
		t.Errorf("/ = %d -> %s", res.StatusCode, res.Header.Get("Location"))
	}
	res := get("/login")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("/login = %d, CSP %q", res.StatusCode, res.Header.Get("Content-Security-Policy"))
	}
	if res := get("/static/htmx.min.js"); res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") {
		t.Errorf("/static/htmx.min.js = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	// The bootstrap admin signs in and sees the dashboard.
	res, err = hc.PostForm(base+"/login", url.Values{"username": {"admin"}, "password": {adminPassword}})
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || len(res.Cookies()) == 0 {
		t.Fatalf("login = %d, %d cookies", res.StatusCode, len(res.Cookies()))
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/devices", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(res.Cookies()[0])
	res, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("/devices as admin = %d", res.StatusCode)
	}
}
