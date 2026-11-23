// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package queue sends queued outbound messages: one worker per channel pulls
// messages from the store under a lease, honours the channel's rate limit,
// calls the connector and records the outcome (ADR 0002, SPEC §4.2-§4.5).
//
// # Wiring
//
//	d, err := queue.New(queue.Options{Messages: st.Messages(), Clock: clk, Logger: log,
//		Retry: retryPolicy, OnEvent: health.HandleQueueEvent})
//	d.SetChannels(specs)       // from the store: channels + their device connectors
//	err = d.Start(ctx)         // recovers expired leases, starts workers
//	...
//	d.Notify()                 // after MessageRepo.Enqueue (API, T21)
//	d.SetChannelState(id, domain.ChannelOK, "") // health checker (T17)
//	d.SetChannels(specs)       // after a device/channel change (API, T21)
//	st := d.Stats()            // diagnostics (T23), dashboard (T31)
//	...
//	err = d.Shutdown(ctx)      // SIGTERM: finish in-flight sends until ctx ends
//
// # Behaviour
//
//   - A worker takes leases only while its channel is enabled, in state ok
//     and has a connector. Routing happens on acquire (store.AcquireNext):
//     pinned messages go to their channel, others to any channel carrying
//     their tags.
//   - The lease is 2 x Capabilities.SendTimeout + LeaseMargin; each Send
//     runs with a timeout of Capabilities.SendTimeout on the injected clock.
//   - Results follow domain.NextStatus: success -> MarkSent; Permanent ->
//     MarkFailed; Transient, Auth and unclassified -> Requeue with backoff
//     from the RetryPolicy, or MarkFailed once the retry budget is used up.
//     Ambiguous errors (domain.IsAmbiguous) set PossibleDuplicate.
//   - An Auth error degrades the channel locally and reports EventAuthFailed;
//     the worker stops taking leases until SetChannelState or SetChannels
//     sets the channel back to ok.
//   - domain.ErrLeaseLost on a result write means the lease was recovered by
//     someone else; the worker logs it and moves on.
//   - Waking without polling the database: a worker sleeps until Notify, a
//     channel update, the earliest NextDue of its eligible messages or
//     MaxIdle, whichever comes first. An idle poll is read-only.
//   - A janitor goroutine runs RecoverExpiredLeases at Start and every
//     lease/2, and ExpireQueued every ExpireInterval (and on demand when a
//     worker sees a due message it cannot acquire, i.e. an expired one).
//   - Shutdown stops taking leases, waits for in-flight sends until its ctx
//     ends, then cancels them; a cancelled send is ambiguous and is
//     requeued (or failed at the retry limit) with PossibleDuplicate.
package queue

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
	"github.com/m4drak/tikker/internal/store"
)

// Defaults for [Options].
const (
	DefaultMaxIdle         = 30 * time.Second
	DefaultExpireInterval  = time.Minute
	DefaultErrorBackoff    = 5 * time.Second
	DefaultLeaseMargin     = 30 * time.Second
	DefaultSendTimeout     = 65 * time.Second
	DefaultFinalizeTimeout = 10 * time.Second
	DefaultRateBurst       = 1
	// maxLastErrorLen bounds Message.LastError written by the dispatcher.
	maxLastErrorLen = 500
)

// ErrNotStarted, ErrClosed and ErrAlreadyStarted are returned by Start.
var (
	ErrClosed         = errors.New("queue: dispatcher closed")
	ErrAlreadyStarted = errors.New("queue: dispatcher already started")
)

// EventKind names a dispatcher [Event].
type EventKind string

// Event kinds.
const (
	// EventSent: a send succeeded on the channel.
	EventSent EventKind = "sent"
	// EventSendFailed: a send failed with a Transient, Permanent or
	// unclassified error. The health checker may use it as a hint.
	EventSendFailed EventKind = "send_failed"
	// EventAuthFailed: a send failed with an Auth error. The worker has
	// degraded the channel locally and stopped taking leases; the health
	// checker (T17) should persist the degraded state and call
	// SetChannelState(ok) once the device accepts the credentials again.
	EventAuthFailed EventKind = "auth_failed"
)

