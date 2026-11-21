// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
)

func TestSendOK(t *testing.T) {
	e := newEnv(t, setup{})
	res, err := e.conn.Send(t.Context(), lte2, sms("+48601234567", "Hello {world} €"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !res.AcceptedAt.Equal(tNow) {
		t.Errorf("AcceptedAt = %v, want fake clock %v", res.AcceptedAt, tNow)
	}
	sent := e.srv.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d", len(sent))
	}
	got := sent[0]
	if got.Port != "lte2" || got.PhoneNumber != "+48601234567" || got.Message != "Hello {world} €" || got.Ambiguous {
		t.Errorf("sent = %+v", got)
	}
}

func TestSendValidation(t *testing.T) {
	long := strings.Repeat("a", 161)
	tests := []struct {
		name string
		ch   connector.ChannelRef
		msg  connector.OutboundSMS
		want error
	}{
		{"ucs2 encoding", lte1, connector.OutboundSMS{To: "+48601234567", Body: "x", Encoding: domain.EncodingUCS2, Segments: 1}, ErrUnsupportedEncoding},
		{"two segments", lte1, connector.OutboundSMS{To: "+48601234567", Body: "x", Encoding: domain.EncodingGSM7, Segments: 2}, ErrTooLong},
		{"161 septets", lte1, sms("+48601234567", long), ErrTooLong},
		{"81 extension chars", lte1, sms("+48601234567", strings.Repeat("€", 81)), ErrTooLong},
		{"non gsm7 char", lte1, sms("+48601234567", "zażółć"), ErrUnsupportedEncoding},
		{"emoji", lte1, sms("+48601234567", "hi 🙂"), ErrUnsupportedEncoding},
		{"no recipient", lte1, sms("  ", "x"), ErrInvalidMessage},
		{"no port", connector.ChannelRef{}, sms("+48601234567", "x"), ErrInvalidMessage},
	}
	e := newEnv(t, setup{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.conn.Send(t.Context(), tt.ch, tt.msg)
			wantErr(t, err, domain.KindPermanent, false, "")
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if n := e.srv.Requests(fake.EndpointAny); n != 0 {
		t.Fatalf("router was called %d times for invalid messages", n)
	}

	// Limits that still fit: 160 septets, 80 extension chars, defaults.
	for _, body := range []string{strings.Repeat("a", 160), strings.Repeat("€", 80)} {
		if _, err := e.conn.Send(t.Context(), lte1, sms("+48601234567", body)); err != nil {
			t.Errorf("Send(%d runes): %v", len([]rune(body)), err)
		}
	}
	if _, err := e.conn.Send(t.Context(), lte1, connector.OutboundSMS{To: "+48601234567", Body: "defaults"}); err != nil {
		t.Errorf("Send with zero encoding/segments: %v", err)
	}
}

// TestSendErrorClassification covers every row of the classification table
// (README.md) against the fake's faults.
func TestSendErrorClassification(t *testing.T) {
	type tc struct {
		name      string
		prepare   func(e *env)
		opts      func(*Options)
		ctxTO     time.Duration
		to        string
		port      string
		kind      domain.ErrorKind
		ambiguous bool
		code      domain.ErrorCode
		// accepted: the fake recorded the SMS as sent despite the error.
		accepted bool
	}
	fail := func(f fake.Fault) func(*env) {
		return func(e *env) { e.srv.FailNext(fake.EndpointSend, f, 1) }
	}
	tests := []tc{
		{name: "401 unauthorized", prepare: fail(fake.Unauthorized()), kind: domain.KindAuth, code: domain.CodeAuthFailed},
		{name: "403 forbidden", prepare: fail(fake.HTTPError(403, "")), kind: domain.KindAuth, code: domain.CodeInsufficientPolicy},
		{name: "500 not allowed", prepare: fail(fake.NotAllowed()), kind: domain.KindAuth, code: domain.CodeInsufficientPolicy},
		{name: "500 not enough permissions", prepare: fail(fake.HTTPError(500, "not enough permissions (9)")), kind: domain.KindAuth, code: domain.CodeInsufficientPolicy},
		{name: "400 session closed", prepare: fail(fake.SessionClosed()), kind: domain.KindTransient, ambiguous: true, code: domain.CodeTimeout},
		{name: "400 session closed after accept", prepare: fail(fake.SessionClosed().AfterAccept()), kind: domain.KindTransient, ambiguous: true, code: domain.CodeTimeout, accepted: true},
		{name: "session limit hit by slow modem", prepare: func(e *env) {
			e.srv.SetSessionTimeout(50 * time.Millisecond)
			e.srv.SetSendDelay(100 * time.Millisecond)
		}, kind: domain.KindTransient, ambiguous: true, code: domain.CodeTimeout, accepted: true},
		{name: "503 busy", prepare: fail(fake.HTTPError(503, "busy")), kind: domain.KindTransient},
		{name: "500 without detail", prepare: fail(fake.HTTPError(500, "")), kind: domain.KindTransient},
		{name: "504 gateway timeout", prepare: fail(fake.HTTPError(504, "")), kind: domain.KindTransient, ambiguous: true, code: domain.CodeTimeout},
		{name: "500 not registered (fake modem state)", prepare: func(e *env) {
			e.srv.SetLTE("lte1", func(s *fake.LTEState) { s.Registration = "searching"; s.Signal = nil })
		}, kind: domain.KindTransient},
		{name: "400 not registered", prepare: fail(fake.HTTPError(400, "failure: not registered to network")), kind: domain.KindTransient},
		{name: "400 modem busy", prepare: fail(fake.HTTPError(400, "failure: modem is busy")), kind: domain.KindTransient},
		{name: "400 invalid phone (fake validation)", to: "48-601", kind: domain.KindPermanent},
		{name: "400 invalid phone", prepare: fail(fake.HTTPError(400, "invalid phone-number")), kind: domain.KindPermanent},
		{name: "400 body too long", prepare: fail(fake.HTTPError(400, "failure: Body too long")), kind: domain.KindPermanent},
		{name: "400 outside gsm7", prepare: fail(fake.HTTPError(400, "failure: message contains characters outside the GSM 7-bit alphabet")), kind: domain.KindPermanent},
		{name: "400 unknown port (channel misconfigured)", port: "lte9", kind: domain.KindTransient, code: domain.CodeUnexpectedResponse},
		{name: "400 no such command", prepare: fail(fake.HTTPError(400, "no such command or directory (sms)")), kind: domain.KindTransient, code: domain.CodeUnsupportedVersion},
		{name: "400 unknown detail", prepare: fail(fake.HTTPError(400, "something new")), kind: domain.KindTransient, code: domain.CodeUnexpectedResponse},
		{name: "415 unknown 4xx", prepare: fail(fake.HTTPError(415, "")), kind: domain.KindTransient, code: domain.CodeUnexpectedResponse},
		{name: "302 redirect not followed", prepare: fail(fake.HTTPError(302, "")), kind: domain.KindTransient, code: domain.CodeUnexpectedResponse},
		{name: "connection dropped after request", prepare: fail(fake.DropConnection()), kind: domain.KindTransient, ambiguous: true, code: domain.CodeUnreachable},
		{name: "connection dropped after accept", prepare: fail(fake.DropConnection().AfterAccept()), kind: domain.KindTransient, ambiguous: true, code: domain.CodeUnreachable, accepted: true},
		{name: "hang until caller deadline", prepare: fail(fake.Hang()), ctxTO: 150 * time.Millisecond, kind: domain.KindTransient, ambiguous: true, code: domain.CodeTimeout},
		{name: "hang after accept until send timeout", prepare: fail(fake.Hang().AfterAccept()),
			opts: func(o *Options) { o.SendTimeout = 150 * time.Millisecond }, kind: domain.KindTransient, ambiguous: true, code: domain.CodeTimeout, accepted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, setup{opts: tt.opts})
			if tt.prepare != nil {
				tt.prepare(e)
			}
			ctx := t.Context()
			if tt.ctxTO > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.ctxTO)
				defer cancel()
			}
			to := tt.to
			if to == "" {
				to = "+48601234567"
			}
			ch := lte1
			if tt.port != "" {
				ch.Port = tt.port
			}
			_, err := e.conn.Send(ctx, ch, sms(to, "classified"))
			wantErr(t, err, tt.kind, tt.ambiguous, tt.code)
			if tt.accepted {
				if !e.srv.WaitSent(1, 2*time.Second) || !e.srv.Sent()[0].Ambiguous {
					t.Errorf("fake did not record an ambiguous send: %+v", e.srv.Sent())
				}
			}
			if strings.Contains(err.Error(), "classified") || strings.Contains(err.Error(), "601234567") {
				t.Errorf("error leaks body or number: %v", err)
			}
		})
	}
}

