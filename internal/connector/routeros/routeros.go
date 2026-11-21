// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package routeros implements the "routeros-rest" connector: tikker calls the
// RouterOS 7 REST API (/rest/tool/sms/*, /rest/interface/lte,
// /rest/system/*) of one MikroTik device (SPEC §3, PLAN T12).
//
// Register it explicitly (no init-time registration):
//
//	reg := connector.NewRegistry()
//	err := routeros.Register(reg, routeros.Options{Logger: logger})
//
// Behaviour that has not been measured on real hardware yet (PLAN T00) is
// marked "ASSUMPTION(T00):" and kept in few places: the error table in
// errors.go, the constants below and the field names in health.go and
// inbox.go. See README.md.
package routeros

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

// Defaults. ASSUMPTION(T00) where noted.
const (
	// DefaultSendTimeout: RouterOS sends take up to 60 s and REST closes the
	// request after 60 s (research W7), plus a margin (ADR 0002).
	DefaultSendTimeout = 65 * time.Second
	// DefaultRequestTimeout bounds every other call.
	// ASSUMPTION(T00): RouterOS queues the requests of one user behind a
	// running command, so a health check during a send may wait for it.
	DefaultRequestTimeout = 15 * time.Second
	// DefaultMaxResponseBytes caps a response body (a full inbox with PDUs
	// is far smaller).
	DefaultMaxResponseBytes = 4 << 20
	// DefaultDialTimeout and DefaultTLSHandshakeTimeout bound connection set-up.
	DefaultDialTimeout         = 10 * time.Second
	DefaultTLSHandshakeTimeout = 10 * time.Second
	// maxConnsPerHost: sends on every port, inbox polling and health checks
	// may run at once; RouterOS serialises one user's requests anyway.
	maxConnsPerHost = 8
	idleConnTimeout = 90 * time.Second
)

// MinVersion is the oldest RouterOS release Probe accepts.
// ASSUMPTION(T00): working minimum from research W1 ("polling" in /tool sms).
var MinVersion = [2]int{7, 16}

// capabilities of /tool sms send (research W4/W5/W7).
// ASSUMPTION(T00): GSM-7 only, one 160-septet SMS, no concatenation, one
// command per modem port, inbox entries carry a "pdu".
func capabilities(sendTimeout time.Duration) connector.Capabilities {
	return connector.Capabilities{
		Encodings:          []domain.Encoding{domain.EncodingGSM7},
		MaxSegments:        1,
		Concatenation:      false,
		MaxConcurrentSends: 1,
		SendTimeout:        sendTimeout,
		InboundPDU:         true,
	}
}

// Options tune the connector; the zero value is valid.
type Options struct {
	// Logger receives connector logs (default slog.Default()). Message
	// bodies and passwords are never logged; numbers are masked.
	Logger *slog.Logger
	// Clock stamps SendResult.AcceptedAt and ChannelHealth.CheckedAt
	// (default clock.System()).
	Clock clock.Clock
	// Location is the time zone of inbox timestamps (default UTC).
	// ASSUMPTION(T00): RouterOS prints the SMSC time without a zone.
	Location *time.Location
	// SendTimeout bounds one Send (default DefaultSendTimeout). It is also
	// reported as Capabilities.SendTimeout.
	SendTimeout time.Duration
	// RequestTimeout bounds every other request (default
	// DefaultRequestTimeout; DeviceConfig.Timeout takes precedence).
	RequestTimeout time.Duration
	// MaxResponseBytes caps response bodies (default DefaultMaxResponseBytes).
	MaxResponseBytes int64
	// DialTimeout and TLSHandshakeTimeout bound connection set-up.
	DialTimeout         time.Duration
	TLSHandshakeTimeout time.Duration

	// rootCAs replaces the system roots in TLSVerify mode (tests only).
	rootCAs *x509.CertPool
}

