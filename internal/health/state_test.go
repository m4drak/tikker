// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/queue"
)

const (
	ok       = domain.ChannelOK
	degraded = domain.ChannelDegraded
	down     = domain.ChannelDown
	disabled = domain.ChannelDisabled
)

// The first character of every sequence primes the channel (first checks
// skip the hysteresis).
func TestHysteresis(t *testing.T) {
	tests := []struct {
		name string
		seq  string
		want []domain.ChannelState // state changes after priming
	}{
		{"two failures are tolerated", "gbb", nil},
		{"three failures take it down", "gbbb", []domain.ChannelState{down}},
		{"transient errors count too", "geee", []domain.ChannelState{down}},
		{"a success resets the failure streak", "gbbgbbg", nil},
		{"one success is not enough to recover", "gbbbg", []domain.ChannelState{down}},
		{"two successes recover", "gbbbgg", []domain.ChannelState{down, ok}},
		{"a failure resets the recovery streak", "gbbbgbgg", []domain.ChannelState{down, ok}},
		{"recovery streak broken twice", "gbbbgbgbgg", []domain.ChannelState{down, ok}},
		{"flapping never goes down", "gbgbgbgbgb", nil},
		{"down stays down while failing", "gbbbbbbbb", []domain.ChannelState{down}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envConfig{})
			conn := newStub(script(tt.seq))
			c := e.primed(conn)
			for range len(tt.seq) - 1 {
				e.check(c.ID)
			}
			got := e.chg.states()
			if len(got) != len(tt.want) {
				t.Fatalf("changes = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("changes = %v, want %v", got, tt.want)
				}
			}
			last := ok
			if len(tt.want) > 0 {
				last = tt.want[len(tt.want)-1]
			}
			if st := e.status(c.ID).State; st != last {
				t.Fatalf("state = %s, want %s", st, last)
			}
			if db := e.stored(c.ID).State; db != last {
				t.Fatalf("stored state = %s, want %s", db, last)
			}
		})
	}
}

func TestThresholdsAreConfigurable(t *testing.T) {
	e := newEnv(t, envConfig{opts: func(o *Options) { o.FailThreshold, o.RecoverThreshold = 1, 3 }})
	c := e.primed(newStub(script("gbggg")))
	if st := e.check(c.ID); st.State != down {
		t.Fatalf("one failure: %s", st.State)
	}
	for i := 1; i <= 2; i++ {
		if st := e.check(c.ID); st.State != down || st.ConsecutiveSuccesses != i {
			t.Fatalf("recovery %d: %+v", i, st)
		}
	}
	if st := e.check(c.ID); st.State != ok {
		t.Fatalf("three successes: %s", st.State)
	}
}

func TestFirstCheckSkipsHysteresis(t *testing.T) {
	tests := []struct {
		name   string
		stored domain.ChannelState
		seq    string
		want   domain.ChannelState
	}{
		{"stored ok, device down", ok, "b", down},
		{"stored ok, unreachable", ok, "e", down},
		{"stored down, device fine", down, "g", ok},
		{"stored degraded, device fine", degraded, "g", ok},
		{"stored ok, device fine", ok, "g", ok},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envConfig{})
			c := e.channel("dev", "lte1", tt.stored)
			e.hc.SetChannels([]ChannelSpec{spec(c, newStub(script(tt.seq)))})
			if st := e.check(c.ID); st.State != tt.want || !st.Checked {
				t.Fatalf("state = %s (checked %v), want %s", st.State, st.Checked, tt.want)
			}
			if got := e.stored(c.ID).State; got != tt.want {
				t.Fatalf("stored = %s", got)
			}
		})
	}
}