// Event reports something the health checker or UI may care about. It never
// carries message bodies or full numbers.
type Event struct {
	Kind      EventKind
	ChannelID domain.ID
	MessageID domain.ID
	Attempt   int
	ErrorKind domain.ErrorKind // empty for EventSent
	Err       error            // nil for EventSent
	At        time.Time
}

// Transactor runs fn in one database transaction; store.Store implements it.
type Transactor interface {
	WithTx(ctx context.Context, fn func(tx store.Repos) error) error
}

// StatusPublisher records an event for an outbound message that reached a
// final status (m carries it) in the transaction tx; Wake is called after the
// commit. *webhook.Publisher implements it.
type StatusPublisher interface {
	PublishStatus(ctx context.Context, tx store.Repos, m domain.Message) error
	Wake()
}

// SendReporter receives changes of a channel's send health, mirroring
// inbox.ChannelStateReporter (T16). The health checker (T17) implements both.
//
// ReportSend is called from the worker goroutine only on transitions: the
// first failed send, a change of failure class (domain.KindOf(err)), and the
// first successful send after a failure (err == nil). Permanent errors are
// about the message, not the channel, and are not reported. It must not
// block and must not call back into the dispatcher synchronously.
type SendReporter interface {
	ReportSend(channelID domain.ID, err error)
}

// SendReporterFunc adapts a function to [SendReporter].
type SendReporterFunc func(channelID domain.ID, err error)

// ReportSend implements [SendReporter].
func (f SendReporterFunc) ReportSend(channelID domain.ID, err error) { f(channelID, err) }

// Options configures a [Dispatcher].
type Options struct {
	// Messages is the queue store (required).
	Messages store.MessageRepo
	// Clock drives leases, timeouts, backoff and the rate limit (default
	// clock.System()).
	Clock clock.Clock
	// Logger receives the dispatcher's logs (default slog.Default()).
	Logger *slog.Logger
	// Retry is the retry policy; the zero value means
	// domain.DefaultRetryPolicy(). It must pass Validate.
	Retry domain.RetryPolicy
	// Rand is the jitter source for Retry (default domain.DefaultRand).
	Rand domain.RandFunc

	// MaxIdle caps how long an idle worker sleeps before checking the
	// store again (safety net for missed notifications).
	MaxIdle time.Duration
	// ExpireInterval is the period of ExpireQueued.
	ExpireInterval time.Duration
	// RecoverInterval is the period of RecoverExpiredLeases; zero means
	// half of the shortest lease among the configured channels.
	RecoverInterval time.Duration
	// ErrorBackoff is how long a worker waits after a store error.
	ErrorBackoff time.Duration
	// LeaseMargin is added to 2 x SendTimeout to form the lease.
	LeaseMargin time.Duration
	// DefaultSendTimeout is used when a connector reports no
	// Capabilities.SendTimeout.
	DefaultSendTimeout time.Duration
	// FinalizeTimeout bounds a result write after the dispatcher's context
	// was cancelled (shutdown).
	FinalizeTimeout time.Duration
	// RateBurst is the token bucket capacity (default 1: sends are spaced
	// evenly at 60 s / rate_per_min).
	RateBurst int

	// Events, when set together with Tx, is told about the final status of a
	// message (sent or failed) inside the transaction that records it, so a
	// webhook event cannot be lost between the status change and the
	// notification. A failure of Events fails the write: the lease then
	// expires and the message is recovered as a possible duplicate (ADR 0002).
	Events StatusPublisher
	// Tx runs the transaction for Events (the store). Required with Events.
	Tx Transactor

	// Reporter, when set, receives send-health transitions per channel.
	Reporter SendReporter
	// OnEvent, when set, is called synchronously from worker goroutines.
	// It must not block and must not call back into the dispatcher
	// synchronously.
	OnEvent func(Event)
	// OnConnectorReleased, when set, is called once a connector is no
	// longer referenced by the configured channels and no worker is using
	// it any more (after SetChannels replaced or removed it, or after
	// Shutdown), so its owner may close it. The dispatcher never closes
	// connectors itself. Connectors must be comparable (pointer types).
	OnConnectorReleased func(connector.Connector)
}

