// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import "fmt"

// ParseError reports a value that is not a member of an enumeration (or not a
// valid ID).
type ParseError struct {
	Type  string // e.g. "direction"
	Value string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("invalid %s %q", e.Type, e.Value)
}

// enum is satisfied by every string enumeration in this package.
type enum interface {
	~string
	Valid() bool
}

func parseEnum[T enum](typ, s string) (T, error) {
	v := T(s)
	if !v.Valid() {
		return v, &ParseError{Type: typ, Value: s}
	}
	return v, nil
}

func marshalEnum[T enum](typ string, v T) ([]byte, error) {
	if !v.Valid() {
		return nil, &ParseError{Type: typ, Value: string(v)}
	}
	return []byte(v), nil
}

func unmarshalEnum[T enum](typ string, b []byte, dst *T) error {
	v, err := parseEnum[T](typ, string(b))
	if err != nil {
		return err
	}
	*dst = v
	return nil
}

// ---------------------------------------------------------------------------

// Direction tells outbound messages from inbound ones.
type Direction string

// Direction values.
const (
	DirectionOut Direction = "out"
	DirectionIn  Direction = "in"
)

// ParseDirection parses s as a [Direction].
func ParseDirection(s string) (Direction, error) { return parseEnum[Direction]("direction", s) }

// String returns the wire value.
func (d Direction) String() string { return string(d) }

