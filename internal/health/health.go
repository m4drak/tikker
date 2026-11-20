// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package health decides whether a channel is fit to send: it polls
// Connector.Health, merges the failures reported by the queue and the inbox
// poller, applies hysteresis and publishes the result to the store, the
// dispatcher and the UI (SPEC §4.4, PLAN T17).
//
// # Wiring
//
//	hc, err := health.New(health.Options{Channels: st.Channels(), Clock: clk, Logger: log,
//		Dispatcher: disp,                  // *queue.Dispatcher (SetChannelState)
//		OnStateChange: events.Publish})    // optional, for the UI
//	disp, _ := queue.New(queue.Options{..., Reporter: hc, OnEvent: hc.HandleQueueEvent})
//	poller config: inbox.Config{..., Reporter: hc}
//	hc.SetChannels(specs)                  // same channels and connectors as the dispatcher
//	err = hc.Start(ctx)
//	...
//	hc.SetChannels(specs)                  // after a device/channel change (API, T21)
//	snap := hc.Snapshot()                  // /readyz (T23), dashboard (T31)
//	err = hc.Shutdown(ctx)
//
// Routing itself stays in the store and the queue (store.AcquireNext, ADR
// 0002 §2): a channel that is not "ok" simply takes no leases, so messages
// wait in the queue. When the specs given to the dispatcher come from the
// database, [Checker.ApplyTo] overlays the live state on them.
//
// # States
//
//   - ok: the interface runs and the modem is registered in a network.
//   - degraded: the device rejected our credentials (Auth error from a
//     health check, a send or an inbox poll). Applied at once, without
//     hysteresis. The channel returns to ok only after a successful Health
//     and a successful Probe (credentials and permissions work again).
//   - down: unreachable, interface stopped, no SIM or not registered. Applied
//     after FailThreshold consecutive failed checks; back to ok after
//     RecoverThreshold consecutive successful ones.
//   - disabled: the channel (or its device) is disabled; no checks are made.
//
// The first check of a channel after start-up (or after it was enabled) is
// applied without hysteresis, so a restart converges at once.
//
// # Slow health checks
//
// RouterOS queues the requests of one user, so Health may wait for a send in
// progress (up to 60 s, routeros README ASSUMPTION 9) and time out. Timeouts
// are ordinary failures, therefore the hysteresis absorbs them: with the
// defaults three consecutive failures are needed. Besides, a successful send
// (queue.EventSent) proves the modem is alive and resets the failure count.
// Checks of channels of one device never overlap.
//
// # Flash wear
//
// The signal is quantized before it is compared or stored (default 5 dB for
// RSSI/RSRP and 2 dB for RSRQ/SINR, with a dead band so that values hovering
// around a rounding boundary do not flip). The store is written only when the
// state, reason, operator or quantized signal changed, plus a refresh of
// last_health_at every RefreshInterval. Steady operation writes nothing else.
package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/inbox"
	"github.com/m4drak/tikker/internal/queue"
	"github.com/m4drak/tikker/internal/store"
)

// Defaults for [Options].
const (
	DefaultInterval             = 30 * time.Second // SPEC health_interval
	DefaultRetryInterval        = 10 * time.Second // while a transition is pending
	DefaultTimeout              = 20 * time.Second // above the connector's own 15 s
	DefaultFailThreshold        = 3
	DefaultRecoverThreshold     = 2
	DefaultAuthRecoverThreshold = 1
	DefaultRefreshInterval      = 5 * time.Minute // as sqlite.DefaultHealthRefresh

	DefaultStepRSSI = 5
	DefaultStepRSRP = 5
	DefaultStepRSRQ = 2
	DefaultStepSINR = 2

	// maxChanges bounds the queue of undelivered state changes.
	maxChanges = 256
	// maxStreak keeps the streak counters from overflowing.
	maxStreak = 1 << 20
)