func (o Options) withDefaults() (Options, error) {
	if o.Messages == nil {
		return o, errors.New("queue: Options.Messages is required")
	}
	if o.Events != nil && o.Tx == nil {
		return o, errors.New("queue: Options.Events requires Options.Tx")
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Retry == (domain.RetryPolicy{}) {
		o.Retry = domain.DefaultRetryPolicy()
	}
	if err := o.Retry.Validate(); err != nil {
		return o, fmt.Errorf("queue: %w", err)
	}
	if o.Rand == nil {
		o.Rand = domain.DefaultRand
	}
	setDefault(&o.MaxIdle, DefaultMaxIdle)
	setDefault(&o.ExpireInterval, DefaultExpireInterval)
	setDefault(&o.ErrorBackoff, DefaultErrorBackoff)
	setDefault(&o.LeaseMargin, DefaultLeaseMargin)
	setDefault(&o.DefaultSendTimeout, DefaultSendTimeout)
	setDefault(&o.FinalizeTimeout, DefaultFinalizeTimeout)
	if o.RateBurst <= 0 {
		o.RateBurst = DefaultRateBurst
	}
	return o, nil
}

func setDefault(d *time.Duration, def time.Duration) {
	if *d <= 0 {
		*d = def
	}
}

// ChannelSpec is one channel the dispatcher should serve.
type ChannelSpec struct {
	// Channel supplies ID, Port, Tags, RatePerMin, Enabled and State.
	// Enabled must already account for the device (channel and device
	// both enabled).
	Channel domain.Channel
	// Connector of the channel's device; nil keeps the worker idle.
	Connector connector.Connector
}

func (s ChannelSpec) active() bool {
	return s.Channel.Enabled && s.Channel.State == domain.ChannelOK && s.Connector != nil
}

// hooks let tests observe workers going to sleep without time.Sleep.
type hooks struct {
	sleep func(id domain.ID, d time.Duration) // d == 0: until woken
	wake  func(id domain.ID)
}

// janitorID identifies the janitor goroutine in hooks.
const janitorID domain.ID = "janitor"

// Dispatcher runs the per-channel workers. Its methods are safe for
// concurrent use.
type Dispatcher struct {
	opts  Options
	repo  store.MessageRepo
	clk   clock.Clock
	log   *slog.Logger
	stats *statsBook
	hooks hooks

	mu       sync.Mutex
	workers  map[domain.ID]*worker
	connRefs map[connector.Connector]int
	started  bool
	closed   bool

	ctx    context.Context // cancelled by a forced shutdown
	cancel context.CancelFunc
	wg     sync.WaitGroup

	sweep       chan struct{} // cap 1: ask the janitor to expire now
	janitorQuit chan struct{}
}

// New validates opts and returns a dispatcher that is not running yet.
func New(opts Options) (*Dispatcher, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Dispatcher{
		opts:        opts,
		repo:        opts.Messages,
		clk:         opts.Clock,
		log:         opts.Logger.With(slog.String("component", "queue")),
		stats:       newStatsBook(),
		workers:     make(map[domain.ID]*worker),
		connRefs:    make(map[connector.Connector]int),
		sweep:       make(chan struct{}, 1),
		janitorQuit: make(chan struct{}),
	}, nil
}

// Start recovers expired leases and expired messages, then starts the
// janitor and a worker for every channel set so far. Cancelling ctx is a
// forced stop (like Shutdown with an expired context); use Shutdown for a
// graceful one. Start fails if the initial recovery fails.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.mu.Lock()
	switch {
	case d.closed:
		d.mu.Unlock()
		return ErrClosed
	case d.started:
		d.mu.Unlock()
		return ErrAlreadyStarted
	}
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.mu.Unlock()

	if err := d.recover(d.ctx); err != nil {
		d.cancel()
		return fmt.Errorf("queue: start: %w", err)
	}
	d.expire(d.ctx)

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed { // Shutdown raced with Start
		d.cancel()
		return ErrClosed
	}
	d.started = true
	d.wg.Add(1)
	go d.janitor()
	for _, w := range d.workers {
		d.launchLocked(w)
	}
	d.log.Info("dispatcher started", slog.Int("channels", len(d.workers)))
	return nil
}

