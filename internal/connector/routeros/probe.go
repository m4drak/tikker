// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

// Probe implements connector.Connector (POST /devices/{id}/test). Every
// error it returns carries a domain.ErrorCode from the DeviceTestResult enum
// of api/openapi.yaml; the DeviceProbe holds what was learned before the
// failure (after a TLS failure: the certificate fingerprint to pin).
func (c *Conn) Probe(ctx context.Context) (connector.DeviceProbe, error) {
	p, err := c.probe(ctx)
	if err != nil {
		if domain.CodeOf(err) == "" {
			code := domain.CodeUnexpectedResponse
			if ctx.Err() != nil {
				code = domain.CodeTimeout
			}
			err = domain.WithCode(err, code)
		}
		c.log.Warn("device probe failed", slog.String("code", string(domain.CodeOf(err))), slog.Any("error", err))
		return p, err
	}
	c.log.Info("device probe succeeded", slog.String("version", p.RouterOSVersion), slog.Int("lte_ports", len(p.Ports)))
	return p, nil
}

func (c *Conn) probe(ctx context.Context) (connector.DeviceProbe, error) {
	var p connector.DeviceProbe

	// 1. Connectivity, TLS, credentials and the api/rest-api/read policies.
	resp, err := c.do(ctx, call{op: opProbe, method: http.MethodGet, path: "/system/resource"})
	if c.secure {
		p.TLS = &connector.TLSInfo{Mode: c.tlsMode}
		switch {
		case resp != nil && resp.tls != nil && len(resp.tls.PeerCertificates) > 0:
			p.TLS.PeerFingerprint = certFingerprint(resp.tls.PeerCertificates[0])
			p.TLS.Verified = c.tlsMode != domain.TLSInsecure
		case domain.CodeOf(err) == domain.CodeTLSError:
			p.TLS.PeerFingerprint = c.peerFingerprint(ctx)
		}
	}
	if err != nil {
		return p, err
	}
	var res record
	if err := decode(opProbe, "/system/resource", resp, &res); err != nil {
		return p, err
	}
	p.RouterOSVersion = res.str("version")
	p.BoardName = res.str("board-name")
	if err := checkVersion(p.RouterOSVersion); err != nil {
		return p, err
	}

	// 2. Board model (absent on CHR/x86, so only auth problems count).
	if resp, err := c.do(ctx, call{op: opProbe, method: http.MethodGet, path: "/system/routerboard"}); err != nil {
		if domain.KindOf(err) == domain.KindAuth {
			return p, err
		}
	} else if p.BoardName == "" {
		var rb record
		if decode(opProbe, "/system/routerboard", resp, &rb) == nil {
			p.BoardName = rb.str("model")
		}
	}

	// 3. SMS package and its settings are readable.
	if _, err := c.do(ctx, call{op: opProbe, method: http.MethodGet, path: "/tool/sms"}); err != nil {
		return p, err
	}

	// 4. LTE ports and their state.
	ifaces, err := c.lteInterfaces(ctx, opProbe)
	if err != nil {
		return p, err
	}
	for _, it := range ifaces {
		h, err := c.portHealth(ctx, opProbe, it)
		if err != nil {
			if domain.KindOf(err) == domain.KindAuth || ctx.Err() != nil {
				return p, err
			}
			// One port's monitor failing does not fail the test; the port
			// is listed with what is known.
			c.log.Warn("probe: lte monitor failed", slog.String("port", it.name), slog.Any("error", err))
		}
		p.Ports = append(p.Ports, connector.PortInfo{Port: it.name, Health: h})
	}
	return p, nil
}

var versionRe = regexp.MustCompile(`^\s*(\d+)\.(\d+)`)

// checkVersion rejects RouterOS older than MinVersion.
func checkVersion(v string) error {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return &domain.ConnectorError{Kind: domain.KindPermanent, Op: opProbe, Code: domain.CodeUnexpectedResponse,
			Err: fmt.Errorf("%w: RouterOS version %q not recognised", ErrUnexpectedResponse, v)}
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < MinVersion[0] || (major == MinVersion[0] && minor < MinVersion[1]) {
		return &domain.ConnectorError{Kind: domain.KindPermanent, Op: opProbe, Code: domain.CodeUnsupportedVersion,
			Err: fmt.Errorf("%w: %s, need %d.%d or newer", ErrUnsupportedVersion, v, MinVersion[0], MinVersion[1])}
	}
	return nil
}

// peerFingerprint fetches the device certificate without verifying it, only
// to show its fingerprint after a failed handshake. Nothing is sent over this
// connection.
func (c *Conn) peerFingerprint(ctx context.Context) string {
	u, err := url.Parse(c.base)
	if err != nil {
		return ""
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	d := tls.Dialer{
		NetDialer: &net.Dialer{},
		// #nosec G402 -- the certificate is only read for display, never trusted.
		Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, ServerName: u.Hostname()},
	}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return ""
	}
	return certFingerprint(certs[0])
}
