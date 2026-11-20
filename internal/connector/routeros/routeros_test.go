// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
)

func TestNewConnValidation(t *testing.T) {
	ok := connector.DeviceConfig{DeviceID: "D1", BaseURL: "https://10.0.0.1", TLSMode: domain.TLSVerify}
	tests := []struct {
		name string
		edit func(*connector.DeviceConfig)
		base string // expected REST root, "" = error
	}{
		{"https", nil, "https://10.0.0.1/rest"},
		{"trailing slash", func(c *connector.DeviceConfig) { c.BaseURL = "https://router.lan:8443/" }, "https://router.lan:8443/rest"},
		{"already /rest", func(c *connector.DeviceConfig) { c.BaseURL = "HTTPS://10.0.0.1/rest/" }, "https://10.0.0.1/rest"},
		{"plain http", func(c *connector.DeviceConfig) { c.BaseURL = "http://172.17.0.1" }, "http://172.17.0.1/rest"},
		{"empty mode is verify", func(c *connector.DeviceConfig) { c.TLSMode = "" }, "https://10.0.0.1/rest"},
		{"bad scheme", func(c *connector.DeviceConfig) { c.BaseURL = "ftp://10.0.0.1" }, ""},
		{"no host", func(c *connector.DeviceConfig) { c.BaseURL = "https:///rest" }, ""},
		{"credentials in url", func(c *connector.DeviceConfig) { c.BaseURL = "https://u:p@10.0.0.1" }, ""},
		{"query", func(c *connector.DeviceConfig) { c.BaseURL = "https://10.0.0.1?x=1" }, ""},
		{"unparsable", func(c *connector.DeviceConfig) { c.BaseURL = "https://[::1" }, ""},
		{"bad tls mode", func(c *connector.DeviceConfig) { c.TLSMode = "pinned" }, ""},
		{"ca without pem", func(c *connector.DeviceConfig) { c.TLSMode = domain.TLSCustomCA }, ""},
		{"bad fingerprint", func(c *connector.DeviceConfig) { c.TLSMode, c.Fingerprint = domain.TLSFingerprint, "abcd" }, ""},
		{"fingerprint not hex", func(c *connector.DeviceConfig) {
			c.TLSMode, c.Fingerprint = domain.TLSFingerprint, strings.Repeat("zz", 32)
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ok
			if tt.edit != nil {
				tt.edit(&cfg)
			}
			c, err := NewConn(t.Context(), cfg, Options{})
			if tt.base == "" {
				if err == nil {
					t.Fatalf("NewConn(%+v) succeeded", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewConn: %v", err)
			}
			defer func() { _ = c.Close() }()
			if c.base != tt.base {
				t.Errorf("base = %q, want %q", c.base, tt.base)
			}
		})
	}
}

func TestRegistryAndDefaults(t *testing.T) {
	reg := connector.NewRegistry()
	quiet := Options{Logger: slog.New(slog.DiscardHandler)}
	if err := Register(reg, quiet); err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, quiet); err == nil {
		t.Fatal("double registration accepted")
	}
	c, err := reg.New(t.Context(), connector.DeviceConfig{DeviceID: "D1", BaseURL: "https://10.0.0.1", Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	conn := c.(*Conn)
	if conn.requestTimeout != 3*time.Second {
		t.Errorf("request timeout = %v", conn.requestTimeout)
	}
	caps := c.Capabilities()
	want := connector.Capabilities{
		Encodings: []domain.Encoding{domain.EncodingGSM7}, MaxSegments: 1, MaxConcurrentSends: 1,
		SendTimeout: 65 * time.Second, InboundPDU: true,
	}
	if fmt.Sprint(caps) != fmt.Sprint(want) {
		t.Errorf("Capabilities = %+v, want %+v", caps, want)
	}
	caps.Encodings[0] = domain.EncodingUCS2
	if c.Capabilities().Encodings[0] != domain.EncodingGSM7 {
		t.Error("Capabilities shares its slice")
	}
	if c.(*Conn).InsecureTransport() {
		t.Error("https verify flagged insecure")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := New(t.Context(), connector.DeviceConfig{BaseURL: "https://127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
}

func TestClosed(t *testing.T) {
	e := newEnv(t, setup{})
	_ = e.conn.Close()
	_, err := e.conn.FetchInbox(t.Context(), lte1)
	wantErr(t, err, domain.KindTransient, false, "")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v", err)
	}
}

// TestLogsHideSecrets: no message body, full number or password in logs,
// whatever happens.
func TestLogsHideSecrets(t *testing.T) {
	const (
		body   = "Top secret OTP 917263"
		number = "+48601234567"
	)
	e := newEnv(t, setup{})
	ctx := t.Context()
	if _, err := e.conn.Send(ctx, lte1, sms(number, body)); err != nil {
		t.Fatal(err)
	}
	for _, f := range []fake.Fault{
		fake.HTTPError(400, "invalid phone-number "+number),
		fake.SessionClosed(), fake.DropConnection(), fake.Unauthorized(),
	} {
		e.srv.FailNext(fake.EndpointSend, f, 1)
		_, err := e.conn.Send(ctx, lte1, sms(number, body))
		if err == nil {
			t.Fatal("fault ignored")
		}
		if strings.Contains(err.Error(), number) || strings.Contains(err.Error(), body) {
			t.Errorf("error leaks secrets: %v", err)
		}
	}
	_, _ = e.conn.Send(ctx, lte1, connector.OutboundSMS{To: number, Body: body + " ąę", Encoding: domain.EncodingGSM7})
	if _, err := e.srv.InjectSMS(fake.IncomingSMS{From: number, Text: body}); err != nil {
		t.Fatal(err)
	}
	got, err := e.conn.FetchInbox(ctx, lte1)
	if err != nil || len(got) != 1 {
		t.Fatalf("FetchInbox = %v, %v", got, err)
	}
	if err := e.conn.DeleteInbox(ctx, lte1, []string{got[0].RemoteID}); err != nil {
		t.Fatal(err)
	}
	_, _ = e.conn.Health(ctx, lte1)
	_, _ = e.conn.Probe(ctx)

	logs := e.logs.String()
	for _, secret := range []string{body, "917263", number, "601234567", tPass} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, "+48*****4567") {
		t.Errorf("masked number missing from logs:\n%s", logs)
	}
	if !strings.Contains(logs, "plain HTTP") {
		t.Error("plain HTTP not warned about")
	}
}

func TestSanitizeDetail(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct{ in, want string }{
		{"invalid phone-number +48601234567", "invalid phone-number +48*****4567"},
		{"number 601 234 567 rejected", "number *******67 rejected"},
		{"no such item (4)", "no such item (4)"},
		{long, strings.Repeat("x", maxDetail) + "…"},
		{"bad\xffutf8", "bad?utf8"},
	}
	for _, tt := range tests {
		if got := sanitizeDetail(tt.in); got != tt.want {
			t.Errorf("sanitizeDetail(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := parseRouterError([]byte(`{"error":400,"detail":"only detail"}`)); got != "only detail" {
		t.Errorf("parseRouterError = %q", got)
	}
	if got := parseRouterError([]byte(`<html>oops</html>`)); got != "<html>oops</html>" {
		t.Errorf("parseRouterError = %q", got)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestTransportCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want domain.ErrorCode
	}{
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), domain.CodeTimeout},
		{"net timeout", &net.OpError{Op: "dial", Err: timeoutErr{}}, domain.CodeTimeout},
		{"canceled", context.Canceled, ""},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, domain.CodeUnreachable},
		{"dns", &net.DNSError{Err: "no such host", Name: "router.invalid"}, domain.CodeUnreachable},
		{"unknown authority", x509.UnknownAuthorityError{}, domain.CodeTLSError},
		{"hostname", x509.HostnameError{Certificate: &x509.Certificate{}, Host: "x"}, domain.CodeTLSError},
		{"alert", tls.AlertError(42), domain.CodeTLSError},
		{"remote tls error", errors.New("remote error: tls: handshake failure"), domain.CodeTLSError},
		{"http to https", errors.New("http: server gave HTTP response to HTTPS client"), domain.CodeTLSError},
		{"pin", fmt.Errorf("x: %w", ErrFingerprintMismatch), domain.CodeTLSError},
		{"too large", ErrResponseTooLarge, domain.CodeUnexpectedResponse},
	}
	for _, tt := range tests {
		if got := transportCode(tt.err); got != tt.want {
			t.Errorf("%s: transportCode = %q, want %q", tt.name, got, tt.want)
		}
	}
	// TLS failures are never ambiguous, even for a send.
	if domain.IsAmbiguous(classifyTransport(opSend, true, true, x509.UnknownAuthorityError{})) {
		t.Error("tls error marked ambiguous")
	}
	if !domain.IsAmbiguous(classifyTransport(opSend, true, true, errors.New("EOF"))) {
		t.Error("EOF after write not ambiguous")
	}
	if domain.IsAmbiguous(classifyTransport(opFetchInbox, false, true, errors.New("EOF"))) {
		t.Error("read-only call marked ambiguous")
	}
}

func TestRecordValues(t *testing.T) {
	r := record{"s": "x", "f": 1.5, "t": true, "n": false, "y": "yes", "o": map[string]any{}}
	if r.str("s") != "x" || r.str("f") != "1.5" || r.str("t") != "true" || r.str("n") != "false" || r.str("o") != "" || r.str("missing") != "" {
		t.Errorf("str: %v", r)
	}
	if !r.boolean("t") || !r.boolean("y") || r.boolean("n") || !r.has("o") || r.has("missing") {
		t.Errorf("boolean/has: %v", r)
	}
}

func TestFingerprintFormat(t *testing.T) {
	b, err := ParseFingerprint("ab:CD" + strings.Repeat(" 00", 30))
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatFingerprint(b); !strings.HasPrefix(got, "AB:CD:00:") || len(got) != 95 {
		t.Errorf("FormatFingerprint = %q", got)
	}
	if err := (&Conn{pin: b}).verifyPin(nil, nil); !errors.Is(err, ErrFingerprintMismatch) {
		t.Errorf("verifyPin(no certs) = %v", err)
	}
}