// Valid reports whether d is a known value.
func (d Direction) Valid() bool { return d == DirectionOut || d == DirectionIn }

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (d Direction) MarshalText() ([]byte, error) { return marshalEnum("direction", d) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (d *Direction) UnmarshalText(b []byte) error { return unmarshalEnum("direction", b, d) }

// ---------------------------------------------------------------------------

// MessageStatus is the lifecycle state of a message. Outbound messages move
// through the state machine in statemachine.go; inbound messages are always
// [StatusReceived].
type MessageStatus string

// MessageStatus values.
const (
	StatusQueued    MessageStatus = "queued"
	StatusSending   MessageStatus = "sending"
	StatusSent      MessageStatus = "sent"
	StatusFailed    MessageStatus = "failed"
	StatusCancelled MessageStatus = "cancelled"
	StatusReceived  MessageStatus = "received"
)

// AllMessageStatuses lists every status, in lifecycle order.
var AllMessageStatuses = []MessageStatus{
	StatusQueued, StatusSending, StatusSent, StatusFailed, StatusCancelled, StatusReceived,
}

// ParseMessageStatus parses s as a [MessageStatus].
func ParseMessageStatus(s string) (MessageStatus, error) {
	return parseEnum[MessageStatus]("message status", s)
}

// String returns the wire value.
func (s MessageStatus) String() string { return string(s) }

// Valid reports whether s is a known value.
func (s MessageStatus) Valid() bool {
	switch s {
	case StatusQueued, StatusSending, StatusSent, StatusFailed, StatusCancelled, StatusReceived:
		return true
	}
	return false
}

// Terminal reports whether no further transition is possible from s.
func (s MessageStatus) Terminal() bool {
	switch s {
	case StatusSent, StatusFailed, StatusCancelled, StatusReceived:
		return true
	}
	return false
}

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (s MessageStatus) MarshalText() ([]byte, error) { return marshalEnum("message status", s) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (s *MessageStatus) UnmarshalText(b []byte) error {
	return unmarshalEnum("message status", b, s)
}

// ---------------------------------------------------------------------------

// Encoding is the SMS character encoding.
type Encoding string

// Encoding values.
const (
	EncodingGSM7 Encoding = "gsm7"
	EncodingUCS2 Encoding = "ucs2"
)

// ParseEncoding parses s as an [Encoding].
func ParseEncoding(s string) (Encoding, error) { return parseEnum[Encoding]("encoding", s) }

// String returns the wire value.
func (e Encoding) String() string { return string(e) }

// Valid reports whether e is a known value.
func (e Encoding) Valid() bool { return e == EncodingGSM7 || e == EncodingUCS2 }

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (e Encoding) MarshalText() ([]byte, error) { return marshalEnum("encoding", e) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (e *Encoding) UnmarshalText(b []byte) error { return unmarshalEnum("encoding", b, e) }

// ---------------------------------------------------------------------------

// ChannelState is the health of a channel as decided by the health checker.
type ChannelState string

// ChannelState values.
const (
	// ChannelOK: reachable, modem registered; the channel takes traffic.
	ChannelOK ChannelState = "ok"
	// ChannelDegraded: reachable but not usable, e.g. authentication
	// failure or modem not registered. Needs attention in the UI.
	ChannelDegraded ChannelState = "degraded"
	// ChannelDown: the device does not answer.
	ChannelDown ChannelState = "down"
	// ChannelDisabled: switched off by an administrator.
	ChannelDisabled ChannelState = "disabled"
)

// ParseChannelState parses s as a [ChannelState].
func ParseChannelState(s string) (ChannelState, error) {
	return parseEnum[ChannelState]("channel state", s)
}

// String returns the wire value.
func (c ChannelState) String() string { return string(c) }

// Valid reports whether c is a known value.
func (c ChannelState) Valid() bool {
	switch c {
	case ChannelOK, ChannelDegraded, ChannelDown, ChannelDisabled:
		return true
	}
	return false
}

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (c ChannelState) MarshalText() ([]byte, error) { return marshalEnum("channel state", c) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (c *ChannelState) UnmarshalText(b []byte) error {
	return unmarshalEnum("channel state", b, c)
}

// ---------------------------------------------------------------------------

// Role is the permission level of a panel user.
type Role string

// Role values, from least to most privileged.
const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// ParseRole parses s as a [Role].
func ParseRole(s string) (Role, error) { return parseEnum[Role]("role", s) }

// String returns the wire value.
func (r Role) String() string { return string(r) }

// Valid reports whether r is a known value.
func (r Role) Valid() bool { return r.rank() > 0 }

func (r Role) rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// Allows reports whether a user with role r may perform an action that
// requires at least role required. Roles are ordered viewer < operator <
// admin. Invalid roles allow nothing.
func (r Role) Allows(required Role) bool {
	return r.Valid() && required.Valid() && r.rank() >= required.rank()
}

// Scopes returns the API scopes a user with role r may exercise: viewer ->
// sms:read; operator -> sms:read, sms:send; admin -> everything.
func (r Role) Scopes() Scopes {
	switch r {
	case RoleViewer:
		return Scopes{ScopeSMSRead}
	case RoleOperator:
		return Scopes{ScopeSMSRead, ScopeSMSSend}
	case RoleAdmin:
		return Scopes{ScopeAdmin}
	}
	return nil
}

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (r Role) MarshalText() ([]byte, error) { return marshalEnum("role", r) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (r *Role) UnmarshalText(b []byte) error { return unmarshalEnum("role", b, r) }

// ---------------------------------------------------------------------------

// Scope is a permission granted to an API token.
type Scope string

// Scope values.
const (
	ScopeSMSSend Scope = "sms:send"
	ScopeSMSRead Scope = "sms:read"
	// ScopeAdmin grants access to administrative endpoints and implies every
	// other scope.
	ScopeAdmin Scope = "admin"
)

// ParseScope parses s as a [Scope].
func ParseScope(s string) (Scope, error) { return parseEnum[Scope]("scope", s) }

// String returns the wire value.
func (s Scope) String() string { return string(s) }

// Valid reports whether s is a known value.
func (s Scope) Valid() bool {
	switch s {
	case ScopeSMSSend, ScopeSMSRead, ScopeAdmin:
		return true
	}
	return false
}

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (s Scope) MarshalText() ([]byte, error) { return marshalEnum("scope", s) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (s *Scope) UnmarshalText(b []byte) error { return unmarshalEnum("scope", b, s) }

// Scopes is the set of scopes granted to a token.
type Scopes []Scope

// Allows reports whether the set grants required. [ScopeAdmin] grants
// everything.
func (ss Scopes) Allows(required Scope) bool {
	for _, s := range ss {
		if s == required || s == ScopeAdmin {
			return true
		}
	}
	return false
}

// TokenAllows reports whether a token with scopes granted, owned by a user
// with role owner, may use required. The effective permission is the
// intersection of the token's scopes and the owner's role, so demoting a user
// immediately narrows all of their tokens.
func TokenAllows(granted Scopes, owner Role, required Scope) bool {
	return granted.Allows(required) && owner.Scopes().Allows(required)
}

// Valid reports whether the set is non-empty and every member is valid.
func (ss Scopes) Valid() bool {
	if len(ss) == 0 {
		return false
	}
	for _, s := range ss {
		if !s.Valid() {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------

// TLSMode selects how the connector verifies a device's TLS certificate
// (SPEC §3.3). Plain HTTP is selected by an http:// base URL instead and is
// only acceptable on the local veth link.
type TLSMode string

// TLSMode values.
const (
	// TLSVerify verifies the certificate against the system roots.
	TLSVerify TLSMode = "verify"
	// TLSCustomCA verifies against Device.CAPEM only.
	TLSCustomCA TLSMode = "ca"
	// TLSFingerprint accepts exactly the certificate whose SHA-256
	// fingerprint equals Device.Fingerprint (useful for RouterOS
	// self-signed certificates).
	TLSFingerprint TLSMode = "fingerprint"
	// TLSInsecure skips verification. The UI shows a warning.
	TLSInsecure TLSMode = "insecure"
)

// ParseTLSMode parses s as a [TLSMode].
func ParseTLSMode(s string) (TLSMode, error) { return parseEnum[TLSMode]("tls mode", s) }

// String returns the wire value.
func (m TLSMode) String() string { return string(m) }

// Valid reports whether m is a known value.
func (m TLSMode) Valid() bool {
	switch m {
	case TLSVerify, TLSCustomCA, TLSFingerprint, TLSInsecure:
		return true
	}
	return false
}

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (m TLSMode) MarshalText() ([]byte, error) { return marshalEnum("tls mode", m) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (m *TLSMode) UnmarshalText(b []byte) error { return unmarshalEnum("tls mode", b, m) }

// ---------------------------------------------------------------------------

// ConnectorKind names the connector implementation used to talk to a device.
type ConnectorKind string

// ConnectorKind values.
const (
	// ConnectorRouterOSREST: tikker calls the RouterOS REST API (MVP).
	ConnectorRouterOSREST ConnectorKind = "routeros-rest"
	// ConnectorAgent: a script on the router connects to tikker and pulls
	// work (post-MVP, works behind CGNAT without VPN).
	ConnectorAgent ConnectorKind = "agent"
)

// ParseConnectorKind parses s as a [ConnectorKind].
func ParseConnectorKind(s string) (ConnectorKind, error) {
	return parseEnum[ConnectorKind]("connector kind", s)
}

// String returns the wire value.
func (k ConnectorKind) String() string { return string(k) }

// Valid reports whether k is a known value.
func (k ConnectorKind) Valid() bool { return k == ConnectorRouterOSREST || k == ConnectorAgent }

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (k ConnectorKind) MarshalText() ([]byte, error) { return marshalEnum("connector kind", k) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (k *ConnectorKind) UnmarshalText(b []byte) error {
	return unmarshalEnum("connector kind", b, k)
}

// ---------------------------------------------------------------------------

// ActorType says who performed an action.
type ActorType string

// ActorType values.
const (
	ActorUser   ActorType = "user"
	ActorToken  ActorType = "token"
	ActorSystem ActorType = "system"
)

// ParseActorType parses s as an [ActorType].
func ParseActorType(s string) (ActorType, error) { return parseEnum[ActorType]("actor type", s) }

// String returns the wire value.
func (a ActorType) String() string { return string(a) }

// Valid reports whether a is a known value.
func (a ActorType) Valid() bool {
	switch a {
	case ActorUser, ActorToken, ActorSystem:
		return true
	}
	return false
}

// MarshalText implements encoding.TextMarshaler. Invalid values are an error.
func (a ActorType) MarshalText() ([]byte, error) { return marshalEnum("actor type", a) }

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are an error.
func (a *ActorType) UnmarshalText(b []byte) error { return unmarshalEnum("actor type", b, a) }