// Errors.
var (
	// ErrClosed is returned after Shutdown.
	ErrClosed = errors.New("health: checker closed")
	// ErrAlreadyStarted is returned by a second Start.
	ErrAlreadyStarted = errors.New("health: checker already started")
	// ErrUnknownChannel is returned by CheckNow for a channel that is not
	// configured.
	ErrUnknownChannel = errors.New("health: unknown channel")
)

// Compile-time checks: one object serves as both reporters.
var (
	_ queue.SendReporter         = (*Checker)(nil)
	_ inbox.ChannelStateReporter = (*Checker)(nil)
)

// StateSetter is the part of queue.Dispatcher the checker needs.
type StateSetter interface {
	// SetChannelState reports whether the channel is configured.
	SetChannelState(id domain.ID, state domain.ChannelState, reason string) bool
}

// Quantization is the step in dB the signal metrics are rounded to before
// they are compared and stored. A step of 0 means the default; 1 (or less
// than 0) disables quantization for that metric.
type Quantization struct {
	RSSI, RSRP, RSRQ, SINR int
}

// StateChange describes a change of a channel's state. It is delivered to
// [Options.OnStateChange] in order, after the dispatcher and the store were
// updated.
type StateChange struct {
	ChannelID domain.ID
	DeviceID  domain.ID
	From, To  domain.ChannelState
	Reason    string
	At        time.Time
}

// Options configures a [Checker].
type Options struct {
	// Channels stores the health of channels (required).
	Channels store.ChannelRepo
	// Clock drives schedules and timeouts (default clock.System()).
	Clock clock.Clock
	// Logger receives state changes and persistence errors (default
	// slog.Default()). Message data and secrets are never logged.
	Logger *slog.Logger
	// Dispatcher, when set, learns every state change (SetChannelState).
	Dispatcher StateSetter
	// OnStateChange, when set, is called from the publishing goroutine for
	// every state change. It must not block and must not call back into
	// the checker synchronously.
	OnStateChange func(StateChange)

	// Interval is the time between checks of a healthy channel
	// (health_interval).
	Interval time.Duration
	// RetryInterval is the time between checks while a state change is
	// pending (a failure or recovery streak is running).
	RetryInterval time.Duration
	// Timeout bounds one Health or Probe call (on the injected clock).
	Timeout time.Duration
	// FailThreshold is the number of consecutive failed checks before an
	// ok or degraded channel goes down.
	FailThreshold int
	// RecoverThreshold is the number of consecutive successful checks
	// before a down channel is ok again.
	RecoverThreshold int
	// AuthRecoverThreshold is the same for a channel that had an Auth
	// error; each of those checks includes a Probe.
	AuthRecoverThreshold int
	// Quantization of the stored signal.
	Quantization Quantization
	// RefreshInterval is the longest last_health_at may stay unchanged in
	// the store while nothing else changes. Keep it at least the store's
	// HealthRefresh.
	RefreshInterval time.Duration
}

