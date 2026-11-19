// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"context"
	"errors"
	"fmt"
)

// Sentinel errors shared by all layers. Wrap them with fmt.Errorf("...: %w")
// and test with errors.Is.
var (
	// ErrNotFound: the requested entity does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict: a uniqueness constraint was violated (username, token
	// prefix, channel port on a device, ...).
	ErrConflict = errors.New("conflict")
	// ErrInvalidTransition: the state machine forbids the requested change,
	// e.g. cancelling a message that is already sending.
	ErrInvalidTransition = errors.New("invalid status transition")
	// ErrLeaseLost: the caller's lease on a message is no longer current
	// (it expired and was recovered, possibly re-acquired by another worker).
	// The caller must drop the message without touching it.
	ErrLeaseLost = errors.New("lease lost")
	// ErrIdempotencyMismatch: an Idempotency-Key was reused within its
	// validity window with a different request (API 422).
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
)

// Connector error classes (SPEC §3.2). Test with errors.Is(err, ErrTransient)
// etc.; every [*ConnectorError] matches the sentinel of its kind.
var (
	// ErrTransient: network failure, timeout, HTTP 5xx, modem busy. The
	// operation may be retried.
	ErrTransient = errors.New("transient connector error")
	// ErrPermanent: the request itself is wrong (invalid number, HTTP 4xx
	// validation). Retrying cannot help; the message fails.
	ErrPermanent = errors.New("permanent connector error")
	// ErrAuth: HTTP 401/403. The channel becomes degraded and the UI shows
	// an alert; the message is retried later like a transient error.
	ErrAuth = errors.New("connector authentication error")
)

// ErrorKind classifies connector errors.
type ErrorKind string

// ErrorKind values.
const (
	KindTransient ErrorKind = "transient"
	KindPermanent ErrorKind = "permanent"
	KindAuth      ErrorKind = "auth"
)

// ParseErrorKind parses s as an [ErrorKind].
func ParseErrorKind(s string) (ErrorKind, error) { return parseEnum[ErrorKind]("error kind", s) }

// String returns the wire value.
func (k ErrorKind) String() string { return string(k) }

// Valid reports whether k is a known value.
func (k ErrorKind) Valid() bool {
	return k == KindTransient || k == KindPermanent || k == KindAuth
}

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (k ErrorKind) MarshalText() ([]byte, error) { return marshalEnum("error kind", k) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (k *ErrorKind) UnmarshalText(b []byte) error { return unmarshalEnum("error kind", b, k) }

func (k ErrorKind) sentinel() error {
	switch k {
	case KindPermanent:
		return ErrPermanent
	case KindAuth:
		return ErrAuth
	}
	return ErrTransient
}

// ConnectorError is the error type returned by connectors.
//
//	err := &domain.ConnectorError{Kind: domain.KindTransient, Op: "send", Err: ioErr}
//	errors.Is(err, domain.ErrTransient) // true
//	errors.Is(err, ioErr)               // true
type ConnectorError struct {
	Kind ErrorKind
	// Op is the connector operation: "send", "fetch_inbox", "delete_inbox",
	// "health", "probe".
	Op string
	// Code optionally refines the cause for display, e.g. in the device
	// test result. Empty when the connector cannot tell.
	Code ErrorCode
	// Ambiguous is set when the request may have been executed by the
	// router even though tikker saw an error (timeout or connection reset
	// after the request was written). A retried send is then flagged as a
	// possible duplicate. Only meaningful for KindTransient.
	Ambiguous bool
	// Err is the underlying cause. It must not contain secrets or message
	// bodies because it is logged and stored in Message.LastError.
	Err error
}

func (e *ConnectorError) Error() string {
	msg := fmt.Sprintf("connector %s: %s", e.Op, e.Kind)
	if e.Ambiguous {
		msg += " (ambiguous)"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap returns the underlying cause.
func (e *ConnectorError) Unwrap() error { return e.Err }

// Is makes the error match the sentinel of its kind.
func (e *ConnectorError) Is(target error) bool { return target == e.Kind.sentinel() }

// ErrorCode is a machine-readable cause of a connector error. The values
// match the "error.code" enum of DeviceTestResult in api/openapi.yaml; more
// codes may be added for send failures.
type ErrorCode string

// ErrorCode values.
const (
	CodeUnreachable        ErrorCode = "unreachable"
	CodeTimeout            ErrorCode = "timeout"
	CodeTLSError           ErrorCode = "tls_error"
	CodeAuthFailed         ErrorCode = "auth_failed"
	CodeInsufficientPolicy ErrorCode = "insufficient_policy"
	CodeUnsupportedVersion ErrorCode = "unsupported_version"
	CodeUnexpectedResponse ErrorCode = "unexpected_response"
)

// CodeOf returns the [ErrorCode] carried by err, or "" if none.
func CodeOf(err error) ErrorCode {
	var ce *ConnectorError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// WithCode sets Code on the [*ConnectorError] in err's chain and returns err.
// Errors without one are returned unchanged.
func WithCode(err error, code ErrorCode) error {
	var ce *ConnectorError
	if errors.As(err, &ce) {
		ce.Code = code
	}
	return err
}

// Transient returns a [KindTransient] connector error.
func Transient(op string, err error) error {
	return &ConnectorError{Kind: KindTransient, Op: op, Err: err}
}

// TransientAmbiguous returns a [KindTransient] connector error for a request
// that may nevertheless have been executed by the router.
func TransientAmbiguous(op string, err error) error {
	return &ConnectorError{Kind: KindTransient, Op: op, Ambiguous: true, Err: err}
}

// Permanent returns a [KindPermanent] connector error.
func Permanent(op string, err error) error {
	return &ConnectorError{Kind: KindPermanent, Op: op, Err: err}
}

// AuthError returns a [KindAuth] connector error.
func AuthError(op string, err error) error {
	return &ConnectorError{Kind: KindAuth, Op: op, Err: err}
}

// KindOf classifies err. A nil error has no kind and returns "". Errors that
// carry no classification (including context cancellation and deadlines) are
// treated as [KindTransient]: under the at-least-once guarantee an unknown
// failure must lead to a retry, bounded by the retry policy, never to a
// silent drop.
func KindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	var ce *ConnectorError
	if errors.As(err, &ce) && ce.Kind.Valid() {
		return ce.Kind
	}
	switch {
	case errors.Is(err, ErrPermanent):
		return KindPermanent
	case errors.Is(err, ErrAuth):
		return KindAuth
	}
	return KindTransient
}

// IsAmbiguous reports whether err says the operation may have taken effect
// despite failing. Unclassified errors that stem from a cancelled or expired
// context are ambiguous too, because the request may already have been
// written to the router.
func IsAmbiguous(err error) bool {
	if err == nil {
		return false
	}
	var ce *ConnectorError
	if errors.As(err, &ce) {
		return ce.Ambiguous
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
