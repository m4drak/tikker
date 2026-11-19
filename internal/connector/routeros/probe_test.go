// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
)

func TestProbeOK(t *testing.T) {
	e := newEnv(t, setup{tls: true, cfg: func(c *connector.DeviceConfig) { c.TLSMode = domain.TLSInsecure }})
	e.srv.SetLTE("lte2", func(s *fake.LTEState) { s.Running = false })
	p, err := e.conn.Probe(t.Context())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.RouterOSVersion != "7.16.2 (stable)" || p.BoardName != "FakeBoard LTE" {
		t.Errorf("probe = %+v", p)
	}
	if p.TLS == nil || p.TLS.Mode != domain.TLSInsecure || p.TLS.Verified || p.TLS.PeerFingerprint != e.srv.Fingerprint() {
		t.Errorf("tls = %+v, want fingerprint %s", p.TLS, e.srv.Fingerprint())
	}
	if len(p.Ports) != 2 || p.Ports[0].Port != "lte1" || !p.Ports[0].Health.Registered || p.Ports[1].Health.Running {
		t.Errorf("ports = %+v", p.Ports)
	}
}

func TestProbePlainHTTP(t *testing.T) {
	e := newEnv(t, setup{})
	p, err := e.conn.Probe(t.Context())
	if err != nil || p.TLS != nil {
		t.Fatalf("Probe = %+v, %v", p, err)
	}
	if !e.conn.InsecureTransport() {
		t.Error("http:// not flagged as insecure transport")
	}
}

func TestProbeTolerates(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*env)
		check   func(*testing.T, connector.DeviceProbe)
	}{
		{"no routerboard (CHR)", func(e *env) {
			e.srv.FailNext(fake.EndpointRouterboard, fake.HTTPError(400, "no such command or directory"), 1)
		},
			func(t *testing.T, p connector.DeviceProbe) {
				if p.BoardName != "FakeBoard LTE" {
					t.Errorf("board = %q", p.BoardName)
				}
			}},
		{"one port monitor fails", func(e *env) { e.srv.FailNext(fake.EndpointLTEMonitor, fake.HTTPError(500, ""), 1) },
			func(t *testing.T, p connector.DeviceProbe) {
				if len(p.Ports) != 2 || p.Ports[0].Health.Registered || !p.Ports[1].Health.Registered {
					t.Errorf("ports = %+v", p.Ports)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, setup{})
			tt.prepare(e)
			p, err := e.conn.Probe(t.Context())
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			tt.check(t, p)
		})
	}
}

// TestProbeErrorCodes covers every DeviceTestResult error code.
func TestProbeErrorCodes(t *testing.T) {
	tests := []struct {
		name    string
		s       setup
		prepare func(*env)
		ctxTO   time.Duration
		code    domain.ErrorCode
		check   func(*testing.T, *env, connector.DeviceProbe)
	}{
		{name: "unreachable", s: setup{noRun: true, cfg: func(c *connector.DeviceConfig) { c.BaseURL = closedURL(t) }}, code: domain.CodeUnreachable},
		{name: "timeout", prepare: func(e *env) { e.srv.FailNext(fake.EndpointResource, fake.Hang(), 1) }, ctxTO: 150 * time.Millisecond, code: domain.CodeTimeout},
		{name: "timeout in monitor", prepare: func(e *env) { e.srv.FailNext(fake.EndpointLTEMonitor, fake.Hang(), 1) }, ctxTO: 300 * time.Millisecond, code: domain.CodeTimeout},
		{name: "tls fingerprint mismatch", s: setup{tls: true, cfg: func(c *connector.DeviceConfig) {
			c.TLSMode, c.Fingerprint = domain.TLSFingerprint, strings.Repeat("00", 32)
		}}, code: domain.CodeTLSError, check: func(t *testing.T, e *env, p connector.DeviceProbe) {
			if p.TLS == nil || p.TLS.Verified || p.TLS.PeerFingerprint != e.srv.Fingerprint() {
				t.Errorf("tls = %+v, want peer fingerprint %s", p.TLS, e.srv.Fingerprint())
			}
		}},
		{name: "tls unknown authority", s: setup{tls: true}, code: domain.CodeTLSError, check: func(t *testing.T, e *env, p connector.DeviceProbe) {
			if p.TLS == nil || p.TLS.Mode != domain.TLSVerify || p.TLS.PeerFingerprint != e.srv.Fingerprint() {
				t.Errorf("tls = %+v", p.TLS)
			}
		}},
		{name: "auth failed", s: setup{cfg: func(c *connector.DeviceConfig) { c.Password = "wrong" }}, code: domain.CodeAuthFailed},
		{name: "no rest-api policy", prepare: func(e *env) { e.srv.SetPolicies(fake.Policies{API: true}) }, code: domain.CodeAuthFailed},
		{name: "no api policy", prepare: func(e *env) { e.srv.SetPolicies(fake.Policies{RESTAPI: true}) }, code: domain.CodeInsufficientPolicy},
		{name: "no permission for sms", prepare: func(e *env) {
			e.srv.FailNext(fake.EndpointSettings, fake.HTTPError(500, "not enough permissions (9)"), 1)
		}, code: domain.CodeInsufficientPolicy},
		{name: "routerboard auth", prepare: func(e *env) { e.srv.FailNext(fake.EndpointRouterboard, fake.Unauthorized(), 1) }, code: domain.CodeAuthFailed},
		{name: "monitor auth", prepare: func(e *env) { e.srv.FailNext(fake.EndpointLTEMonitor, fake.NotAllowed(), 1) }, code: domain.CodeInsufficientPolicy},
		{name: "old version", s: setup{fake: fake.Options{Version: "7.12.1 (stable)"}}, code: domain.CodeUnsupportedVersion,
			check: func(t *testing.T, _ *env, p connector.DeviceProbe) {
				if p.RouterOSVersion != "7.12.1 (stable)" {
					t.Errorf("version = %q", p.RouterOSVersion)
				}
			}},
		{name: "routeros 6", s: setup{fake: fake.Options{Version: "6.49.10 (long-term)"}}, code: domain.CodeUnsupportedVersion},
		{name: "no sms package", prepare: func(e *env) {
			e.srv.FailNext(fake.EndpointSettings, fake.HTTPError(400, "no such command or directory (sms)"), 1)
		}, code: domain.CodeUnsupportedVersion},
		{name: "garbage version", s: setup{fake: fake.Options{Version: "unknown"}}, code: domain.CodeUnexpectedResponse},
		{name: "resource not json", prepare: func(e *env) { e.srv.FailNext(fake.EndpointResource, fake.HTTPError(200, ""), 1) }, code: domain.CodeUnexpectedResponse},
		{name: "lte list fails", prepare: func(e *env) { e.srv.FailNext(fake.EndpointLTE, fake.HTTPError(404, ""), 1) }, code: domain.CodeUnexpectedResponse},
		{name: "server error", prepare: func(e *env) { e.srv.FailNext(fake.EndpointResource, fake.HTTPError(500, ""), 1) }, code: domain.CodeUnexpectedResponse},
		{name: "cancelled", ctxTO: -1, code: domain.CodeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.s)
			if tt.prepare != nil {
				tt.prepare(e)
			}
			ctx := t.Context()
			var cancel context.CancelFunc
			switch {
			case tt.ctxTO > 0:
				ctx, cancel = context.WithTimeout(ctx, tt.ctxTO)
				defer cancel()
			case tt.ctxTO < 0:
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			p, err := e.conn.Probe(ctx)
			if err == nil {
				t.Fatalf("Probe succeeded: %+v", p)
			}
			if got := domain.CodeOf(err); got != tt.code {
				t.Errorf("code = %q, want %q (%v)", got, tt.code, err)
			}
			if domain.IsAmbiguous(err) {
				t.Errorf("probe error is ambiguous: %v", err)
			}
			if tt.check != nil {
				tt.check(t, e, p)
			}
		})
	}
}