func (o Options) withDefaults() (Options, error) {
	if o.Channels == nil {
		return o, errors.New("health: Options.Channels is required")
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	for _, p := range []struct {
		v    *time.Duration
		def  time.Duration
		name string
	}{
		{&o.Interval, DefaultInterval, "Interval"},
		{&o.RetryInterval, DefaultRetryInterval, "RetryInterval"},
		{&o.Timeout, DefaultTimeout, "Timeout"},
		{&o.RefreshInterval, DefaultRefreshInterval, "RefreshInterval"},
	} {
		switch {
		case *p.v < 0:
			return o, fmt.Errorf("health: Options.%s must not be negative", p.name)
		case *p.v == 0:
			*p.v = p.def
		}
	}
	for _, p := range []struct {
		v    *int
		def  int
		name string
	}{
		{&o.FailThreshold, DefaultFailThreshold, "FailThreshold"},
		{&o.RecoverThreshold, DefaultRecoverThreshold, "RecoverThreshold"},
		{&o.AuthRecoverThreshold, DefaultAuthRecoverThreshold, "AuthRecoverThreshold"},
	} {
		switch {
		case *p.v < 0:
			return o, fmt.Errorf("health: Options.%s must not be negative", p.name)
		case *p.v == 0:
			*p.v = p.def
		}
	}
	q := &o.Quantization
	if q.RSSI == 0 {
		q.RSSI = DefaultStepRSSI
	}
	if q.RSRP == 0 {
		q.RSRP = DefaultStepRSRP
	}
	if q.RSRQ == 0 {
		q.RSRQ = DefaultStepRSRQ
	}
	if q.SINR == 0 {
		q.SINR = DefaultStepSINR
	}
	return o, nil
}

// ChannelSpec is one channel the checker watches.
type ChannelSpec struct {
	// Channel supplies ID, DeviceID, Port, Enabled and the initial health
	// (State, StateReason, Operator, Signal, LastHealthAt). Enabled must
	// already account for the device (channel and device both enabled).
	Channel domain.Channel
	// Connector of the channel's device. It must be comparable (a pointer),
	// as for the dispatcher. Nil makes the channel down.
	Connector connector.Connector
}

// hooks let tests observe the loops without time.Sleep.
type hooks struct {
	sleep   func(group string, d time.Duration) // d < 0: until woken
	wake    func(group string)
	checked func(id domain.ID) // after a check was applied and published
}

// Checker is the channel health checker. Its methods are safe for concurrent
// use.
type Checker struct {
	opts  Options
	repo  store.ChannelRepo
	clk   clock.Clock
	log   *slog.Logger
	hooks hooks

	mu      sync.Mutex
	entries map[domain.ID]*entry
	groups  map[string]*group
	dirty   map[domain.ID]*entry
	changes []StateChange
	started bool
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	pubMu   sync.Mutex    // serialises publish
	pubWake chan struct{} // cap 1
}

// New validates opts and returns a checker that is not running yet.
func New(opts Options) (*Checker, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Checker{
		opts:    opts,
		repo:    opts.Channels,
		clk:     opts.Clock,
		log:     opts.Logger.With(slog.String("component", "health")),
		entries: make(map[domain.ID]*entry),
		groups:  make(map[string]*group),
		dirty:   make(map[domain.ID]*entry),
		pubWake: make(chan struct{}, 1),
	}, nil
}

// Start launches the check loops (one per device) and the publisher.
// Cancelling ctx stops them like Shutdown does, without the final flush.
func (c *Checker) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed:
		return ErrClosed
	case c.started:
		return ErrAlreadyStarted
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.started = true
	c.wg.Add(1)
	go c.publisher()
	for _, g := range c.groups {
		c.launchLocked(g)
	}
	c.log.Info("health checker started", slog.Int("channels", len(c.entries)))
	if len(c.dirty) > 0 {
		c.wakePublisher()
	}
	return nil
}

// Shutdown stops the loops (cancelling checks in progress; they do not count
// as failures), waits for them and flushes pending state to the store and the
// dispatcher until ctx ends. It is idempotent.
func (c *Checker) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	first := !c.closed
	c.closed = true
	started := c.started
	if first {
		for _, g := range c.groups {
			g.stop()
		}
		if started {
			c.cancel()
		}
	}
	c.mu.Unlock()

	if started {
		done := make(chan struct{})
		go func() {
			c.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("health: shutdown: %w", ctx.Err())
		}
	}
	if first {
		if err := c.Flush(ctx); err != nil {
			return err
		}
		c.log.Info("health checker stopped")
	}
	return nil
}

// SetChannels replaces the set of watched channels. New channels are checked
// at once, removed ones forgotten, and a changed connector, port or enabled
// flag triggers a new check. State, streaks and the Auth flag of existing
// channels are kept. Duplicate channel IDs: the last one wins.
func (c *Checker) SetChannels(specs []ChannelSpec) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	now := c.clk.Now()
	seen := make(map[domain.ID]bool, len(specs))
	for _, s := range specs {
		id := s.Channel.ID
		seen[id] = true
		e := c.entries[id]
		changed := false
		if e == nil {
			e = c.newEntryLocked(s, now)
			c.entries[id] = e
		} else {
			changed = c.updateEntryLocked(e, s, now)
		}
		c.attachLocked(e, groupKey(s.Channel))
		if changed && e.grp != nil {
			e.grp.poke()
		}
	}
	for id, e := range c.entries {
		if seen[id] {
			continue
		}
		delete(c.entries, id)
		delete(c.dirty, id)
		c.detachLocked(e)
	}
	pending := len(c.dirty) > 0
	c.mu.Unlock()
	if pending {
		c.wakePublisher()
	}
}

