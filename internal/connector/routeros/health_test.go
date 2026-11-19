// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package routeros

import (
	"errors"
	"testing"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
)

func intp(v int) *int { return &v }

func TestHealth(t *testing.T) {
	tests := []struct {
		name     string
		edit     func(*fake.LTEState)
		want     connector.ChannelHealth
		monitors int
	}{
		{"registered home", nil, connector.ChannelHealth{
			Running: true, Registered: true, Registration: "home", Operator: "Fake Mobile PL", Technology: "LTE",
			Signal: &domain.Signal{RSSI: intp(-67), RSRP: intp(-95), RSRQ: intp(-10), SINR: intp(12)},
		}, 1},
		{"roaming", func(s *fake.LTEState) { s.Registration = "roaming"; s.Operator = "Other" }, connector.ChannelHealth{
			Running: true, Registered: true, Registration: "roaming", Operator: "Other", Technology: "LTE",
			Signal: &domain.Signal{RSSI: intp(-67), RSRP: intp(-95), RSRQ: intp(-10), SINR: intp(12)},
		}, 1},
		{"searching without signal", func(s *fake.LTEState) {
			s.Registration, s.Operator, s.AccessTechnology, s.Signal = "searching", "", "", nil
		}, connector.ChannelHealth{Running: true, Registration: "searching"}, 1},
		{"interface down", func(s *fake.LTEState) { s.Running = false }, connector.ChannelHealth{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, setup{})
			if tt.edit != nil {
				e.srv.SetLTE("lte2", tt.edit)
			}
			got, err := e.conn.Health(t.Context(), lte2)
			if err != nil {
				t.Fatalf("Health: %v", err)
			}
			tt.want.CheckedAt = tNow
			if !healthEqual(got, tt.want) {
				t.Errorf("Health = %+v (signal %+v), want %+v", got, got.Signal, tt.want)
			}
			if n := e.srv.Requests(fake.EndpointLTEMonitor); n != tt.monitors {
				t.Errorf("monitor calls = %d, want %d", n, tt.monitors)
			}
		})
	}
}

func healthEqual(a, b connector.ChannelHealth) bool {
	if a.CheckedAt != b.CheckedAt || a.Running != b.Running || a.Registered != b.Registered ||
		a.Registration != b.Registration || a.Operator != b.Operator || a.Technology != b.Technology {
		return false
	}
	if (a.Signal == nil) != (b.Signal == nil) {
		return false
	}
	if a.Signal == nil {
		return true
	}
	eq := func(x, y *int) bool { return (x == nil && y == nil) || (x != nil && y != nil && *x == *y) }
	return eq(a.Signal.RSSI, b.Signal.RSSI) && eq(a.Signal.RSRP, b.Signal.RSRP) &&
		eq(a.Signal.RSRQ, b.Signal.RSRQ) && eq(a.Signal.SINR, b.Signal.SINR)
}

func TestHealthErrors(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*env)
		port    string
		kind    domain.ErrorKind
		code    domain.ErrorCode
		is      error
	}{
		{name: "unknown port", port: "lte9", kind: domain.KindPermanent, is: ErrUnknownPort},
		{name: "401", prepare: func(e *env) { e.srv.FailNext(fake.EndpointLTE, fake.Unauthorized(), 1) }, kind: domain.KindAuth, code: domain.CodeAuthFailed},
		{name: "list not an array", prepare: func(e *env) { e.srv.FailNext(fake.EndpointLTE, fake.HTTPError(200, ""), 1) }, kind: domain.KindTransient, code: domain.CodeUnexpectedResponse},
		{name: "monitor 500", prepare: func(e *env) { e.srv.FailNext(fake.EndpointLTEMonitor, fake.HTTPError(500, ""), 1) }, kind: domain.KindTransient},
		{name: "monitor not allowed", prepare: func(e *env) { e.srv.FailNext(fake.EndpointLTEMonitor, fake.NotAllowed(), 1) }, kind: domain.KindAuth, code: domain.CodeInsufficientPolicy},
		{name: "monitor not an array", prepare: func(e *env) { e.srv.FailNext(fake.EndpointLTEMonitor, fake.HTTPError(200, ""), 1) }, kind: domain.KindTransient, code: domain.CodeUnexpectedResponse},
		{name: "unreachable", prepare: func(e *env) { e.srv.FailNext(fake.EndpointLTE, fake.DropConnection(), 1) }, kind: domain.KindTransient, code: domain.CodeUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, setup{})
			if tt.prepare != nil {
				tt.prepare(e)
			}
			ch := lte1
			if tt.port != "" {
				ch.Port = tt.port
			}
			_, err := e.conn.Health(t.Context(), ch)
			wantErr(t, err, tt.kind, false, tt.code)
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("err = %v, want %v", err, tt.is)
			}
		})
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want *int
	}{
		{"-67", intp(-67)},
		{"-67dBm", intp(-67)},
		{" -10.5dB", intp(-11)},
		{"12", intp(12)},
		{"+3", intp(3)},
		{"", nil},
		{"n/a", nil},
		{"--", nil},
		{"99999", nil},
	}
	for _, tt := range tests {
		got := parseLevel(tt.in)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("parseLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