// Conn is the connector of one RouterOS device. It is safe for concurrent use.
type Conn struct {
	deviceID domain.ID
	base     string // scheme://host[:port]/rest
	user     string
	password string
	secure   bool // https
	tlsMode  domain.TLSMode
	pin      []byte // SHA-256 for TLSFingerprint

	hc  *http.Client
	tr  *http.Transport
	log *slog.Logger
	clk clock.Clock
	loc *time.Location

	caps           connector.Capabilities
	requestTimeout time.Duration
	maxBody        int64

	portsMu sync.Mutex
	ports   map[string]chan struct{} // send slots per modem port

	delVariant atomic.Int32 // deleteUnknown, deleteByPath, deleteByRemove
	closed     atomic.Bool
}

var _ connector.Connector = (*Conn)(nil)

// New is the connector.Factory with default Options (slog.Default()).
// Prefer Factory or Register to inject a logger.
func New(ctx context.Context, cfg connector.DeviceConfig) (connector.Connector, error) {
	return NewConn(ctx, cfg, Options{})
}

// Factory returns a connector.Factory that builds connectors with opts.
func Factory(opts Options) connector.Factory {
	return func(ctx context.Context, cfg connector.DeviceConfig) (connector.Connector, error) {
		return NewConn(ctx, cfg, opts)
	}
}

// Register adds the routeros-rest factory with opts to reg.
func Register(reg *connector.Registry, opts Options) error {
	return reg.Register(domain.ConnectorRouterOSREST, Factory(opts))
}

// NewConn validates cfg and builds the HTTP client. It does not contact the
// device.
func NewConn(_ context.Context, cfg connector.DeviceConfig, opts Options) (*Conn, error) {
	opts = withDefaults(opts)
	base, secure, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("routeros device %s: %w", cfg.DeviceID, err)
	}
	mode := cfg.TLSMode
	if mode == "" {
		mode = domain.TLSVerify
	}
	if !mode.Valid() {
		return nil, fmt.Errorf("routeros device %s: invalid tls mode %q", cfg.DeviceID, mode)
	}
	c := &Conn{
		deviceID: cfg.DeviceID,
		base:     base,
		user:     cfg.Username,
		password: cfg.Password,
		secure:   secure,
		tlsMode:  mode,
		log:      opts.Logger.With(slog.String("connector", string(domain.ConnectorRouterOSREST)), slog.String("device_id", cfg.DeviceID.String())),
		clk:      opts.Clock,
		loc:      opts.Location,
		caps:     capabilities(opts.SendTimeout),
		maxBody:  opts.MaxResponseBytes,
		ports:    map[string]chan struct{}{},
	}
	c.requestTimeout = opts.RequestTimeout
	if cfg.Timeout > 0 {
		c.requestTimeout = cfg.Timeout
	}

	var tc *tls.Config
	if secure {
		if tc, err = c.tlsConfig(cfg, opts); err != nil {
			return nil, fmt.Errorf("routeros device %s: %w", cfg.DeviceID, err)
		}
	}
	dialer := &net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}
	c.tr = &http.Transport{
		// A router on the LAN is never reached through an HTTP proxy, and a
		// proxy from the environment would see the basic-auth header.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tc,
		TLSHandshakeTimeout:   opts.TLSHandshakeTimeout,
		ResponseHeaderTimeout: opts.SendTimeout + 5*time.Second, // backstop; contexts bound each call
		MaxIdleConns:          maxConnsPerHost,
		MaxIdleConnsPerHost:   maxConnsPerHost,
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ForceAttemptHTTP2:     false, // RouterOS speaks HTTP/1.1
	}
	c.hc = &http.Client{
		Transport: c.tr,
		// Redirects are not followed: the basic-auth header must not reach
		// another host. A 3xx is an unexpected response.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if !secure {
		c.log.Warn("device uses plain HTTP: credentials and messages travel unencrypted")
	} else if mode == domain.TLSInsecure {
		c.log.Warn("TLS certificate verification is disabled for this device (tls_mode=insecure)")
	}
	return c, nil
}

func withDefaults(o Options) Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.SendTimeout <= 0 {
		o.SendTimeout = DefaultSendTimeout
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = DefaultDialTimeout
	}
	if o.TLSHandshakeTimeout <= 0 {
		o.TLSHandshakeTimeout = DefaultTLSHandshakeTimeout
	}
	return o
}