// CheckNow runs one check of the channel synchronously (after the checks of
// its device that are in progress) and publishes the result. It is meant for
// "test channel" actions and tests. A disabled channel is not contacted.
func (c *Checker) CheckNow(ctx context.Context, id domain.ID) (ChannelStatus, error) {
	return c.check(ctx, id)
}

// Flush stores and pushes everything that is out of date now, including
// writes that failed earlier.
func (c *Checker) Flush(ctx context.Context) error {
	c.mu.Lock()
	now := c.clk.Now()
	for _, e := range c.entries {
		c.markLocked(e, now)
	}
	c.mu.Unlock()
	return c.publish(ctx)
}

// ReportSend implements queue.SendReporter. An Auth error degrades the
// channel at once; other outcomes only ask for an early health check.
func (c *Checker) ReportSend(channelID domain.ID, err error) {
	c.report(channelID, domain.KindOf(err), "send", err)
}

// ReportInbox implements inbox.ChannelStateReporter, like [Checker.ReportSend].
func (c *Checker) ReportInbox(channelID domain.ID, err error) {
	c.report(channelID, domain.KindOf(err), "inbox", err)
}

// HandleQueueEvent is the queue.Options.OnEvent hook. It catches Auth
// failures that ReportSend does not repeat (the dispatcher reports only
// changes of the failure class, but pauses the worker on every Auth error)
// and uses successful sends as proof of life.
func (c *Checker) HandleQueueEvent(ev queue.Event) {
	switch ev.Kind {
	case queue.EventAuthFailed:
		c.report(ev.ChannelID, domain.KindAuth, "send", ev.Err)
	case queue.EventSent:
		c.mu.Lock()
		if e := c.entries[ev.ChannelID]; e != nil && e.state == domain.ChannelOK {
			e.failStreak = 0
		}
		c.mu.Unlock()
	case queue.EventSendFailed:
		if ev.ErrorKind == domain.KindTransient {
			c.report(ev.ChannelID, domain.KindTransient, "send", ev.Err)
		}
	}
}

// report handles a failure class from the queue or the inbox; kind "" means
// success.
func (c *Checker) report(id domain.ID, kind domain.ErrorKind, source string, err error) {
	c.mu.Lock()
	e := c.entries[id]
	if c.closed || e == nil || !e.spec.Channel.Enabled {
		c.mu.Unlock()
		return
	}
	now := c.clk.Now()
	switch kind {
	case domain.KindAuth:
		c.authLocked(e, source, now)
		c.log.Warn("channel reported an authentication error",
			slog.String("channel_id", id.String()), slog.String("source", source),
			slog.String("code", string(domain.CodeOf(err))))
	case domain.KindTransient, "":
		// A send or inbox failure says nothing certain about the modem, and
		// a success after a failure only hints at recovery: ask the health
		// check, but only when something may have changed.
		if kind != "" || e.state != domain.ChannelOK || e.failStreak > 0 {
			c.requestCheckLocked(e, now)
		}
	}
	pending := len(c.dirty) > 0
	c.mu.Unlock()
	if pending {
		c.wakePublisher()
	}
}

func (c *Checker) wakePublisher() {
	select {
	case c.pubWake <- struct{}{}:
	default:
	}
}

func (c *Checker) publisher() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.pubWake:
			if err := c.publish(c.ctx); err != nil && c.ctx.Err() == nil {
				c.log.Error("publishing channel health failed", slog.Any("error", err))
			}
		}
	}
}