func TestReasons(t *testing.T) {
	tests := []struct {
		name string
		h    connector.ChannelHealth
		err  error
		want string
	}{
		{"searching", badHealth("searching"), nil, "not registered to a network (searching)"},
		{"denied", badHealth("Denied"), nil, "not registered to a network (denied)"},
		{"unknown registration", badHealth(""), nil, "not registered to a network"},
		{"hostile registration text", badHealth("<b>x</b>; DROP"), nil, "not registered to a network (bxb drop)"},
		{"no sim", badHealth("no-sim"), nil, "no SIM card"},
		{"no sim spelled out", badHealth("No SIM inserted"), nil, "no SIM card"},
		{"stopped interface", connector.ChannelHealth{Running: false}, nil, "interface not running"},
		{"timeout", connector.ChannelHealth{}, domain.WithCode(domain.Transient("h", errors.New("x")), domain.CodeTimeout),
			"health check timed out"},
		{"unreachable", connector.ChannelHealth{}, domain.WithCode(domain.Transient("h", errors.New("x")), domain.CodeUnreachable),
			"health check failed: unreachable"},
		{"permanent", connector.ChannelHealth{}, domain.Permanent("h", errors.New("unknown port")),
			"health check failed (permanent)"},
		{"unclassified", connector.ChannelHealth{}, errors.New("boom"), "health check failed (transient)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envConfig{})
			c := e.channel("dev", "lte1", ok)
			conn := newStub(func(context.Context, int) (connector.ChannelHealth, error) { return tt.h, tt.err })
			e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
			st := e.check(c.ID)
			if st.State != down || st.Reason != tt.want {
				t.Fatalf("state %s reason %q, want down %q", st.State, st.Reason, tt.want)
			}
			if got := e.stored(c.ID); got.State != down || got.StateReason != tt.want {
				t.Fatalf("stored %s %q", got.State, got.StateReason)
			}
		})
	}
}

func TestNoConnectorIsDown(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	e.hc.SetChannels([]ChannelSpec{spec(c, nil)})
	if st := e.check(c.ID); st.State != down || st.Reason != reasonNoConnector {
		t.Fatalf("%+v", st)
	}
}

func TestDownDataIsRecorded(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	h := badHealth("searching")
	h.Operator = "Plus"
	h.Signal = sig(-101, -120, -18, -3)
	conn := newStub(func(context.Context, int) (connector.ChannelHealth, error) { return h, nil })
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	st := e.check(c.ID)
	if st.Operator != "Plus" || st.Signal == nil || *st.Signal.RSSI != -100 {
		t.Fatalf("%+v", st)
	}
	// An unreachable device has no signal.
	conn.hfn = func(context.Context, int) (connector.ChannelHealth, error) {
		return connector.ChannelHealth{}, errTransient
	}
	e.check(c.ID)
	e.check(c.ID)
	e.check(c.ID)
	st = e.status(c.ID)
	if st.Signal != nil || st.Operator != "Plus" {
		t.Fatalf("after outage: %+v", st)
	}
}

func TestReasonChangeWhileDown(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	conn := newStub(func(_ context.Context, n int) (connector.ChannelHealth, error) {
		if n == 1 {
			return badHealth("searching"), nil
		}
		return badHealth("no-sim"), nil
	})
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	e.check(c.ID)
	e.check(c.ID)
	if got := e.stored(c.ID); got.State != down || got.StateReason != reasonNoSIM {
		t.Fatalf("stored %s %q", got.State, got.StateReason)
	}
	if n := len(e.chg.all()); n != 1 {
		t.Fatalf("a new reason is not a state change: %d changes", n)
	}
}

// ---------------------------------------------------------------------------
// Auth

type authSource struct {
	name string
	fire func(e *env, id domain.ID)
	want string
}

func authSources() []authSource {
	return []authSource{
		{"health", func(e *env, id domain.ID) {
			e.hc.SetChannels([]ChannelSpec{spec(e.stored(id), newStub(script("a")))})
			e.check(id)
		}, "authentication failed (health check)"},
		{"queue ReportSend", func(e *env, id domain.ID) { e.hc.ReportSend(id, errAuth) }, "authentication failed (send)"},
		{"inbox ReportInbox", func(e *env, id domain.ID) { e.hc.ReportInbox(id, errAuth) }, "authentication failed (inbox)"},
		{"queue event", func(e *env, id domain.ID) {
			e.hc.HandleQueueEvent(queue.Event{Kind: queue.EventAuthFailed, ChannelID: id, ErrorKind: domain.KindAuth, Err: errAuth})
		}, "authentication failed (send)"},
		{"wrapped auth error", func(e *env, id domain.ID) {
			e.hc.ReportSend(id, errors.Join(errors.New("ctx"), errAuth))
		}, "authentication failed (send)"},
	}
}