// SetChannels replaces the set of served channels. Workers of new channels
// start, workers of removed channels stop after their in-flight send, and
// existing workers pick up changed tags, rate, state and enabled flag at
// once. A changed connector restarts the worker; the new one waits for the
// old one to finish its in-flight send, so one channel never runs two sends
// at a time. Duplicate channel IDs: the last one wins.
func (d *Dispatcher) SetChannels(specs []ChannelSpec) {
	var released []connector.Connector
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	seen := make(map[domain.ID]bool, len(specs))
	for _, s := range specs {
		id := s.Channel.ID
		seen[id] = true
		old := d.workers[id]
		if old != nil && old.connector() == s.Connector {
			old.update(func(cur *ChannelSpec) { *cur = s })
			continue
		}
		var prev <-chan struct{}
		if old != nil {
			prev = old.done
			released = append(released, d.retireLocked(old)...)
		}
		w := newWorker(d, s, prev)
		d.workers[id] = w
		d.refLocked(s.Connector)
		if d.started {
			d.launchLocked(w)
		}
	}
	for id, w := range d.workers {
		if !seen[id] {
			delete(d.workers, id)
			released = append(released, d.retireLocked(w)...)
		}
	}
	d.mu.Unlock()
	d.release(released)
}

// SetChannelState sets the state of one channel (health checker, T17). A
// channel leaves the paused state set after an Auth error only through this
// method or SetChannels. It reports whether the channel is configured.
func (d *Dispatcher) SetChannelState(id domain.ID, state domain.ChannelState, reason string) bool {
	d.mu.Lock()
	w := d.workers[id]
	d.mu.Unlock()
	if w == nil {
		return false
	}
	w.update(func(cur *ChannelSpec) {
		cur.Channel.State = state
		cur.Channel.StateReason = reason
	})
	return true
}

// Notify wakes every worker to look for due messages. Call it after
// enqueueing; it never blocks and costs nothing when workers are busy.
func (d *Dispatcher) Notify() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, w := range d.workers {
		w.poke()
	}
}

// Stats returns a snapshot of counters and channel states.
func (d *Dispatcher) Stats() Stats {
	d.mu.Lock()
	views := make([]ChannelStats, 0, len(d.workers))
	for _, w := range d.workers {
		s := w.current()
		views = append(views, ChannelStats{
			ChannelID:   s.Channel.ID,
			State:       s.Channel.State,
			StateReason: s.Channel.StateReason,
			Enabled:     s.Channel.Enabled,
			Active:      s.active(),
		})
	}
	started, closed := d.started, d.closed
	d.mu.Unlock()
	st := d.stats.snapshot(views)
	st.Started, st.Closed = started, closed
	return st
}

// Shutdown stops the dispatcher: workers take no new leases and in-flight
// sends may finish until ctx ends. Then they are cancelled; a cancelled send
// is ambiguous and recorded with PossibleDuplicate (ADR 0002). Shutdown
// returns after every goroutine has exited, with ctx's error if sends had to
// be cancelled. Calling it again waits for the first call's work.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	first := !d.closed
	d.closed = true
	var released []connector.Connector
	if first {
		close(d.janitorQuit)
		for id, w := range d.workers {
			delete(d.workers, id)
			released = append(released, d.retireLocked(w)...)
		}
	}
	started := d.started
	d.mu.Unlock()
	d.release(released)
	if !started {
		return nil
	}

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = fmt.Errorf("queue: shutdown: in-flight sends cancelled: %w", ctx.Err())
		d.log.Warn("shutdown deadline reached, cancelling in-flight sends")
		d.cancel()
		<-done
	}
	d.cancel()
	if first {
		d.log.Info("dispatcher stopped")
	}
	return err
}

