// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestConnectorErrorIs(t *testing.T) {
	cause := errors.New("dial tcp: timeout")
	tests := []struct {
		name string
		err  error
		is   error
		not  []error
	}{
		{"transient", Transient("send", cause), ErrTransient, []error{ErrPermanent, ErrAuth}},
		{"ambiguous", TransientAmbiguous("send", cause), ErrTransient, []error{ErrPermanent, ErrAuth}},
		{"permanent", Permanent("send", cause), ErrPermanent, []error{ErrTransient, ErrAuth}},
		{"auth", AuthError("health", cause), ErrAuth, []error{ErrTransient, ErrPermanent}},
		{"wrapped", fmt.Errorf("worker: %w", Permanent("send", cause)), ErrPermanent, []error{ErrTransient}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.is) {
				t.Errorf("errors.Is(%v, %v) = false", tt.err, tt.is)
			}
			if !errors.Is(tt.err, cause) {
				t.Errorf("cause not reachable through Unwrap")
			}
			for _, n := range tt.not {
				if errors.Is(tt.err, n) {
					t.Errorf("errors.Is(%v, %v) = true", tt.err, n)
				}
			}
			var ce *ConnectorError
			if !errors.As(tt.err, &ce) {
				t.Fatal("errors.As failed")
			}
		})
	}
}

func TestConnectorErrorMessage(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{Transient("send", errors.New("eof")), "connector send: transient: eof"},
		{TransientAmbiguous("send", errors.New("eof")), "connector send: transient (ambiguous): eof"},
		{AuthError("health", nil), "connector health: auth"},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}

func TestKindOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{"nil", nil, ""},
		{"transient", Transient("send", nil), KindTransient},
		{"permanent", Permanent("send", nil), KindPermanent},
		{"auth", AuthError("send", nil), KindAuth},
		{"wrapped auth", fmt.Errorf("x: %w", AuthError("send", nil)), KindAuth},
		{"bare sentinel permanent", fmt.Errorf("x: %w", ErrPermanent), KindPermanent},
		{"bare sentinel auth", ErrAuth, KindAuth},
		{"bare sentinel transient", ErrTransient, KindTransient},
		{"invalid kind falls through", &ConnectorError{Kind: "weird", Op: "send"}, KindTransient},
		{"unclassified", errors.New("boom"), KindTransient},
		{"context canceled", context.Canceled, KindTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KindOf(tt.err); got != tt.want {
				t.Fatalf("KindOf = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsAmbiguous(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"transient", Transient("send", nil), false},
		{"ambiguous", TransientAmbiguous("send", nil), true},
		{"wrapped ambiguous", fmt.Errorf("x: %w", TransientAmbiguous("send", nil)), true},
		{"connector error wrapping deadline is explicit", Transient("send", context.DeadlineExceeded), false},
		{"bare deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), true},
		{"bare canceled", context.Canceled, true},
		{"other", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAmbiguous(tt.err); got != tt.want {
				t.Fatalf("IsAmbiguous = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestErrorKindEnum(t *testing.T) {
	for _, k := range []ErrorKind{KindTransient, KindPermanent, KindAuth} {
		b, err := k.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		var got ErrorKind
		if err := got.UnmarshalText(b); err != nil || got != k {
			t.Fatalf("round trip %q -> %q, %v", k, got, err)
		}
		if p, err := ParseErrorKind(k.String()); err != nil || p != k {
			t.Fatalf("ParseErrorKind(%q) = %q, %v", k, p, err)
		}
	}
	if _, err := ErrorKind("x").MarshalText(); err == nil {
		t.Fatal("invalid kind marshalled")
	}
	var k ErrorKind
	if err := k.UnmarshalText([]byte("x")); err == nil {
		t.Fatal("invalid kind unmarshalled")
	}
}

func TestErrorCode(t *testing.T) {
	err := fmt.Errorf("probe: %w", WithCode(AuthError("probe", errors.New("401")), CodeAuthFailed))
	if got := CodeOf(err); got != CodeAuthFailed {
		t.Fatalf("CodeOf = %q", got)
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatal("WithCode lost the kind")
	}
	plain := errors.New("x")
	if WithCode(plain, CodeTimeout) != plain || CodeOf(plain) != "" {
		t.Fatal("unclassified error modified or coded")
	}
	if CodeOf(Transient("send", nil)) != "" {
		t.Fatal("uncoded connector error has a code")
	}
}