func TestAuthDegradesAtOnce(t *testing.T) {
	for _, src := range authSources() {
		t.Run(src.name, func(t *testing.T) {
			e := newEnv(t, envConfig{})
			conn := newStub(script("g"))
			c := e.primed(conn)
			src.fire(e, c.ID)
			e.flush()
			st := e.status(c.ID)
			if st.State != degraded || st.Reason != src.want || !st.AuthPending {
				t.Fatalf("status %+v", st)
			}
			if got := e.stored(c.ID); got.State != degraded || got.StateReason != src.want {
				t.Fatalf("stored %s %q", got.State, got.StateReason)
			}
			last := e.disp.all()[len(e.disp.all())-1]
			if last.ID != c.ID || last.State != degraded || last.Reason != src.want {
				t.Fatalf("dispatcher got %+v", last)
			}
			if got := e.chg.states(); len(got) != 1 || got[0] != degraded {
				t.Fatalf("changes %v", got)
			}
		})
	}
}

func TestAuthRepeatedReportsAreQuiet(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.primed(newStub(script("g")))
	e.hc.ReportSend(c.ID, errAuth)
	e.flush()
	updates, pushes := e.repo.count(), len(e.disp.all())
	e.hc.ReportInbox(c.ID, errAuth)
	e.hc.HandleQueueEvent(queue.Event{Kind: queue.EventAuthFailed, ChannelID: c.ID})
	e.flush()
	if e.repo.count() != updates || len(e.disp.all()) != pushes {
		t.Fatal("repeated Auth reports caused writes or pushes")
	}
	if st := e.status(c.ID); st.Reason != "authentication failed (send)" {
		t.Fatalf("first source should win: %q", st.Reason)
	}
}

func TestAuthRecovery(t *testing.T) {
	probeErr := func(err error) func(context.Context) (connector.DeviceProbe, error) {
		return func(context.Context) (connector.DeviceProbe, error) { return connector.DeviceProbe{}, err }
	}
	tests := []struct {
		name      string
		probe     func(context.Context) (connector.DeviceProbe, error)
		seq       string // Health answers after the Auth error
		wantState domain.ChannelState
		wantAuth  bool
	}{
		{"health and probe ok", nil, "gg", ok, false},
		{"probe says auth", probeErr(errAuth), "gg", degraded, true},
		{"probe unreachable", probeErr(errTransient), "gg", degraded, true},
		{"health still unauthorized", nil, "aa", degraded, true},
		{"health ok but modem down", nil, "bb", degraded, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envConfig{})
			conn := newStub(script("g" + tt.seq))
			conn.setProbe(tt.probe)
			c := e.primed(conn)
			e.hc.ReportSend(c.ID, errAuth)
			e.flush()
			e.check(c.ID)
			st := e.status(c.ID)
			if st.State != tt.wantState || st.AuthPending != tt.wantAuth {
				t.Fatalf("status %+v, want %s auth=%v", st, tt.wantState, tt.wantAuth)
			}
			if tt.wantState == ok {
				if !contains(e.disp.states(), ok) {
					t.Fatalf("dispatcher never got ok: %v", e.disp.states())
				}
				if got := e.stored(c.ID).State; got != ok {
					t.Fatalf("stored %s", got)
				}
			}
		})
	}
}

func TestAuthRecoveryRequiresProbe(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(script("g"))
	c := e.primed(conn)
	if _, probes := conn.calls(); probes != 0 {
		t.Fatalf("a healthy channel must not be probed, got %d", probes)
	}
	e.hc.ReportInbox(c.ID, errAuth)
	e.check(c.ID)
	if _, probes := conn.calls(); probes != 1 {
		t.Fatalf("probes = %d", probes)
	}
	if st := e.status(c.ID); st.State != ok {
		t.Fatalf("%+v", st)
	}
	e.check(c.ID)
	if _, probes := conn.calls(); probes != 1 {
		t.Fatalf("recovered channel was probed again: %d", probes)
	}
}

func TestAuthThenOutageThenRecovery(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(script("geeeggg"))
	c := e.primed(conn)
	e.hc.ReportSend(c.ID, errAuth)
	for range 3 {
		e.check(c.ID)
	}
	st := e.status(c.ID)
	if st.State != down || !st.AuthPending {
		t.Fatalf("after outage: %+v", st)
	}
	e.check(c.ID)
	if st := e.status(c.ID); st.State != ok || st.AuthPending {
		t.Fatalf("auth recovery needs one good Health + Probe: %+v", st)
	}
	if got := e.chg.states(); !equalStates(got, []domain.ChannelState{degraded, down, ok}) {
		t.Fatalf("changes %v", got)
	}
}