func TestSendTransportBeforeWrite(t *testing.T) {
	selfSigned, _, err := fake.SelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		s    setup
		code domain.ErrorCode
	}{
		{"connection refused", setup{noRun: true, cfg: func(c *connector.DeviceConfig) { c.BaseURL = closedURL(t) }}, domain.CodeUnreachable},
		{"dns failure", setup{noRun: true, cfg: func(c *connector.DeviceConfig) { c.BaseURL = "http://router.invalid" }}, domain.CodeUnreachable},
		{"fingerprint mismatch", setup{tls: true, cfg: func(c *connector.DeviceConfig) {
			c.TLSMode, c.Fingerprint = domain.TLSFingerprint, strings.Repeat("ab", 32)
		}}, domain.CodeTLSError},
		{"unknown authority", setup{cert: &selfSigned}, domain.CodeTLSError},
		{"tls to plain http", setup{cfg: func(c *connector.DeviceConfig) {
			c.BaseURL = strings.Replace(c.BaseURL, "http://", "https://", 1)
		}}, domain.CodeTLSError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.s)
			_, err := e.conn.Send(t.Context(), lte1, sms("+48601234567", "x"))
			wantErr(t, err, domain.KindTransient, false, tt.code)
			if n := len(e.srv.Sent()); n != 0 {
				t.Fatalf("sent %d", n)
			}
		})
	}
}

