// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"bytes"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
)

const (
	tUser = "tikker"
	tPass = "s3cr3t-Pa55"
)

var tNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// logBuf is a goroutine-safe buffer behind a debug-level slog handler.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type env struct {
	srv  *fake.Server
	url  string
	conn *Conn
	logs *logBuf
	clk  *clock.Fake
}

type setup struct {
	fake  fake.Options
	cfg   func(*connector.DeviceConfig)
	opts  func(*Options)
	tls   bool             // StartTLS (httptest certificate)
	cert  *tls.Certificate // serve this certificate over TLS instead
	noRun bool             // do not start the fake (custom URL in cfg)
}

func newEnv(t *testing.T, s setup) *env {
	t.Helper()
	if s.fake.User == "" {
		s.fake.User, s.fake.Password = tUser, tPass
	}
	if len(s.fake.Ports) == 0 {
		s.fake.Ports = []string{"lte1", "lte2"}
	}
	e := &env{srv: fake.New(s.fake), logs: &logBuf{}, clk: clock.NewFake(tNow)}
	switch {
	case s.noRun:
		t.Cleanup(e.srv.Close)
	case s.cert != nil:
		ts := httptest.NewUnstartedServer(e.srv)
		ts.TLS = &tls.Config{Certificates: []tls.Certificate{*s.cert}, MinVersion: tls.VersionTLS12}
		ts.StartTLS()
		e.url = ts.URL
		t.Cleanup(func() { e.srv.Close(); ts.Close() })
	case s.tls:
		e.url = e.srv.StartTLS()
		t.Cleanup(e.srv.Close)
	default:
		e.url = e.srv.Start()
		t.Cleanup(e.srv.Close)
	}
	cfg := connector.DeviceConfig{
		DeviceID: "01DEVICE", Kind: domain.ConnectorRouterOSREST, BaseURL: e.url,
		Username: tUser, Password: tPass, TLSMode: domain.TLSVerify,
	}
	if s.cfg != nil {
		s.cfg(&cfg)
	}
	opts := Options{
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Clock:  e.clk,
	}
	if s.opts != nil {
		s.opts(&opts)
	}
	c, err := NewConn(t.Context(), cfg, opts)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	e.conn = c
	return e
}

var lte1 = connector.ChannelRef{ChannelID: "01CHANNEL1", Port: "lte1"}
var lte2 = connector.ChannelRef{ChannelID: "01CHANNEL2", Port: "lte2"}

func sms(to, body string) connector.OutboundSMS {
	return connector.OutboundSMS{MessageID: "01MSG", To: to, Body: body, Encoding: domain.EncodingGSM7, Segments: 1}
}

// wantErr checks the class, ambiguity and code of a connector error.
func wantErr(t *testing.T, err error, kind domain.ErrorKind, ambiguous bool, code domain.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %s", kind)
	}
	var ce *domain.ConnectorError
	if !errors.As(err, &ce) {
		t.Fatalf("err %v (%T) is not a ConnectorError", err, err)
	}
	if got := domain.KindOf(err); got != kind {
		t.Errorf("kind = %s, want %s (%v)", got, kind, err)
	}
	if got := domain.IsAmbiguous(err); got != ambiguous {
		t.Errorf("ambiguous = %v, want %v (%v)", got, ambiguous, err)
	}
	if code != "*" {
		if got := domain.CodeOf(err); got != code {
			t.Errorf("code = %q, want %q (%v)", got, code, err)
		}
	}
}

// closedURL returns an http URL on which nothing listens.
func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}
