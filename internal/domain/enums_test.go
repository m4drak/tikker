// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

// textEnum is implemented by every enum in this package via its pointer.
type textEnum interface {
	String() string
	Valid() bool
	MarshalText() ([]byte, error)
}

type enumCase struct {
	name    string
	values  []string
	parse   func(string) (textEnum, error)
	newZero func() interface{ UnmarshalText([]byte) error }
}

func wrapParse[T textEnum](f func(string) (T, error)) func(string) (textEnum, error) {
	return func(s string) (textEnum, error) {
		v, err := f(s)
		return v, err
	}
}

func enumCases() []enumCase {
	return []enumCase{
		{"direction", []string{"out", "in"}, wrapParse(ParseDirection),
			func() interface{ UnmarshalText([]byte) error } { return new(Direction) }},
		{"message status", []string{"queued", "sending", "sent", "failed", "cancelled", "received"}, wrapParse(ParseMessageStatus),
			func() interface{ UnmarshalText([]byte) error } { return new(MessageStatus) }},
		{"encoding", []string{"gsm7", "ucs2"}, wrapParse(ParseEncoding),
			func() interface{ UnmarshalText([]byte) error } { return new(Encoding) }},
		{"channel state", []string{"ok", "degraded", "down", "disabled"}, wrapParse(ParseChannelState),
			func() interface{ UnmarshalText([]byte) error } { return new(ChannelState) }},
		{"role", []string{"viewer", "operator", "admin"}, wrapParse(ParseRole),
			func() interface{ UnmarshalText([]byte) error } { return new(Role) }},
		{"scope", []string{"sms:send", "sms:read", "admin"}, wrapParse(ParseScope),
			func() interface{ UnmarshalText([]byte) error } { return new(Scope) }},
		{"tls mode", []string{"verify", "ca", "fingerprint", "insecure"}, wrapParse(ParseTLSMode),
			func() interface{ UnmarshalText([]byte) error } { return new(TLSMode) }},
		{"connector kind", []string{"routeros-rest", "agent"}, wrapParse(ParseConnectorKind),
			func() interface{ UnmarshalText([]byte) error } { return new(ConnectorKind) }},
		{"actor type", []string{"user", "token", "system"}, wrapParse(ParseActorType),
			func() interface{ UnmarshalText([]byte) error } { return new(ActorType) }},
	}
}

func TestEnums(t *testing.T) {
	for _, c := range enumCases() {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range c.values {
				v, err := c.parse(s)
				if err != nil {
					t.Fatalf("parse %q: %v", s, err)
				}
				if !v.Valid() || v.String() != s {
					t.Fatalf("parse %q -> %q valid=%v", s, v.String(), v.Valid())
				}
				b, err := v.MarshalText()
				if err != nil || string(b) != s {
					t.Fatalf("MarshalText(%q) = %q, %v", s, b, err)
				}
				dst := c.newZero()
				if err := dst.UnmarshalText(b); err != nil {
					t.Fatalf("UnmarshalText(%q): %v", s, err)
				}
				if got := dst.(textEnum).String(); got != s {
					t.Fatalf("UnmarshalText(%q) -> %q", s, got)
				}
			}
			for _, bad := range []string{"", "QUEUED", "unknown", " out"} {
				v, err := c.parse(bad)
				var pe *ParseError
				if !errors.As(err, &pe) {
					t.Fatalf("parse %q: err = %v, want *ParseError", bad, err)
				}
				if pe.Error() == "" {
					t.Fatal("empty error message")
				}
				if _, err := v.MarshalText(); err == nil {
					t.Fatalf("MarshalText(%q) succeeded", bad)
				}
				dst := c.newZero()
				if err := dst.UnmarshalText([]byte(bad)); err == nil {
					t.Fatalf("UnmarshalText(%q) succeeded", bad)
				}
				if got := dst.(textEnum).String(); got != "" {
					t.Fatalf("UnmarshalText(%q) modified destination to %q", bad, got)
				}
			}
		})
	}
}