// launchLocked starts w's goroutine. d.mu must be held.
func (d *Dispatcher) launchLocked(w *worker) {
	if w.launched {
		return
	}
	w.launched = true
	d.wg.Add(1)
	go w.run()
}

// retireLocked tells w to stop. A worker that never ran releases its
// connector reference at once (returned for release outside the lock).
func (d *Dispatcher) retireLocked(w *worker) []connector.Connector {
	w.stop()
	if w.launched {
		return nil // the goroutine drops its reference when it exits
	}
	close(w.done)
	return d.unrefLocked(w.connector())
}

func (d *Dispatcher) refLocked(c connector.Connector) {
	if c != nil {
		d.connRefs[c]++
	}
}

func (d *Dispatcher) unrefLocked(c connector.Connector) []connector.Connector {
	if c == nil {
		return nil
	}
	d.connRefs[c]--
	if d.connRefs[c] > 0 {
		return nil
	}
	delete(d.connRefs, c)
	return []connector.Connector{c}
}

// workerExited drops the reference of an exited worker.
func (d *Dispatcher) workerExited(c connector.Connector) {
	d.mu.Lock()
	rel := d.unrefLocked(c)
	d.mu.Unlock()
	d.release(rel)
}

func (d *Dispatcher) release(cs []connector.Connector) {
	if d.opts.OnConnectorReleased == nil {
		return
	}
	for _, c := range cs {
		d.opts.OnConnectorReleased(c)
	}
}

// leaseFor returns the send timeout and lease length for a connector.
func (d *Dispatcher) leaseFor(c connector.Connector) (timeout, lease time.Duration) {
	timeout = d.opts.DefaultSendTimeout
	if c != nil {
		if t := c.Capabilities().SendTimeout; t > 0 {
			timeout = t
		}
	}
	return timeout, 2*timeout + d.opts.LeaseMargin
}

// recoverInterval is RecoverInterval or half the shortest lease.
func (d *Dispatcher) recoverInterval() time.Duration {
	if d.opts.RecoverInterval > 0 {
		return d.opts.RecoverInterval
	}
	_, shortest := d.leaseFor(nil)
	d.mu.Lock()
	for _, w := range d.workers {
		if _, l := d.leaseFor(w.connector()); l < shortest {
			shortest = l
		}
	}
	d.mu.Unlock()
	return max(shortest/2, time.Second)
}

