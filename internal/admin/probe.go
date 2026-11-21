// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package admin

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

const (
	maxProbeDetail  = 300
	minScrubbedPass = 3
)

// TestTLS is what the test learned about the TLS session.
type TestTLS struct {
	Mode            string
	Verified        bool
	PeerFingerprint string // "AB:CD:..."; may be set even when the test failed
}

// TestPort is a modem port found by the test.
type TestPort struct {
	Port         string
	Running      bool
	Operator     string
	Registration string
	Signal       *domain.Signal
	ChannelID    domain.ID // the channel configured for the port, "" if none
}

// TestError is the reason a test failed.
type TestError struct {
	Code   domain.ErrorCode
	Detail string
}

// TestResult is the outcome of [Service.TestDevice].
type TestResult struct {
	OK              bool
	CheckedAt       time.Time
	Latency         *time.Duration
	RouterOSVersion string
	BoardName       string
	TLS             *TestTLS
	Ports           []TestPort
	Error           *TestError
}

// probeCodes are the error codes a failed test may carry.
var probeCodes = map[domain.ErrorCode]bool{
	domain.CodeUnreachable: true, domain.CodeTimeout: true, domain.CodeTLSError: true, domain.CodeAuthFailed: true,
	domain.CodeInsufficientPolicy: true, domain.CodeUnsupportedVersion: true, domain.CodeUnexpectedResponse: true,
}

// TestDevice probes the stored device: connectivity, TLS, credentials and the
// modem ports. A failed test is a result with Error set, not an error; the
// device and its channels are not changed. An unknown ID yields
// domain.ErrNotFound.
func (s *Service) TestDevice(ctx context.Context, id domain.ID) (TestResult, error) {
	d, err := s.GetDevice(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	chans, err := s.st.Channels().List(ctx, store.ChannelFilter{DeviceID: d.ID})
	if err != nil {
		return TestResult{}, wrap("list channels of device", err)
	}
	res := TestResult{CheckedAt: s.clk.Now()}
	fail := func(code domain.ErrorCode, detail string) (TestResult, error) {
		res.Error = &TestError{Code: code, Detail: truncate(detail, maxProbeDetail)}
		return res, nil
	}

	cfg, err := s.creds.ConnectorConfig(d)
	if err != nil {
		s.log.Warn("device password cannot be decrypted", slog.String("device_id", string(d.ID)), slog.Any("error", err))
		return fail(domain.CodeUnexpectedResponse, "The stored password cannot be decrypted; was the secret key changed?")
	}
	cfg.Timeout = s.opts.ProbeTimeout
	conn, err := s.conns.New(ctx, cfg)
	if err != nil {
		return fail(domain.CodeUnexpectedResponse, scrub("Connector cannot be created: "+err.Error(), cfg.Password))
	}
	defer func() { _ = conn.Close() }()

	pctx, cancel := context.WithTimeout(ctx, s.opts.ProbeTimeout)
	defer cancel()
	start := s.clk.Now()
	probe, perr := conn.Probe(pctx)
	latency := s.clk.Now().Sub(start)

	res.OK = perr == nil
	res.RouterOSVersion, res.BoardName = probe.RouterOSVersion, probe.BoardName
	if probe.TLS != nil {
		res.TLS = &TestTLS{Mode: string(probe.TLS.Mode), Verified: probe.TLS.Verified,
			PeerFingerprint: FormatFingerprint(probe.TLS.PeerFingerprint)}
	}
	for _, pi := range probe.Ports {
		pd := TestPort{Port: pi.Port, Running: pi.Health.Running, Operator: pi.Health.Operator,
			Registration: pi.Health.Registration, Signal: pi.Health.Signal}
		for _, c := range chans {
			if c.Port == pi.Port {
				pd.ChannelID = c.ID
				break
			}
		}
		res.Ports = append(res.Ports, pd)
	}
	if perr == nil {
		res.Latency = &latency
		return res, nil
	}
	code := probeErrorCode(pctx, perr)
	if code != domain.CodeUnreachable && code != domain.CodeTimeout {
		res.Latency = &latency
	}
	return fail(code, scrub(perr.Error(), cfg.Password))
}

func probeErrorCode(ctx context.Context, err error) domain.ErrorCode {
	if c := domain.CodeOf(err); probeCodes[c] {
		return c
	}
	switch {
	case domain.KindOf(err) == domain.KindAuth:
		return domain.CodeAuthFailed
	case ctx.Err() != nil:
		return domain.CodeTimeout
	}
	return domain.CodeUnexpectedResponse
}

// scrub removes secret from s; connectors do not put passwords into errors,
// this is the last line of defence.
func scrub(s, secret string) string {
	if len(secret) >= minScrubbedPass {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