func TestEnumJSON(t *testing.T) {
	type dto struct {
		Status MessageStatus `json:"status"`
		Scopes []Scope       `json:"scopes"`
	}
	in := dto{Status: StatusSent, Scopes: []Scope{ScopeSMSSend, ScopeAdmin}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"status":"sent","scopes":["sms:send","admin"]}` {
		t.Fatalf("json = %s", b)
	}
	var out dto
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusSent || len(out.Scopes) != 2 {
		t.Fatalf("decoded %+v", out)
	}
	if err := json.Unmarshal([]byte(`{"status":"nope"}`), &out); err == nil {
		t.Fatal("unknown status decoded without error")
	}
}

func TestMessageStatusTerminal(t *testing.T) {
	want := map[MessageStatus]bool{
		StatusQueued: false, StatusSending: false,
		StatusSent: true, StatusFailed: true, StatusCancelled: true, StatusReceived: true,
		"bogus": false,
	}
	for s, w := range want {
		if got := s.Terminal(); got != w {
			t.Errorf("%q.Terminal() = %v, want %v", s, got, w)
		}
	}
}

func TestRoleAllows(t *testing.T) {
	tests := []struct {
		role, required Role
		want           bool
	}{
		{RoleAdmin, RoleAdmin, true},
		{RoleAdmin, RoleOperator, true},
		{RoleAdmin, RoleViewer, true},
		{RoleOperator, RoleAdmin, false},
		{RoleOperator, RoleOperator, true},
		{RoleOperator, RoleViewer, true},
		{RoleViewer, RoleOperator, false},
		{RoleViewer, RoleViewer, true},
		{"", RoleViewer, false},
		{RoleAdmin, "", false},
		{"root", RoleViewer, false},
	}
	for _, tt := range tests {
		if got := tt.role.Allows(tt.required); got != tt.want {
			t.Errorf("%q.Allows(%q) = %v, want %v", tt.role, tt.required, got, tt.want)
		}
	}
}

func TestScopes(t *testing.T) {
	tests := []struct {
		name     string
		scopes   Scopes
		required Scope
		allows   bool
		valid    bool
	}{
		{"exact", Scopes{ScopeSMSSend}, ScopeSMSSend, true, true},
		{"missing", Scopes{ScopeSMSSend}, ScopeSMSRead, false, true},
		{"admin implies send", Scopes{ScopeAdmin}, ScopeSMSSend, true, true},
		{"admin implies read", Scopes{ScopeAdmin}, ScopeSMSRead, true, true},
		{"send does not imply admin", Scopes{ScopeSMSSend, ScopeSMSRead}, ScopeAdmin, false, true},
		{"empty", nil, ScopeSMSRead, false, false},
		{"invalid member", Scopes{ScopeSMSRead, "sms:delete"}, ScopeSMSRead, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.scopes.Allows(tt.required); got != tt.allows {
				t.Errorf("Allows(%q) = %v, want %v", tt.required, got, tt.allows)
			}
			if got := tt.scopes.Valid(); got != tt.valid {
				t.Errorf("Valid() = %v, want %v", got, tt.valid)
			}
		})
	}
}

func TestTokenAllows(t *testing.T) {
	tests := []struct {
		name     string
		granted  Scopes
		owner    Role
		required Scope
		want     bool
	}{
		{"operator token send", Scopes{ScopeSMSSend}, RoleOperator, ScopeSMSSend, true},
		{"viewer owner cannot send", Scopes{ScopeSMSSend}, RoleViewer, ScopeSMSSend, false},
		{"viewer owner can read", Scopes{ScopeSMSRead}, RoleViewer, ScopeSMSRead, true},
		{"admin token of operator is not admin", Scopes{ScopeAdmin}, RoleOperator, ScopeAdmin, false},
		{"admin token of operator still sends", Scopes{ScopeAdmin}, RoleOperator, ScopeSMSSend, true},
		{"admin token of admin", Scopes{ScopeAdmin}, RoleAdmin, ScopeAdmin, true},
		{"admin owner, narrow token", Scopes{ScopeSMSRead}, RoleAdmin, ScopeSMSSend, false},
		{"invalid owner", Scopes{ScopeAdmin}, "", ScopeSMSRead, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TokenAllows(tt.granted, tt.owner, tt.required); got != tt.want {
				t.Fatalf("TokenAllows = %v, want %v", got, tt.want)
			}
		})
	}
}
