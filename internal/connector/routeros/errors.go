// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"syscall"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/logging"
)

// Operation names used in domain.ConnectorError.Op.
const (
	opSend        = "send"
	opFetchInbox  = "fetch_inbox"
	opDeleteInbox = "delete_inbox"
	opHealth      = "health"
	opProbe       = "probe"
)

// Sentinel causes wrapped into connector errors; test with errors.Is.
var (
	// ErrUnsupportedEncoding: the message encoding is not in Capabilities.
	ErrUnsupportedEncoding = errors.New("encoding not supported by RouterOS")
	// ErrTooLong: the message exceeds Capabilities (segments or septets).
	ErrTooLong = errors.New("message too long for RouterOS")
	// ErrInvalidMessage: the message cannot be sent at all (no recipient,
	// characters outside GSM-7).
	ErrInvalidMessage = errors.New("invalid message")
	// ErrUnknownPort: the channel's port is not an LTE interface of the device.
	ErrUnknownPort = errors.New("unknown LTE port")
	// ErrFingerprintMismatch: the device presented a certificate other than
	// the pinned one.
	ErrFingerprintMismatch = errors.New("certificate fingerprint mismatch")
	// ErrResponseTooLarge: the response exceeded Options.MaxResponseBytes.
	ErrResponseTooLarge = errors.New("response too large")
	// ErrUnexpectedResponse: the device answered with something tikker
	// cannot interpret.
	ErrUnexpectedResponse = errors.New("unexpected response")
	// ErrUnsupportedVersion: RouterOS is older than MinVersion.
	ErrUnsupportedVersion = errors.New("unsupported RouterOS version")
	// ErrClosed: the connector was closed.
	ErrClosed = errors.New("connector closed")
)

// statusRule maps a RouterOS HTTP error to a connector error class. The
// rules are evaluated in order; the first match wins. Status 0 matches any
// status; an empty match matches any detail. Match strings are compared
// case-insensitively against "message: detail" of the RouterOS error body.
//
// This table is the single place to correct after the T00 measurements
// (docs/research/routeros-sms.md W6). Every line that is not taken verbatim
// from the RouterOS documentation is an ASSUMPTION(T00).
type statusRule struct {
	status int
	match  []string
	// sendOnly limits the rule to Send (validation errors of the message).
	sendOnly bool
	kind     domain.ErrorKind
	code     domain.ErrorCode
	// ambiguous marks the request as possibly executed (Send only).
	ambiguous bool
}

var statusRules = []statusRule{
	// Documented (research W2/W6): bad credentials or missing rest-api policy.
	{status: http.StatusUnauthorized, kind: domain.KindAuth, code: domain.CodeAuthFailed},
	// ASSUMPTION(T00): RouterOS is not known to answer 403; treat it as a
	// policy problem like SPEC §3.2 (401/403 = Auth).
	{status: http.StatusForbidden, kind: domain.KindAuth, code: domain.CodeInsufficientPolicy},
	// Documented: missing api policy gives 500 "not allowed (9)"; forum
	// reports "not enough permissions (9)" for missing read/write/test.
	{match: []string{"not allowed", "not enough permissions"}, kind: domain.KindAuth, code: domain.CodeInsufficientPolicy},
	// Documented: REST ends every request after 60 s with 400 "Session
	// closed". The command may still be running on the modem.
	// ASSUMPTION(T00): the SMS can still go out, so a send is ambiguous.
	{status: http.StatusBadRequest, match: []string{"session closed"}, kind: domain.KindTransient, code: domain.CodeTimeout, ambiguous: true},
	// ASSUMPTION(T00): a gateway timeout between tikker and the router
	// leaves the command's fate unknown.
	{status: http.StatusGatewayTimeout, kind: domain.KindTransient, code: domain.CodeTimeout, ambiguous: true},
	// ASSUMPTION(T00): an unknown path means the RouterOS version lacks the
	// command (or the package is missing).
	{match: []string{"no such command"}, kind: domain.KindTransient, code: domain.CodeUnsupportedVersion},
	// ASSUMPTION(T00): modem/network state errors; wording unknown, only the
	// fake's "not registered to network" is certain to match.
	{match: []string{"not registered", "no sim", "sim not", "busy", "inactive", "timeout", "timed out", "not ready", "modem"}, kind: domain.KindTransient},
	// ASSUMPTION(T00): validation errors of the message itself. Wording
	// from the fake (forum F3 for "Body too long").
	{status: http.StatusBadRequest, sendOnly: true, kind: domain.KindPermanent, match: []string{
		"invalid phone", "phone-number", "too long", "outside the gsm", "7-bit", "invalid value for argument message",
	}},
}

