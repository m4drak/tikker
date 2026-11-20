// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package health

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

func gkey(c domain.Channel) string { return groupKey(c) }

// settled waits until channel c finished n checks and its loop sleeps again.
func (e *env) settled(c domain.Channel, n int) {
	e.t.Helper()
	g := gkey(c)
	e.tr.wait(e.t, "check and sleep", func(v view) bool { return v.checks[c.ID] >= n && v.asleep(g) })
}

// settledFor is settled, and the loop sleeps for d.
func (e *env) settledFor(c domain.Channel, n int, d time.Duration) {
	e.t.Helper()
	g := gkey(c)
	e.tr.wait(e.t, "check and sleep "+d.String(), func(v view) bool {
		return v.checks[c.ID] >= n && v.sleeping[g] == d
	})
}

func TestLoopSchedule(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	// Good, then two failures (retry cadence), then good again.
	e.hc.SetChannels([]ChannelSpec{spec(c, newStub(script("gbbg")))})
	e.start()
	e.settledFor(c, 1, DefaultInterval) // healthy: every Interval
	e.clk.Advance(DefaultInterval)
	e.settledFor(c, 2, DefaultRetryInterval) // failing: faster
	e.clk.Advance(DefaultRetryInterval)
	e.settledFor(c, 3, DefaultRetryInterval)
	e.clk.Advance(DefaultRetryInterval)
	e.settledFor(c, 4, DefaultInterval) // recovered
	if st := e.status(c.ID); st.State != ok || st.ConsecutiveFailures != 0 || len(e.chg.all()) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestLoopDownChannelIsCheckedAtInterval(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	e.hc.SetChannels([]ChannelSpec{spec(c, newStub(script("eeeee")))})
	e.start()
	e.settledFor(c, 1, DefaultInterval) // first check: down at once, then polled every Interval
	if st := e.status(c.ID); st.State != down {
		t.Fatalf("%+v", st)
	}
}

// A health check that waits for a long send times out; the hysteresis keeps
// the channel ok (routeros README ASSUMPTION 9).
func TestLoopTimeoutDuringLongSend(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	g := newGate()
	conn := newStub(func(ctx context.Context, n int) (connector.ChannelHealth, error) {
		if n == 1 || n >= 4 {
			return goodHealth(), nil
		}
		return g.fn(ctx, n) // blocked behind a send until the timeout
	})
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	e.start()
	e.settled(c, 1)

	e.clk.Advance(DefaultInterval)
	g.waitEntered(t)
	e.clk.Advance(DefaultTimeout)
	e.settled(c, 2)
	st := e.status(c.ID)
	if st.State != ok || st.ConsecutiveFailures != 1 || st.LastError == "" {
		t.Fatalf("after one timeout: %+v", st)
	}

	e.clk.Advance(DefaultRetryInterval)
	g.waitEntered(t)
	e.clk.Advance(DefaultTimeout)
	e.settled(c, 3)
	if st := e.status(c.ID); st.State != ok || st.ConsecutiveFailures != 2 {
		t.Fatalf("after two timeouts: %+v", st)
	}

	e.clk.Advance(DefaultRetryInterval)
	e.settled(c, 4)
	if st := e.status(c.ID); st.State != ok || st.ConsecutiveFailures != 0 || st.LastError != "" {
		t.Fatalf("after the send finished: %+v", st)
	}
	if len(e.chg.all()) != 0 || len(e.disp.all()) != 0 {
		t.Fatalf("no state change expected: %v %v", e.chg.all(), e.disp.all())
	}
}

func TestLoopTimeoutsEventuallyTakeItDown(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	g := newGate()
	conn := newStub(func(ctx context.Context, n int) (connector.ChannelHealth, error) {
		if n == 1 {
			return goodHealth(), nil
		}
		return g.fn(ctx, n)
	})
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	e.start()
	e.settled(c, 1)
	e.clk.Advance(DefaultInterval)
	for i := 2; i <= 4; i++ {
		g.waitEntered(t)
		e.clk.Advance(DefaultTimeout)
		e.settled(c, i)
		if i < 4 {
			e.clk.Advance(DefaultRetryInterval)
		}
	}
	st := e.status(c.ID)
	if st.State != down || st.Reason != "health check timed out" {
		t.Fatalf("%+v", st)
	}
}

func TestLoopChecksDevicesInParallelAndChannelsInTurn(t *testing.T) {
	e := newEnv(t, envConfig{})
	g1, g2 := newGate(), newGate()
	d1, d2 := newStub(g1.fn), newStub(g2.fn)
	a := e.channel("d1", "lte1", ok)
	b := e.channel("d2", "lte1", ok)
	e.hc.SetChannels([]ChannelSpec{spec(a, d1), spec(b, d2)})
	e.start()
	// Both devices are inside Health at the same time.
	g1.waitEntered(t)
	g2.waitEntered(t)
	g1.release <- goodHealth()
	g2.release <- goodHealth()
	e.settled(a, 1)
	e.settled(b, 1)
}

func TestLoopNeverOverlapsChecksOfOneDevice(t *testing.T) {
	e := newEnv(t, envConfig{})
	conn := newStub(func(context.Context, int) (connector.ChannelHealth, error) {
		runtime.Gosched() // widen the window for an overlap
		return goodHealth(), nil
	})
	chs := []domain.Channel{
		e.channel("dev", "lte1", ok), e.channel("dev", "lte2", ok), e.channel("dev", "lte3", ok),
	}
	var specs []ChannelSpec
	for _, c := range chs {
		specs = append(specs, spec(c, conn))
	}
	e.hc.SetChannels(specs)
	e.start()
	for round := 1; round <= 3; round++ {
		for _, c := range chs {
			e.settled(c, round)
		}
		e.clk.Advance(DefaultInterval)
	}
	// A manual check shares the device lock with the loop.
	for _, c := range chs {
		e.check(c.ID)
	}
	if got := conn.maxConcurrent(); got != 1 {
		t.Fatalf("%d concurrent health requests to one device", got)
	}
}

func TestLoopReportBringsCheckForward(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	conn := newStub(script("g"))
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	e.start()
	e.settled(c, 1)
	g := gkey(c)
	sleeps := func() int {
		e.tr.mu.Lock()
		defer e.tr.mu.Unlock()
		return e.tr.sleeps[g]
	}
	n := sleeps()

	e.hc.ReportSend(c.ID, errTransient)
	e.tr.wait(t, "re-armed", func(v view) bool { return v.sleeps[g] > n && v.sleeping[g] == DefaultRetryInterval })
	e.clk.Advance(DefaultRetryInterval) // not a whole Interval
	e.settled(c, 2)
	if h, _ := conn.calls(); h != 2 {
		t.Fatalf("health calls = %d", h)
	}
}

func TestLoopSetChannelsWhileRunning(t *testing.T) {
	e := newEnv(t, envConfig{})
	a := e.channel("d1", "lte1", ok)
	b := e.channel("d2", "lte1", ok)
	ca, cb := newStub(script("g")), newStub(script("g"))
	e.hc.SetChannels([]ChannelSpec{spec(a, ca)})
	e.start()
	e.settled(a, 1)
	if _, known := e.hc.Status(b.ID); known {
		t.Fatal("b is not configured yet")
	}

	// Add a channel on another device: checked at once.
	e.hc.SetChannels([]ChannelSpec{spec(a, ca), spec(b, cb)})
	e.settled(b, 1)
	if h, _ := cb.calls(); h != 1 {
		t.Fatalf("b health calls = %d", h)
	}

	// Replace b's connector (device edited): checked at once with the new one.
	cb2 := newStub(script("g"))
	e.hc.SetChannels([]ChannelSpec{spec(a, ca), spec(b, cb2)})
	e.settled(b, 2)
	if h, _ := cb2.calls(); h != 1 {
		t.Fatalf("new connector health calls = %d", h)
	}
	if h, _ := cb.calls(); h != 1 {
		t.Fatalf("old connector was used again: %d", h)
	}

	// Remove a: its loop ends and it is forgotten.
	e.hc.SetChannels([]ChannelSpec{spec(b, cb2)})
	e.tr.wait(t, "a's loop gone", func(v view) bool { return !v.asleep(gkey(a)) })
	if _, err := e.hc.CheckNow(e.ctx, a.ID); !errors.Is(err, ErrUnknownChannel) {
		t.Fatalf("CheckNow = %v", err)
	}
	before, _ := ca.calls()
	e.clk.Advance(10 * DefaultInterval)
	e.settled(b, 3)
	if after, _ := ca.calls(); after != before {
		t.Fatal("a removed channel is still checked")
	}

	// Moving a channel to another device keeps one loop per device.
	moved := b
	moved.DeviceID = a.DeviceID
	e.hc.SetChannels([]ChannelSpec{spec(moved, cb2)})
	e.tr.wait(t, "loops swapped", func(v view) bool { return !v.asleep(gkey(b)) && v.asleep(gkey(moved)) })
	e.clk.Advance(DefaultInterval)
	e.settled(moved, 4)
}

func TestLoopReconfiguredDuringCheckDiscardsResult(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	g := newGate()
	old := newStub(func(ctx context.Context, n int) (connector.ChannelHealth, error) {
		if n == 1 {
			return goodHealth(), nil
		}
		return g.fn(ctx, n)
	})
	e.hc.SetChannels([]ChannelSpec{spec(c, old)})
	e.start()
	e.settled(c, 1)
	e.clk.Advance(DefaultInterval)
	g.waitEntered(t)

	fresh := newStub(script("g"))
	e.hc.SetChannels([]ChannelSpec{spec(c, fresh)})
	g.release <- badHealth("searching") // about the old device: must be ignored
	e.settled(c, 2)                     // the discarded result is not counted
	if st := e.status(c.ID); st.State != ok || st.ConsecutiveFailures != 0 {
		t.Fatalf("%+v", st)
	}
	if h, _ := fresh.calls(); h != 1 {
		t.Fatalf("new connector health calls = %d", h)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle

func TestShutdownCancelsCheckWithoutRecordingFailure(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	g := newGate()
	conn := newStub(func(ctx context.Context, n int) (connector.ChannelHealth, error) {
		if n == 1 {
			return goodHealth(), nil
		}
		return g.fn(ctx, n)
	})
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	e.start()
	e.settled(c, 1)
	e.clk.Advance(DefaultInterval)
	g.waitEntered(t)
	e.shutdown()
	if st := e.status(c.ID); st.State != ok || st.ConsecutiveFailures != 0 {
		t.Fatalf("a cancelled check must not count: %+v", st)
	}
	e.shutdown() // idempotent
}

func TestShutdownFlushesPendingState(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.primed(newStub(script("g")))
	e.start()
	e.hc.ReportSend(c.ID, errAuth)
	e.shutdown()
	if got := e.stored(c.ID); got.State != degraded {
		t.Fatalf("stored %s", got.State)
	}
	if got := e.chg.states(); !equalStates(got, []domain.ChannelState{degraded}) {
		t.Fatalf("changes %v", got)
	}
}

func TestLifecycleErrors(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	e.start()
	if err := e.hc.Start(e.ctx); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v", err)
	}
	e.shutdown()
	if err := e.hc.Start(e.ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Shutdown = %v", err)
	}
	e.hc.SetChannels([]ChannelSpec{spec(c, newStub(nil))}) // ignored
	if len(e.hc.Snapshot()) != 0 {
		t.Fatal("SetChannels after Shutdown must be ignored")
	}
	e.hc.ReportSend(c.ID, errAuth) // must not panic or block
}

func TestShutdownWithoutStart(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok, func(c *domain.Channel) { c.Enabled = false })
	e.hc.SetChannels([]ChannelSpec{spec(c, newStub(nil))})
	e.shutdown()
	if got := e.stored(c.ID).State; got != disabled {
		t.Fatalf("stored %s", got)
	}
}

func TestShutdownDeadline(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	release := make(chan struct{})
	conn := newStub(func(context.Context, int) (connector.ChannelHealth, error) {
		<-release // a connector that ignores its context
		return goodHealth(), nil
	})
	entered := make(chan struct{}, 1)
	wrapped := &signalConn{stubConn: conn, entered: entered}
	e.hc.SetChannels([]ChannelSpec{spec(c, wrapped)})
	e.start()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.hc.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v", err)
	}
	close(release)
	e.shutdown() // the loop ends now
}

type signalConn struct {
	*stubConn
	entered chan struct{}
}

func (s *signalConn) Health(ctx context.Context, ch connector.ChannelRef) (connector.ChannelHealth, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	return s.stubConn.Health(ctx, ch)
}

func TestParentContextStopsLoops(t *testing.T) {
	e := newEnv(t, envConfig{})
	c := e.channel("dev", "lte1", ok)
	e.hc.SetChannels([]ChannelSpec{spec(c, newStub(script("g")))})
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.hc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.settled(c, 1)
	cancel()
	e.tr.wait(t, "loop gone", func(v view) bool { return !v.asleep(gkey(c)) })
	e.shutdown()
}
