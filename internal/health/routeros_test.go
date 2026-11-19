// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package health

import (
	"log/slog"
	"testing"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/connector/routeros"
	"github.com/m4drak/tikker/internal/domain"
)

// rosEnv wires the checker to the real RouterOS connector talking to the
// fake router.
type rosEnv struct {
	*env
	srv  *fake.Server
	conn connector.Connector
	ch   domain.Channel
}

func newRosEnv(t *testing.T) *rosEnv {
	t.Helper()
	e := newEnv(t, envConfig{})
	srv := fake.New(fake.Options{User: "tikker", Password: "pw", Ports: []string{"lte1", "lte2"}})
	url := srv.Start()
	t.Cleanup(srv.Close)
	conn, err := routeros.NewConn(e.ctx, connector.DeviceConfig{
		DeviceID: "dev", Kind: domain.ConnectorRouterOSREST, BaseURL: url,
		Username: "tikker", Password: "pw", TLSMode: domain.TLSVerify,
	}, routeros.Options{Clock: e.clk, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch := e.channel("dev", "lte1", ok)
	e.hc.SetChannels([]ChannelSpec{spec(ch, conn)})
	return &rosEnv{env: e, srv: srv, conn: conn, ch: ch}
}

func TestRouterOSHealthy(t *testing.T) {
	r := newRosEnv(t)
	st := r.check(r.ch.ID)
	if st.State != ok || st.Operator != "Fake Mobile PL" || st.Technology != "LTE" {
		t.Fatalf("%+v", st)
	}
	// Fake: RSSI -67, RSRP -95, RSRQ -10, SINR 12.
	if !equalSignal(st.Signal, sig(-65, -95, -10, 12)) {
		t.Fatalf("signal %+v", st.Signal)
	}
	got := r.stored(r.ch.ID)
	if got.State != ok || got.Operator != "Fake Mobile PL" || !equalSignal(got.Signal, st.Signal) {
		t.Fatalf("stored %+v", got)
	}
}

func TestRouterOSModemStates(t *testing.T) {
	tests := []struct {
		name string
		edit func(*fake.LTEState)
		want string
	}{
		{"searching", func(l *fake.LTEState) { l.Registration = "searching" }, "not registered to a network (searching)"},
		{"denied", func(l *fake.LTEState) { l.Registration = "denied" }, "not registered to a network (denied)"},
		{"no sim", func(l *fake.LTEState) { l.Registration = "no-sim" }, "no SIM card"},
		{"interface stopped", func(l *fake.LTEState) { l.Running = false }, "interface not running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRosEnv(t)
			if st := r.check(r.ch.ID); st.State != ok {
				t.Fatalf("%+v", st)
			}
			r.srv.SetLTE("lte1", tt.edit)
			for i := 1; i <= 2; i++ { // hysteresis: still ok
				if st := r.check(r.ch.ID); st.State != ok || st.ConsecutiveFailures != i {
					t.Fatalf("check %d: %+v", i, st)
				}
			}
			st := r.check(r.ch.ID)
			if st.State != down || st.Reason != tt.want {
				t.Fatalf("%+v, want down %q", st, tt.want)
			}
			if got := r.stored(r.ch.ID); got.State != down || got.StateReason != tt.want {
				t.Fatalf("stored %s %q", got.State, got.StateReason)
			}
			// Back to normal: two good checks.
			r.srv.SetLTE("lte1", func(l *fake.LTEState) { l.Registration, l.Running = "home", true })
			r.check(r.ch.ID)
			if st := r.check(r.ch.ID); st.State != ok || st.Reason != "" {
				t.Fatalf("%+v", st)
			}
		})
	}
}

func TestRouterOSUnreachable(t *testing.T) {
	r := newRosEnv(t)
	r.check(r.ch.ID)
	r.srv.FailNext(fake.EndpointLTE, fake.DropConnection(), 10)
	for range 3 {
		r.check(r.ch.ID)
	}
	if st := r.status(r.ch.ID); st.State != down || st.Reason == "" {
		t.Fatalf("%+v", st)
	}
}

func TestRouterOSAuthLifecycle(t *testing.T) {
	r := newRosEnv(t)
	r.check(r.ch.ID)

	// Wrong credentials on the router: degraded at once, no hysteresis.
	r.srv.SetCredentials("tikker", "changed")
	st := r.check(r.ch.ID)
	if st.State != degraded || !st.AuthPending {
		t.Fatalf("%+v", st)
	}
	if got := r.stored(r.ch.ID); got.State != degraded {
		t.Fatalf("stored %s", got.State)
	}
	if got := r.disp.states(); got[len(got)-1] != degraded {
		t.Fatalf("dispatcher %v", got)
	}
	if st := r.check(r.ch.ID); st.State != degraded {
		t.Fatalf("still unauthorized: %+v", st)
	}

	// Credentials fixed: one good Health + Probe brings it back.
	r.srv.SetCredentials("tikker", "pw")
	st = r.check(r.ch.ID)
	if st.State != ok || st.AuthPending {
		t.Fatalf("%+v", st)
	}
	if got := r.stored(r.ch.ID); got.State != ok {
		t.Fatalf("stored %s", got.State)
	}
	if got := r.chg.states(); !equalStates(got, []domain.ChannelState{degraded, ok}) {
		t.Fatalf("changes %v", got)
	}
}

func TestRouterOSInsufficientPolicy(t *testing.T) {
	r := newRosEnv(t)
	r.check(r.ch.ID)
	r.srv.SetPolicies(fake.Policies{RESTAPI: false, API: true}) // REST API not allowed: 401
	if st := r.check(r.ch.ID); st.State != degraded {
		t.Fatalf("%+v", st)
	}
	r.srv.SetPolicies(fake.Policies{RESTAPI: true, API: true})
	if st := r.check(r.ch.ID); st.State != ok {
		t.Fatalf("%+v", st)
	}
}

func TestRouterOSUnknownPort(t *testing.T) {
	r := newRosEnv(t)
	bad := r.channel("dev", "lte9", ok)
	r.hc.SetChannels([]ChannelSpec{spec(r.ch, r.conn), spec(bad, r.conn)})
	st := r.check(bad.ID)
	if st.State != down || st.Reason != "health check failed (permanent)" {
		t.Fatalf("%+v", st)
	}
	if st := r.check(r.ch.ID); st.State != ok {
		t.Fatalf("a bad port must not affect its sibling: %+v", st)
	}
}