func TestAuthDuringCheckBlocksRecovery(t *testing.T) {
	e := newEnv(t, envConfig{})
	g := newGate()
	conn := newStub(func(ctx context.Context, n int) (connector.ChannelHealth, error) {
		if n == 1 {
			return goodHealth(), nil
		}
		return g.fn(ctx, n)
	})
	c := e.primed(conn)
	done := make(chan ChannelStatus, 1)
	go func() {
		st, _ := e.hc.CheckNow(e.ctx, c.ID)
		done <- st
	}()
	g.waitEntered(t)
	e.hc.ReportSend(c.ID, errAuth) // arrives while Health is running
	g.release <- goodHealth()
	st := <-done
	if st.State != degraded || !st.AuthPending {
		t.Fatalf("a Health that started before the Auth error must not clear it: %+v", st)
	}
}

func TestReportsFromTransientAndSuccess(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.primed(newStub(script("g")))
	updates := e.repo.count()
	for _, err := range []error{errTransient, nil, domain.Permanent("send", errors.New("bad number")), errors.New("x")} {
		e.hc.ReportSend(c.ID, err)
		e.hc.ReportInbox(c.ID, err)
	}
	e.hc.ReportSend("no-such-channel", errAuth)
	e.flush()
	if st := e.status(c.ID); st.State != ok || st.AuthPending {
		t.Fatalf("%+v", st)
	}
	if e.repo.count() != updates {
		t.Fatal("non-Auth reports must not write")
	}
}

func TestQueueEvents(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.primed(newStub(script("gbb")))
	e.check(c.ID)
	if st := e.status(c.ID); st.ConsecutiveFailures != 1 {
		t.Fatalf("%+v", st)
	}
	// A successful send proves the modem is alive.
	e.hc.HandleQueueEvent(queue.Event{Kind: queue.EventSent, ChannelID: c.ID})
	if st := e.status(c.ID); st.ConsecutiveFailures != 0 {
		t.Fatalf("%+v", st)
	}
	e.hc.HandleQueueEvent(queue.Event{Kind: queue.EventSent, ChannelID: "unknown"})
	e.hc.HandleQueueEvent(queue.Event{Kind: queue.EventSendFailed, ChannelID: c.ID, ErrorKind: domain.KindPermanent})
	e.hc.HandleQueueEvent(queue.Event{Kind: queue.EventSendFailed, ChannelID: c.ID, ErrorKind: domain.KindTransient, Err: errTransient})
	if st := e.status(c.ID); st.State != ok {
		t.Fatalf("send failures are only hints: %+v", st)
	}
}

// ---------------------------------------------------------------------------
// Propagation