// requestSweep asks the janitor to run ExpireQueued soon.
func (d *Dispatcher) requestSweep() {
	select {
	case d.sweep <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) emit(ev Event) {
	if d.opts.OnEvent != nil {
		d.opts.OnEvent(ev)
	}
}

func (d *Dispatcher) sleepHook(id domain.ID, dur time.Duration) {
	if d.hooks.sleep != nil {
		d.hooks.sleep(id, dur)
	}
}

func (d *Dispatcher) wakeHook(id domain.ID) {
	if d.hooks.wake != nil {
		d.hooks.wake(id)
	}
}

// ---------------------------------------------------------------------------
// Janitor

func (d *Dispatcher) janitor() {
	defer d.wg.Done()
	now := d.clk.Now()
	nextRecover := now.Add(d.recoverInterval())
	nextExpire := now.Add(d.opts.ExpireInterval)
	for {
		now = d.clk.Now()
		if !now.Before(nextRecover) {
			if err := d.recover(d.ctx); err != nil && d.ctx.Err() == nil {
				d.log.Error("recover expired leases failed", slog.Any("error", err))
			}
			nextRecover = now.Add(d.recoverInterval())
		}
		if !now.Before(nextExpire) {
			d.expire(d.ctx)
			nextExpire = now.Add(d.opts.ExpireInterval)
		}
		next := nextRecover
		if nextExpire.Before(next) {
			next = nextExpire
		}
		wait := next.Sub(d.clk.Now()) // fresh: the runs above took time
		timer := d.clk.After(wait)
		d.sleepHook(janitorID, wait)
		select {
		case <-d.janitorQuit:
			d.wakeHook(janitorID)
			return
		case <-d.ctx.Done():
			d.wakeHook(janitorID)
			return
		case <-d.sweep:
			d.wakeHook(janitorID)
			d.expire(d.ctx)
		case <-timer:
			d.wakeHook(janitorID)
		}
	}
}

// recover runs RecoverExpiredLeases and logs every possible duplicate. With a
// StatusPublisher the recovery and the events of the messages that became
// failed are one transaction: no event is lost or invented.
func (d *Dispatcher) recover(ctx context.Context) error {
	now := d.clk.Now()
	var recs []store.RecoveredLease
	var err error
	if d.opts.Events == nil {
		recs, err = d.repo.RecoverExpiredLeases(ctx, now, d.opts.Retry.MaxAttempts)
	} else {
		err = d.opts.Tx.WithTx(ctx, func(tx store.Repos) error {
			var err error
			if recs, err = tx.Messages().RecoverExpiredLeases(ctx, now, d.opts.Retry.MaxAttempts); err != nil {
				return err
			}
			for _, r := range recs {
				if r.NewStatus != domain.StatusFailed {
					continue
				}
				m, err := tx.Messages().Get(ctx, r.MessageID)
				if err != nil {
					return err
				}
				if err := d.opts.Events.PublishStatus(ctx, tx, m); err != nil {
					return fmt.Errorf("publish status event: %w", err)
				}
			}
			return nil
		})
	}
	if err != nil {
		d.stats.update("", Counters{StoreErrors: 1}, nil)
		return fmt.Errorf("recover expired leases: %w", err)
	}
	requeued, failed := false, false
	for _, r := range recs {
		d.log.Warn("possible duplicate: lease expired without a recorded result",
			slog.String("message_id", r.MessageID.String()),
			slog.String("channel_id", r.ChannelID.String()),
			slog.Int("attempt", r.Attempts),
			slog.String("new_status", r.NewStatus.String()))
		requeued = requeued || r.NewStatus == domain.StatusQueued
		failed = failed || r.NewStatus == domain.StatusFailed
	}
	if failed && d.opts.Events != nil {
		d.opts.Events.Wake()
	}
	d.stats.janitor(func(s *statsBook) {
		s.recovered += int64(len(recs))
		s.totals.PossibleDuplicates += int64(len(recs))
		s.recoverAt = now
	})
	if requeued {
		d.Notify()
	}
	return nil
}

// expire runs ExpireQueued; with a StatusPublisher the expired messages and
// their events are one transaction.
func (d *Dispatcher) expire(ctx context.Context) {
	now := d.clk.Now()
	var n int
	var err error
	if d.opts.Events == nil {
		n, err = d.repo.ExpireQueued(ctx, now)
	} else {
		err = d.opts.Tx.WithTx(ctx, func(tx store.Repos) error {
			ms, err := tx.Messages().ExpireQueuedMessages(ctx, now)
			if err != nil {
				return err
			}
			for _, m := range ms {
				if err := d.opts.Events.PublishStatus(ctx, tx, m); err != nil {
					return fmt.Errorf("publish status event: %w", err)
				}
			}
			n = len(ms)
			return nil
		})
		if err == nil && n > 0 {
			d.opts.Events.Wake()
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			d.stats.update("", Counters{StoreErrors: 1}, nil)
			d.log.Error("expire queued messages failed", slog.Any("error", err))
		}
		return
	}
	if n > 0 {
		d.log.Info("expired queued messages", slog.Int("count", n))
	}
	d.stats.janitor(func(s *statsBook) {
		s.expired += int64(n)
		s.expireAt = now
	})
	if n > 0 {
		d.Notify() // workers may have waited behind the expired messages
	}
}
