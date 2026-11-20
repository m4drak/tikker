// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector/fake"
)

// start runs the command on a free port and returns its address, captured
// stdout and a stop function that waits for a clean exit.
func start(t *testing.T, args ...string) (addr string, out *bytes.Buffer, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out = &bytes.Buffer{}
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, append([]string{"-listen", "127.0.0.1:0"}, args...), out, io.Discard, ready)
	}()
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	return addr, out, func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}
}

func get(t *testing.T, c *http.Client, url string, auth bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if auth {
		req.SetBasicAuth("u", "p")
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestRunPlainWithControl(t *testing.T) {
	addr, out, stop := start(t, "-user", "u", "-password", "p", "-ports", "lte1, lte2", "-control", "-lenient")
	defer stop()
	c := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + addr
	if code, body := get(t, c, base+"/rest/interface/lte", true); code != 200 || !strings.Contains(body, "lte2") {
		t.Errorf("lte = %d %s", code, body)
	}
	if code, _ := get(t, c, base+"/rest/interface/lte", false); code != 401 {
		t.Errorf("unauthenticated = %d", code)
	}
	res, err := c.Post(base+"/_fake/inject", "application/json", strings.NewReader(`{"from":"+48511222333","text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("inject = %d", res.StatusCode)
	}
	if code, body := get(t, c, base+"/rest/tool/sms/inbox", true); code != 200 || !strings.Contains(body, `"hi"`) {
		t.Errorf("inbox = %d %s", code, body)
	}
	if !strings.Contains(out.String(), "listening on http://") || !strings.Contains(out.String(), "control API: true") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestRunControlDisabled(t *testing.T) {
	addr, _, stop := start(t)
	defer stop()
	c := &http.Client{Timeout: 5 * time.Second}
	// Without -control the path falls through to the RouterOS emulation
	// (which needs credentials and does not know the path).
	if code, _ := get(t, c, "http://"+addr+"/_fake/sent", false); code != 401 {
		t.Errorf("control path without flag = %d, want 401 from the emulation", code)
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/_fake/sent", nil)
	req.SetBasicAuth("tikker", "tikker-test")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 400 {
		t.Errorf("control path without flag = %d, want RouterOS-style 400", res.StatusCode)
	}
}

func TestRunTLSFingerprint(t *testing.T) {
	addr, out, stop := start(t, "-tls", "-tls-hosts", "fake.local, 10.0.0.9", "-user", "u", "-password", "p")
	defer stop()
	m := regexp.MustCompile(`certificate SHA-256: ([0-9A-F:]+)\n`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no fingerprint in %q", out.String())
	}
	var seen string
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // pinning by fingerprint below, as tikker does
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			sum := sha256.Sum256(raw[0])
			seen = fake.FormatFingerprint(sum[:])
			return nil
		},
	}}}
	if code, _ := get(t, c, "https://"+addr+"/rest/system/resource", true); code != 200 {
		t.Errorf("status = %d", code)
	}
	if seen != m[1] {
		t.Errorf("printed fingerprint %s != served certificate %s", m[1], seen)
	}
	if !strings.Contains(out.String(), "listening on https://") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestRunErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), []string{"-bogus"}, io.Discard, &buf, nil); err == nil {
		t.Error("bad flag accepted")
	}
	if err := run(context.Background(), []string{"-ports", " , "}, io.Discard, io.Discard, nil); err == nil {
		t.Error("empty ports accepted")
	}
	if err := run(context.Background(), []string{"-listen", "256.0.0.1:1"}, io.Discard, io.Discard, nil); err == nil {
		t.Error("bad listen address accepted")
	}
}