func TestTLSModes(t *testing.T) {
	cert, fp, err := fake.SelfSignedCert("router.example")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))
	otherCert, _, err := fake.SelfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	otherLeaf, _ := x509.ParseCertificate(otherCert.Certificate[0])
	otherPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: otherLeaf.Raw}))
	roots := x509.NewCertPool()
	roots.AddCert(leaf)

	tests := []struct {
		name     string
		mode     domain.TLSMode
		ca       string
		fp       string
		roots    bool
		wantErr  bool
		verified bool
	}{
		{name: "fingerprint colon upper", mode: domain.TLSFingerprint, fp: fp, verified: true},
		{name: "fingerprint plain lower", mode: domain.TLSFingerprint, fp: strings.ToLower(strings.ReplaceAll(fp, ":", "")), verified: true},
		{name: "fingerprint wrong", mode: domain.TLSFingerprint, fp: strings.Repeat("11", 32), wantErr: true},
		{name: "custom ca", mode: domain.TLSCustomCA, ca: caPEM, verified: true},
		{name: "custom ca wrong", mode: domain.TLSCustomCA, ca: otherPEM, wantErr: true},
		{name: "verify system roots", mode: domain.TLSVerify, wantErr: true},
		{name: "verify injected roots", mode: domain.TLSVerify, roots: true, verified: true},
		{name: "insecure", mode: domain.TLSInsecure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, setup{
				cert: &cert,
				cfg: func(c *connector.DeviceConfig) {
					c.TLSMode, c.CAPEM, c.Fingerprint = tt.mode, tt.ca, tt.fp
				},
				opts: func(o *Options) {
					if tt.roots {
						o.rootCAs = roots
					}
				},
			})
			p, err := e.conn.Probe(t.Context())
			if tt.wantErr {
				if domain.CodeOf(err) != domain.CodeTLSError {
					t.Fatalf("err = %v, want tls_error", err)
				}
			} else if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if p.TLS == nil || p.TLS.PeerFingerprint != fp || p.TLS.Verified != tt.verified || p.TLS.Mode != tt.mode {
				t.Errorf("tls = %+v, want fingerprint %s verified %v", p.TLS, fp, tt.verified)
			}
			if got := e.conn.InsecureTransport(); got != (tt.mode == domain.TLSInsecure) {
				t.Errorf("InsecureTransport = %v", got)
			}
			if tt.mode == domain.TLSInsecure && !strings.Contains(e.logs.String(), "verification is disabled") {
				t.Error("insecure mode not warned about")
			}
		})
	}
}