// TestSendSerializedPerPort: two sends on one port never overlap at the
// router; sends on different ports do.
func TestSendSerializedPerPort(t *testing.T) {
	const delay = 150 * time.Millisecond
	run := func(t *testing.T, a, b connector.ChannelRef) []fake.SentMessage {
		e := newEnv(t, setup{fake: fake.Options{SendDelay: delay}})
		var wg sync.WaitGroup
		for _, ch := range []connector.ChannelRef{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := e.conn.Send(t.Context(), ch, sms("+48601234567", "x")); err != nil {
					t.Errorf("Send: %v", err)
				}
			}()
		}
		wg.Wait()
		sent := e.srv.Sent()
		if len(sent) != 2 {
			t.Fatalf("sent = %d", len(sent))
		}
		sort.Slice(sent, func(i, j int) bool { return sent[i].ReceivedAt.Before(sent[j].ReceivedAt) })
		return sent
	}
	t.Run("same port", func(t *testing.T) {
		s := run(t, lte1, lte1)
		if s[1].ReceivedAt.Before(s[0].CompletedAt) {
			t.Fatalf("second send reached the router at %v, before the first completed at %v",
				s[1].ReceivedAt, s[0].CompletedAt)
		}
	})
	t.Run("different ports", func(t *testing.T) {
		s := run(t, lte1, lte2)
		if !s[1].ReceivedAt.Before(s[0].CompletedAt) {
			t.Fatalf("sends on different ports were serialized: %v >= %v", s[1].ReceivedAt, s[0].CompletedAt)
		}
	})
}

func TestSendContextCancellation(t *testing.T) {
	t.Run("waiting for port is not ambiguous", func(t *testing.T) {
		e := newEnv(t, setup{fake: fake.Options{SendDelay: 400 * time.Millisecond}})
		done := make(chan error, 1)
		go func() {
			_, err := e.conn.Send(context.Background(), lte1, sms("+48601234567", "first"))
			done <- err
		}()
		if !waitFor(func() bool { return e.srv.Requests(fake.EndpointSend) == 1 }) {
			t.Fatal("first send never reached the router")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err := e.conn.Send(ctx, lte1, sms("+48601234567", "second"))
		wantErr(t, err, domain.KindTransient, false, domain.CodeTimeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want deadline", err)
		}
		if n := e.srv.Requests(fake.EndpointSend); n != 1 {
			t.Errorf("router saw %d sends, want 1", n)
		}
		if err := <-done; err != nil {
			t.Fatalf("first send: %v", err)
		}
	})
	t.Run("cancel during request is ambiguous", func(t *testing.T) {
		e := newEnv(t, setup{fake: fake.Options{SendDelay: time.Second}})
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			waitFor(func() bool { return e.srv.Requests(fake.EndpointSend) == 1 })
			cancel()
		}()
		_, err := e.conn.Send(ctx, lte1, sms("+48601234567", "x"))
		wantErr(t, err, domain.KindTransient, true, "")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want canceled", err)
		}
	})
	t.Run("already cancelled", func(t *testing.T) {
		e := newEnv(t, setup{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := e.conn.Send(ctx, lte1, sms("+48601234567", "x"))
		wantErr(t, err, domain.KindTransient, false, "*")
		if n := e.srv.Requests(fake.EndpointAny); n != 0 {
			t.Errorf("router called %d times", n)
		}
	})
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}
