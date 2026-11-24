// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/config"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
)

// TestMailGatewayEndToEnd: an e-mail submitted to the SMTP gateway becomes
// an SMS that the fake router sends, and the gateway stops with the app.
func TestMailGatewayEndToEnd(t *testing.T) {
	router := fake.New(fake.Options{User: routerUser, Password: routerPass, Ports: []string{"lte1"}})
	routerURL := router.Start()
	defer router.Close()

	cfg := testConfig(t)
	cfg.MailGateway.Enabled = true
	cfg.MailGateway.Domain = "sms.test"
	cfg.MailGateway.TLSMode = config.MailTLSNone
	cfg.MailGateway.Listen = "127.0.0.1:2525" // validated; the test listener is used
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := New(ctx, Options{Config: cfg, Logger: log, Listener: ln, MailListener: mln, PasswordParams: fastParams})
	if err != nil {
		t.Fatal(err)
	}
	if a.MailAddr() == nil {
		t.Fatal("MailAddr() = nil with the gateway enabled")
	}
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	_, token, err := CreateToken(context.Background(), cfg, log, TokenRequest{
		Username: auth.BootstrapUsername, Name: "e2e-mail",
		Scopes: domain.Scopes{domain.ScopeAdmin, domain.ScopeSMSSend, domain.ScopeSMSRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	c := &client{t: t, base: "http://" + a.Addr().String(), token: token, http: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
	code, dev := c.do(http.MethodPost, "/api/v1/devices", map[string]any{
		"name": "router", "base_url": routerURL, "username": routerUser, "password": routerPass,
	})
	if code != http.StatusCreated {
		t.Fatalf("create device: %d %v", code, dev)
	}
	code, ch := c.do(http.MethodPost, "/api/v1/channels", map[string]any{"device_id": dev["id"], "port": "lte1"})
	if code != http.StatusCreated {
		t.Fatalf("create channel: %d %v", code, ch)
	}
	waitFor(t, "channel ok", func() bool {
		_, got := c.do(http.MethodGet, fmt.Sprintf("/api/v1/channels/%s", ch["id"]), nil)
		return got["state"] == "ok"
	})

	msg := "From: nas@example.net\r\nTo: 600100200@sms.test\r\nSubject: backup\r\n" +
		"Message-ID: <e2e-1@example.net>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Backup finished: 42 GB\r\n-- \r\nNAS\r\n"
	auth := smtp.PlainAuth("", "token", token, "127.0.0.1")
	for range 2 { // the retry with the same Message-ID is not sent twice
		if err := smtp.SendMail(a.MailAddr().String(), auth, "nas@example.net", []string{"600100200@sms.test"}, []byte(msg)); err != nil {
			t.Fatalf("SendMail: %v", err)
		}
	}
	// A foreign domain is not relayed.
	err = smtp.SendMail(a.MailAddr().String(), auth, "nas@example.net", []string{"someone@example.com"}, []byte(msg))
	if err == nil || !strings.Contains(err.Error(), "550") {
		t.Errorf("foreign domain: %v", err)
	}

	waitFor(t, "SMS sent by the router", func() bool { return len(router.Sent()) > 0 })
	_, list := c.do(http.MethodGet, "/api/v1/messages?direction=out", nil)
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("messages = %v", list)
	}
	m := items[0].(map[string]any)
	waitFor(t, "message sent", func() bool {
		_, got := c.do(http.MethodGet, fmt.Sprintf("/api/v1/messages/%s", m["id"]), nil)
		return got["status"] == "sent"
	})
	sent := router.Sent()
	if len(sent) != 1 || sent[0].Message != "Backup finished: 42 GB" || !strings.HasSuffix(sent[0].PhoneNumber, "600100200") {
		t.Errorf("router got %+v", sent)
	}
	if m["client_ref"] != "e2e-1@example.net" {
		t.Errorf("client_ref = %v", m["client_ref"])
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return")
	}
	out := logs.String()
	for _, want := range []string{"smtp gateway listening", "smtp message queued", "smtp gateway stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs miss %q", want)
		}
	}
	for _, secret := range []string{token, "Backup finished", "600100200"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
}

// TestMailGatewayConfigErrors: a certificate that cannot be loaded stops the
// start-up.
func TestMailGatewayConfigErrors(t *testing.T) {
	cfg := testConfig(t)
	cfg.MailGateway.Enabled = true
	cfg.MailGateway.Domain = "sms.test"
	cfg.MailGateway.TLSCertFile = cfg.DataDir + "/missing.pem"
	cfg.MailGateway.TLSKeyFile = cfg.DataDir + "/missing.key"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = New(ctx, Options{Config: cfg, Logger: slog.New(slog.DiscardHandler), Listener: ln, PasswordParams: fastParams})
	if err == nil || !strings.Contains(err.Error(), "mail gateway tls certificate") {
		t.Errorf("New() = %v", err)
	}
}