// routerError is the RouterOS REST error document (research W6):
// {"error":400,"message":"Bad Request","detail":"..."}.
type routerError struct {
	Error   int    `json:"error"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

// maxDetail bounds the RouterOS detail text copied into errors.
const maxDetail = 160

// phoneLike matches digit runs that may be phone numbers echoed by the
// router; they are masked before the text reaches an error or a log.
var phoneLike = regexp.MustCompile(`\+?\d[\d ]{4,}\d`)

// sanitizeDetail masks numbers and truncates router-provided text so that
// errors (logged and stored in Message.LastError) carry no full numbers and
// no long echoes of the request.
func sanitizeDetail(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "?"))
	s = phoneLike.ReplaceAllStringFunc(s, logging.MaskPhone)
	if r := []rune(s); len(r) > maxDetail {
		s = string(r[:maxDetail]) + "…"
	}
	return s
}

// parseRouterError extracts "message: detail" from a RouterOS error body,
// falling back to the start of the raw body.
func parseRouterError(body []byte) string {
	var re routerError
	if err := json.Unmarshal(body, &re); err == nil && (re.Message != "" || re.Detail != "") {
		switch {
		case re.Detail == "":
			return re.Message
		case re.Message == "":
			return re.Detail
		}
		return re.Message + ": " + re.Detail
	}
	return string(body)
}

// classifyStatus turns a non-2xx response into a connector error.
func classifyStatus(op, method, path string, status int, body []byte) error {
	text := sanitizeDetail(parseRouterError(body))
	cause := fmt.Errorf("%s %s: HTTP %d: %s", method, path, status, text)
	lower := strings.ToLower(text)
	for _, r := range statusRules {
		if r.status != 0 && r.status != status {
			continue
		}
		if r.sendOnly && op != opSend {
			continue
		}
		if len(r.match) > 0 && !containsAny(lower, r.match) {
			continue
		}
		return &domain.ConnectorError{
			Kind: r.kind, Op: op, Code: r.code,
			Ambiguous: r.ambiguous && op == opSend, Err: cause,
		}
	}
	switch {
	case status >= 500:
		// 5xx without a recognised detail: the command failed on the router.
		return &domain.ConnectorError{Kind: domain.KindTransient, Op: op, Err: cause}
	case status >= 300 && status < 400:
		// Redirects are not followed (credentials must not leave the device).
		return &domain.ConnectorError{Kind: domain.KindTransient, Op: op, Code: domain.CodeUnexpectedResponse, Err: cause}
	}
	// Unknown 4xx: under at-least-once an unknown failure is retried.
	return &domain.ConnectorError{Kind: domain.KindTransient, Op: op, Code: domain.CodeUnexpectedResponse, Err: cause}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// classifyTransport turns an error from http.Client.Do (or from reading the
// body) into a connector error. maybeSent is true when the request may have
// reached the router (a connection was obtained and writing did not fail);
// only then can a failed send be ambiguous.
func classifyTransport(op string, mutating, maybeSent bool, err error) error {
	code := transportCode(err)
	ce := &domain.ConnectorError{Kind: domain.KindTransient, Op: op, Code: code, Err: err}
	if code == domain.CodeTLSError {
		// The handshake precedes the request, so nothing was sent.
		return ce
	}
	ce.Ambiguous = mutating && maybeSent
	return ce
}

// transportCode chooses the device-test error code for a transport error.
func transportCode(err error) domain.ErrorCode {
	if isTLSError(err) {
		return domain.CodeTLSError
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.CodeTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return domain.CodeTimeout
	}
	if errors.Is(err, context.Canceled) {
		return ""
	}
	if errors.Is(err, ErrResponseTooLarge) {
		return domain.CodeUnexpectedResponse
	}
	return domain.CodeUnreachable
}

func isTLSError(err error) bool {
	var (
		unknownAuth  x509.UnknownAuthorityError
		hostname     x509.HostnameError
		invalid      x509.CertificateInvalidError
		verification *tls.CertificateVerificationError
		record       tls.RecordHeaderError
		alert        tls.AlertError
	)
	switch {
	case errors.Is(err, ErrFingerprintMismatch),
		errors.As(err, &unknownAuth), errors.As(err, &hostname), errors.As(err, &invalid),
		errors.As(err, &verification), errors.As(err, &record), errors.As(err, &alert):
		return true
	}
	// Handshake failures without a typed error ("remote error: tls: ...",
	// "tls: first record does not look like a TLS handshake").
	msg := err.Error()
	return (strings.Contains(msg, "tls: ") || strings.Contains(msg, "HTTP response to HTTPS client")) &&
		!errors.Is(err, syscall.ECONNREFUSED)
}

// localError is a connector error raised before contacting the router.
func localError(kind domain.ErrorKind, op string, err error) error {
	return &domain.ConnectorError{Kind: kind, Op: op, Err: err}
}