// parseBaseURL accepts "https://host[:port][/rest]" (or http://) and returns
// the REST root without a trailing slash.
func parseBaseURL(raw string) (base string, secure bool, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false, fmt.Errorf("invalid base URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		secure = true
	case "http":
	default:
		return "", false, fmt.Errorf("invalid base URL scheme %q (want https or http)", u.Scheme)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false, errors.New("invalid base URL: want scheme://host[:port] without credentials, query or fragment")
	}
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, "/rest") {
		p += "/rest"
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host + p, secure, nil
}

// tlsConfig builds the TLS settings for cfg.TLSMode (SPEC §3.3).
func (c *Conn) tlsConfig(cfg connector.DeviceConfig, opts Options) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	switch c.tlsMode {
	case domain.TLSVerify:
		tc.RootCAs = opts.rootCAs // nil: system roots
	case domain.TLSCustomCA:
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CAPEM)) {
			return nil, errors.New("tls_mode=ca: no certificate found in ca_pem")
		}
		tc.RootCAs = pool
	case domain.TLSFingerprint:
		pin, err := ParseFingerprint(cfg.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf("tls_mode=fingerprint: %w", err)
		}
		c.pin = pin
		// The chain is not verified against any roots: the pinned leaf
		// certificate (SHA-256 of its DER encoding) is the only trust
		// anchor, checked in VerifyPeerCertificate on every handshake.
		tc.InsecureSkipVerify = true // #nosec G402 -- replaced by the fingerprint check below
		tc.VerifyPeerCertificate = c.verifyPin
	case domain.TLSInsecure:
		tc.InsecureSkipVerify = true // #nosec G402 -- explicit per-device opt-in, warned about in logs and UI
	}
	return tc, nil
}

func (c *Conn) verifyPin(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("%w: no certificate presented", ErrFingerprintMismatch)
	}
	sum := sha256.Sum256(rawCerts[0])
	if !equalBytes(sum[:], c.pin) {
		return fmt.Errorf("%w: device presented %s", ErrFingerprintMismatch, FormatFingerprint(sum[:]))
	}
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ParseFingerprint parses a SHA-256 fingerprint in hex, upper or lower case,
// with or without ':' or ' ' separators.
func ParseFingerprint(s string) ([]byte, error) {
	clean := strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.TrimSpace(s))
	b, err := hex.DecodeString(clean)
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("fingerprint must be a SHA-256 digest in hex (64 hex digits)")
	}
	return b, nil
}

// FormatFingerprint renders a digest as "AB:CD:..." (connector.TLSInfo).
func FormatFingerprint(sum []byte) string {
	h := strings.ToUpper(hex.EncodeToString(sum))
	var b strings.Builder
	for i := 0; i+1 < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

func certFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return FormatFingerprint(sum[:])
}

// Capabilities implements connector.Connector.
func (c *Conn) Capabilities() connector.Capabilities {
	caps := c.caps
	caps.Encodings = append([]domain.Encoding(nil), caps.Encodings...)
	return caps
}

// InsecureTransport reports whether traffic to the device is unencrypted
// (http://) or unauthenticated (tls_mode=insecure). Whether that is allowed
// is decided by the API layer, not by the connector.
func (c *Conn) InsecureTransport() bool {
	return !c.secure || c.tlsMode == domain.TLSInsecure
}

// Close implements connector.Connector.
func (c *Conn) Close() error {
	c.closed.Store(true)
	c.tr.CloseIdleConnections()
	return nil
}

// portSlot returns the send semaphore of port (capacity MaxConcurrentSends).
func (c *Conn) portSlot(port string) chan struct{} {
	c.portsMu.Lock()
	defer c.portsMu.Unlock()
	s, ok := c.ports[port]
	if !ok {
		s = make(chan struct{}, c.caps.MaxConcurrentSends)
		c.ports[port] = s
	}
	return s
}