func TestPropagation(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(script("gbbbgg"))
	c := e.primed(conn)
	for range 5 {
		e.check(c.ID)
	}
	want := []pushCall{
		{c.ID, down, "not registered to a network (searching)"},
		{c.ID, ok, ""},
	}
	got := e.disp.all()
	if len(got) != len(want) {
		t.Fatalf("dispatcher calls %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	chs := e.chg.all()
	if len(chs) != 2 || chs[0].From != ok || chs[0].To != down || chs[0].ChannelID != c.ID ||
		chs[0].DeviceID != c.DeviceID || chs[0].Reason != want[0].Reason || chs[0].At.IsZero() ||
		chs[1].From != down || chs[1].To != ok {
		t.Fatalf("changes %+v", chs)
	}
}

func TestDispatcherUnknownChannelIsRetried(t *testing.T) {
	e := newEnv(t, envConfig{})
	e.disp.setReject(true)
	c := e.primed(newStub(script("gbbbbb")))
	e.check(c.ID)
	e.check(c.ID)
	e.check(c.ID) // down; the dispatcher refuses
	n := len(e.disp.all())
	if n == 0 {
		t.Fatal("no push attempted")
	}
	e.check(c.ID)
	if len(e.disp.all()) <= n {
		t.Fatal("the push was not retried at the next check")
	}
	e.disp.setReject(false)
	e.check(c.ID)
	n = len(e.disp.all())
	e.check(c.ID)
	e.check(c.ID)
	if len(e.disp.all()) != n {
		t.Fatal("pushes continue after the dispatcher accepted the state")
	}
}

func TestStoreFailureIsRetriedAndLoggedOnce(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.primed(newStub(script("gbbbbbbbb")))
	e.repo.setFail(errors.New("disk full"))
	for range 4 {
		e.check(c.ID) // 3rd flips to down, the store refuses every write
	}
	if got := e.stored(c.ID).State; got != ok {
		t.Fatalf("stored %s", got)
	}
	if n := strings.Count(e.logs.String(), "storing channel health failed"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	e.repo.setFail(nil)
	e.check(c.ID)
	if got := e.stored(c.ID).State; got != down {
		t.Fatalf("stored %s after the store recovered", got)
	}
	if !strings.Contains(e.logs.String(), "storing channel health works again") {
		t.Fatal("recovery not logged")
	}
}

func TestStoreNotFoundIsNotRetried(t *testing.T) {
	e := newEnv(t, envConfig{})
	e.repo.setFail(domain.ErrNotFound)
	c := e.primed(newStub(script("g")))
	n := e.repo.count()
	e.check(c.ID)
	e.check(c.ID)
	if e.repo.count() != n {
		t.Fatalf("UpdateHealth called %d more times for a missing row", e.repo.count()-n)
	}
}

func TestLogsDoNotSpam(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.primed(newStub(script("g" + strings.Repeat("e", 30))))
	for range 30 {
		e.check(c.ID)
	}
	logs := e.logs.String()
	if n := strings.Count(logs, "channel state changed"); n != 1 {
		t.Fatalf("state change logged %d times:\n%s", n, logs)
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=WARN") && !strings.Contains(line, "channel state changed") {
			t.Fatalf("unexpected warning: %s", line)
		}
	}
	for _, secret := range []string{"password", "token"} {
		if strings.Contains(strings.ToLower(logs), secret) {
			t.Fatalf("log mentions %q", secret)
		}
	}
}

// ---------------------------------------------------------------------------
// Disabled channels, SetChannels

func TestDisabledChannel(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(script("g"))
	c := e.channel("dev", "lte1", ok, func(c *domain.Channel) { c.Enabled = false })
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	e.flush()
	if st := e.status(c.ID); st.State != disabled || st.Enabled {
		t.Fatalf("%+v", st)
	}
	if got := e.stored(c.ID); got.State != disabled {
		t.Fatalf("stored %s", got.State)
	}
	if got := e.disp.states(); !equalStates(got, []domain.ChannelState{disabled}) {
		t.Fatalf("dispatcher %v", got)
	}
	if st := e.check(c.ID); st.State != disabled {
		t.Fatalf("%+v", st)
	}
	e.hc.ReportSend(c.ID, errAuth) // ignored
	if st := e.status(c.ID); st.State != disabled {
		t.Fatalf("%+v", st)
	}
	if h, p := conn.calls(); h != 0 || p != 0 {
		t.Fatalf("a disabled channel was contacted: %d/%d", h, p)
	}

	// Enabling: not usable until the first check succeeded.
	c.Enabled = true
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	e.flush()
	if st := e.status(c.ID); st.State != down || st.Reason != reasonAwaiting {
		t.Fatalf("after enabling: %+v", st)
	}
	if st := e.check(c.ID); st.State != ok {
		t.Fatalf("first check: %+v", st)
	}
	// Disabling again.
	c.Enabled = false
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	e.flush()
	if got := e.stored(c.ID).State; got != disabled {
		t.Fatalf("stored %s", got)
	}
	if got := e.chg.states(); !equalStates(got, []domain.ChannelState{down, ok, disabled}) {
		t.Fatalf("changes %v", got)
	}
}

func TestEnabledChannelStoredAsDisabled(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", disabled)
	e.hc.SetChannels([]ChannelSpec{spec(c, newStub(script("g")))})
	if st := e.status(c.ID); st.State != down || st.Reason != reasonAwaiting {
		t.Fatalf("%+v", st)
	}
	if st := e.check(c.ID); st.State != ok {
		t.Fatalf("%+v", st)
	}
}

func TestSetChannelsKeepsStateAndForgetsRemoved(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(script("gbbb"))
	c := e.primed(conn)
	e.check(c.ID)
	e.check(c.ID)
	// The same channel again, as read from the store (stale state): the live
	// state and streaks win.
	e.hc.SetChannels([]ChannelSpec{spec(e.stored(c.ID), conn)})
	if st := e.status(c.ID); st.ConsecutiveFailures != 2 || st.State != ok {
		t.Fatalf("%+v", st)
	}
	e.hc.SetChannels(nil)
	if _, known := e.hc.Status(c.ID); known {
		t.Fatal("removed channel still known")
	}
	if _, err := e.hc.CheckNow(e.ctx, c.ID); !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("CheckNow = %v", err)
	}
	if len(e.hc.Snapshot()) != 0 {
		t.Fatal("snapshot not empty")
	}
}

func TestApplyTo(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	e.hc.SetChannels([]ChannelSpec{spec(c, newStub(script("b")))})
	e.check(c.ID)
	stale := e.stored(c.ID)
	stale.State = ok
	stale.StateReason = ""
	if !e.hc.ApplyTo(&stale) || stale.State != down || stale.StateReason == "" || stale.Operator != "" {
		t.Fatalf("%+v", stale)
	}
	if stale.LastHealthAt.IsZero() {
		t.Fatal("LastHealthAt not applied")
	}
	other := domain.Channel{ID: "other", State: ok}
	if e.hc.ApplyTo(&other) || other.State != ok {
		t.Fatal("unknown channel must be left alone")
	}
}

func TestSnapshot(t *testing.T) {
	e := newEnv(t, envConfig{})
	a := e.channel("d1", "lte1", ok)
	b := e.channel("d2", "lte1", ok)
	e.hc.SetChannels([]ChannelSpec{spec(b, newStub(script("e"))), spec(a, newStub(script("g")))})
	e.check(a.ID)
	e.clk.Advance(time.Minute)
	e.check(b.ID)
	snap := e.hc.Snapshot()
	if len(snap) != 2 || snap[0].ChannelID >= snap[1].ChannelID {
		t.Fatalf("snapshot not sorted: %+v", snap)
	}
	sa, sb := e.status(a.ID), e.status(b.ID)
	if !sa.Usable() || sb.Usable() {
		t.Fatalf("Usable: %v %v", sa.Usable(), sb.Usable())
	}
	if sa.Operator != "Orange" || sa.Technology != "LTE" || sa.Signal == nil || *sa.Signal.RSSI != -65 ||
		!sa.LastSuccessAt.Equal(t0) || !sa.LastCheckAt.Equal(t0) || sa.LastError != "" || sa.Port != "lte1" ||
		sa.DeviceID != a.DeviceID {
		t.Fatalf("status a: %+v", sa)
	}
	if sb.State != down || !sb.LastFailureAt.Equal(t0.Add(time.Minute)) || sb.LastError == "" ||
		sb.ConsecutiveFailures != 1 {
		t.Fatalf("status b: %+v", sb)
	}
	// The snapshot is a copy.
	*sa.Signal.RSSI = 1
	if *e.status(a.ID).Signal.RSSI != -65 {
		t.Fatal("Snapshot exposes internal state")
	}
}

func TestOptionsValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("Channels is required")
	}
	e := newEnv(t, envConfig{})
	for name, mod := range map[string]func(*Options){
		"interval":  func(o *Options) { o.Interval = -1 },
		"retry":     func(o *Options) { o.RetryInterval = -1 },
		"timeout":   func(o *Options) { o.Timeout = -1 },
		"refresh":   func(o *Options) { o.RefreshInterval = -1 },
		"fail":      func(o *Options) { o.FailThreshold = -1 },
		"recover":   func(o *Options) { o.RecoverThreshold = -1 },
		"authreco.": func(o *Options) { o.AuthRecoverThreshold = -1 },
	} {
		o := Options{Channels: e.repo}
		mod(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: negative value accepted", name)
		}
	}
	hc, err := New(Options{Channels: e.repo})
	if err != nil {
		t.Fatal(err)
	}
	o := hc.opts
	if o.Interval != DefaultInterval || o.RetryInterval != DefaultRetryInterval || o.Timeout != DefaultTimeout ||
		o.FailThreshold != DefaultFailThreshold || o.RecoverThreshold != DefaultRecoverThreshold ||
		o.AuthRecoverThreshold != DefaultAuthRecoverThreshold || o.RefreshInterval != DefaultRefreshInterval ||
		o.Quantization != (Quantization{DefaultStepRSSI, DefaultStepRSRP, DefaultStepRSRQ, DefaultStepSINR}) {
		t.Fatalf("defaults: %+v", o)
	}
}

func contains(s []domain.ChannelState, v domain.ChannelState) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func equalStates(a, b []domain.ChannelState) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
