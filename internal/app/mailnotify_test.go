// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/config"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

// smtpSink accepts every e-mail; the first DATA is refused with 451 so the
// app has to retry.
type smtpSink struct {
	ln   net.Listener
	wg   sync.WaitGroup
	mu   sync.Mutex
	data []string
	seen int
}

func newSMTPSink(t *testing.T) *smtpSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpSink{ln: ln}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(c)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *smtpSink) mails() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.data...)
}

func (s *smtpSink) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	say := func(l string) { _, _ = io.WriteString(c, l+"\r\n") }
	say("220 sink")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		switch verb, _, _ := strings.Cut(strings.TrimSpace(line), " "); strings.ToUpper(verb) {
		case "EHLO":
			say("250-sink")
			say("250-AUTH PLAIN")
			say("250 8BITMIME")
		case "AUTH":
			say("235 ok")
		case "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.seen++
			first := s.seen == 1
			if !first {
				s.data = append(s.data, b.String())
			}
			s.mu.Unlock()
			if first {
				say("451 4.3.0 try again")
			} else {
				say("250 queued")
			}
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// TestMailNotifyEndToEnd: an SMS injected into the fake router reaches the
// owner's mailbox through the relay, after one refused attempt.
func TestMailNotifyEndToEnd(t *testing.T) {
	router := fake.New(fake.Options{User: routerUser, Password: routerPass, Ports: []string{"lte1"}})
	routerURL := router.Start()
	defer router.Close()
	sink := newSMTPSink(t)

	cfg := testConfig(t)
	pwFile := filepath.Join(cfg.DataDir, "relay.pw")
	if err := os.WriteFile(pwFile, []byte("relay-password-9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.MailNotify = config.MailNotifyConfig{
		Enabled: true, SMTPHost: "127.0.0.1", SMTPPort: sink.ln.Addr().(*net.TCPAddr).Port, TLSMode: config.MailTLSNone,
		Username: "tikker", PasswordFile: pwFile, From: "sms@example.net", To: []string{"owner@example.net"},
		Timeout: 5 * time.Second,
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := New(ctx, Options{
		Config: cfg, Logger: log, Listener: ln, PasswordParams: fastParams,
		MailNotifyRetry: domain.RetryPolicy{MaxAttempts: 3, Initial: 100 * time.Millisecond, Multiplier: 2, Max: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	_, token, err := CreateToken(context.Background(), cfg, log, TokenRequest{
		Username: auth.BootstrapUsername, Name: "e2e-notify", Scopes: domain.Scopes{domain.ScopeAdmin},
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
	if code, ch := c.do(http.MethodPost, "/api/v1/channels", map[string]any{"device_id": dev["id"], "port": "lte1"}); code != http.StatusCreated {
		t.Fatalf("create channel: %d %v", code, ch)
	}
	waitFor(t, "inbox poller", func() bool { return a.pollers.count() == 1 })

	if _, err := router.InjectSMS(fake.IncomingSMS{From: "+48511222333", Text: "Kod logowania: 4711"}); err != nil {
		t.Fatal(err)
	}
	// The first attempt gets 451; the retry comes after the (shortened)
	// backoff.
	waitFor(t, "e-mail after a retry", func() bool { return len(sink.mails()) > 0 })
	mails := sink.mails()
	if len(mails) != 1 || !strings.Contains(mails[0], "From: +48511222333") || !strings.Contains(mails[0], "Kod logowania: 4711") {
		t.Fatalf("mails = %q", mails)
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
	for _, want := range []string{"mail notifications started", "will retry", "sms forwarded by e-mail", "mail notifications stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs miss %q", want)
		}
	}
	for _, secret := range []string{"relay-password-9", "511222333", "4711"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
}

func TestNewMailNotifierErrors(t *testing.T) {
	ok := config.MailNotifyConfig{SMTPHost: "127.0.0.1", SMTPPort: 25, TLSMode: config.MailTLSNone, From: "a@example.net", To: []string{"b@example.net"}}
	tests := []struct {
		name string
		mod  func(*config.MailNotifyConfig)
		want string
	}{
		{"missing password file", func(c *config.MailNotifyConfig) { c.PasswordFile = "/nonexistent/pw" }, "password file"},
		{"invalid relay", func(c *config.MailNotifyConfig) { c.From = "nope" }, "mail notifications"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ok
			tt.mod(&c)
			st := testStore(t)
			if _, err := newMailNotifier(c, st, nil, slog.New(slog.DiscardHandler), domain.RetryPolicy{}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("newMailNotifier() = %v, want %q", err, tt.want)
			}
		})
	}
}

func testStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
