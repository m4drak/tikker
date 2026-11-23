// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/logging"
	"github.com/m4drak/tikker/internal/store"
)

// errSendTimeout is the cancellation cause of a send that ran past
// Capabilities.SendTimeout.
var errSendTimeout = errors.New("send timeout")

// worker serves one channel. Its goroutine is the only one that touches
// bucket; spec is shared with the dispatcher under mu.
type worker struct {
	d    *Dispatcher
	id   domain.ID
	wake chan struct{} // cap 1: notification or spec change
	quit chan struct{} // closed: take no new leases and exit
	done chan struct{} // closed when the goroutine has exited
	prev <-chan struct{}

	launched bool // guarded by d.mu

	mu       sync.Mutex
	spec     ChannelSpec
	stopOnce sync.Once

	bucket   tokenBucket
	lastKind domain.ErrorKind // class of the last reported send failure
}

func newWorker(d *Dispatcher, s ChannelSpec, prev <-chan struct{}) *worker {
	return &worker{
		d:    d,
		id:   s.Channel.ID,
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
		done: make(chan struct{}),
		prev: prev,
		spec: s,
	}
}

func (w *worker) current() ChannelSpec {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.spec
}

func (w *worker) connector() connector.Connector { return w.current().Connector }

func (w *worker) update(fn func(*ChannelSpec)) {
	w.mu.Lock()
	fn(&w.spec)
	w.mu.Unlock()
	w.poke()
}

func (w *worker) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *worker) stop() { w.stopOnce.Do(func() { close(w.quit) }) }

func (w *worker) stopping() bool {
	select {
	case <-w.quit:
		return true
	case <-w.d.ctx.Done():
		return true
	default:
		return false
	}
}

// sleep waits until from+d (d == 0: until woken), a wake-up, or stop. The
// deadline is absolute, so time that passed since from (store calls) is not
// slept again. It returns false when the worker must exit.
func (w *worker) sleep(from time.Time, d time.Duration) bool {
	var timer <-chan time.Time
	if d > 0 {
		d = from.Add(d).Sub(w.d.clk.Now())
		if d <= 0 {
			return !w.stopping()
		}
		timer = w.d.clk.After(d)
	}
	w.d.sleepHook(w.id, d)
	defer w.d.wakeHook(w.id)
	select {
	case <-w.quit:
		return false
	case <-w.d.ctx.Done():
		return false
	case <-w.wake:
		return true
	case <-timer:
		return true
	}
}

func (w *worker) logger(s ChannelSpec) *slog.Logger {
	return w.d.log.With(slog.String("channel_id", w.id.String()), slog.String("port", s.Channel.Port))
}

func (w *worker) run() {
	defer w.d.wg.Done()
	defer func() {
		close(w.done)
		w.d.workerExited(w.connector())
	}()
	if w.prev != nil { // the replaced worker may still be sending
		select {
		case <-w.prev:
		case <-w.quit:
			return
		case <-w.d.ctx.Done():
			return
		}
	}
	for !w.stopping() {
		if !w.step() {
			return
		}
	}
}

// step runs one iteration of the worker loop. It returns false to exit.
func (w *worker) step() bool {
	s := w.current()
	if !s.active() {
		return w.sleep(time.Time{}, 0)
	}
	now := w.d.clk.Now()
	if d := w.bucket.wait(now, s.Channel.RatePerMin, w.d.opts.RateBurst); d > 0 {
		return w.sleep(now, d)
	}
	timeout, lease := w.d.leaseFor(s.Connector)
	m, ok, err := w.d.repo.AcquireNext(w.d.ctx, store.AcquireRequest{
		ChannelID:   w.id,
		ChannelTags: s.Channel.Tags,
		Now:         now,
		LeaseUntil:  now.Add(lease),
	})
	if err != nil {
		if w.d.ctx.Err() != nil {
			return false
		}
		w.d.stats.update(w.id, Counters{StoreErrors: 1}, nil)
		w.logger(s).Error("acquire failed", slog.Any("error", err))
		return w.sleep(now, w.d.opts.ErrorBackoff)
	}
	if !ok {
		return w.sleep(now, w.idleWait(s, now))
	}
	w.bucket.take(now, s.Channel.RatePerMin)
	w.send(s, m, timeout)
	return true
}

// idleWait decides how long to sleep when nothing could be acquired.
func (w *worker) idleWait(s ChannelSpec, now time.Time) time.Duration {
	at, ok, err := w.d.repo.NextDue(w.d.ctx, w.id, s.Channel.Tags)
	switch {
	case err != nil:
		if w.d.ctx.Err() == nil {
			w.d.stats.update(w.id, Counters{StoreErrors: 1}, nil)
			w.logger(s).Error("next due failed", slog.Any("error", err))
		}
		return w.d.opts.ErrorBackoff
	case !ok:
		return w.d.opts.MaxIdle
	}
	d := at.Sub(now)
	if d <= 0 {
		// Due but not acquirable: past its ExpiresAt (or taken by another
		// channel meanwhile). Let the janitor expire it; it notifies us
		// when it changed something.
		w.d.requestSweep()
		return w.d.opts.MaxIdle
	}
	return min(d, w.d.opts.MaxIdle)
}

// send delivers one leased message and records the outcome.
func (w *worker) send(s ChannelSpec, m domain.Message, timeout time.Duration) {
	log := w.logger(s).With(
		slog.String("message_id", m.ID.String()),
		slog.Int("attempt", m.Attempts),
		slog.Any("peer", logging.Phone(m.Peer)))
	started := w.d.clk.Now()
	w.d.stats.update(w.id, Counters{}, func(c *ChannelStats) {
		c.Sending = true
		c.LastAttemptAt = started
		c.LastMessageID = m.ID
	})
	log.Debug("sending")

	var (
		res         connector.SendResult
		err         error
		interrupted bool
	)
	caps := s.Connector.Capabilities()
	if !caps.Fits(m.Encoding, m.Segments) {
		err = domain.Permanent("send", fmt.Errorf("%s message with %d segment(s) does not fit the channel's connector", m.Encoding, m.Segments))
	} else {
		res, interrupted, err = w.callSend(s, m, timeout)
	}
	w.d.stats.update(w.id, Counters{}, func(c *ChannelStats) { c.Sending = false })
	w.record(m, res, err, interrupted, log)
}

// callSend runs Connector.Send with the send timeout measured on the
// dispatcher's clock. interrupted reports a cancellation by a forced
// shutdown.
func (w *worker) callSend(s ChannelSpec, m domain.Message, timeout time.Duration) (connector.SendResult, bool, error) {
	ctx, cancel := context.WithCancelCause(w.d.ctx)
	defer cancel(nil)
	finished := make(chan struct{})
	timer := w.d.clk.After(timeout)
	go func() {
		select {
		case <-timer:
			cancel(errSendTimeout)
		case <-finished:
		}
	}()
	res, err := s.Connector.Send(ctx, connector.ChannelRef{ChannelID: w.id, Port: s.Channel.Port},
		connector.OutboundSMS{MessageID: m.ID, To: m.Peer, Body: m.Body, Encoding: m.Encoding, Segments: m.Segments})
	close(finished)
	interrupted := err != nil && w.d.ctx.Err() != nil
	if err != nil && errors.Is(context.Cause(ctx), errSendTimeout) && !errors.Is(err, errSendTimeout) {
		err = fmt.Errorf("%w after %s: %w", errSendTimeout, timeout, err)
	}
	return res, interrupted, err
}

// record writes the outcome of a send to the store.
func (w *worker) record(m domain.Message, res connector.SendResult, sendErr error, interrupted bool, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.d.ctx), w.d.opts.FinalizeTimeout)
	defer cancel()
	lease := m.Lease()
	now := w.d.clk.Now()

	if sendErr == nil {
		sentAt := res.AcceptedAt
		if sentAt.IsZero() {
			sentAt = now
		}
		done := m
		done.Status, done.SentAt, done.LastError = domain.StatusSent, sentAt, ""
		if !w.storeResult(ctx, log, w.write(ctx, done, func(r store.MessageRepo) error { return r.MarkSent(ctx, lease, sentAt) })) {
			return
		}
		w.d.stats.update(w.id, Counters{Sent: 1}, func(c *ChannelStats) { c.LastSentAt = sentAt })
		log.Info("sms sent")
		w.d.emit(Event{Kind: EventSent, ChannelID: w.id, MessageID: m.ID, Attempt: m.Attempts, At: now})
		w.report(nil)
		return
	}

	kind := domain.KindOf(sendErr)
	ambiguous := domain.IsAmbiguous(sendErr)
	next, err := domain.NextStatus(domain.StatusSending, domain.EventForSendResult(sendErr), m.Attempts, w.d.opts.Retry.MaxAttempts)
	if err != nil { // cannot happen for a sending message
		log.Error("state machine refused send result", slog.Any("error", err))
		return
	}
	lastErr := truncate(sendErr.Error(), maxLastErrorLen)
	log = log.With(slog.String("error_kind", kind.String()), slog.Bool("possible_duplicate", ambiguous),
		slog.Any("error", sendErr))

	var delta Counters
	if next == domain.StatusQueued {
		at := w.d.opts.Retry.NextAttemptAt(now, m.Attempts, w.d.opts.Rand)
		if interrupted {
			at = now // shutdown, not the message's fault: retry at once after restart
		}
		if !w.storeResult(ctx, log, w.d.repo.Requeue(ctx, lease, store.RequeueParams{
			NextAttemptAt: at, LastError: lastErr, PossibleDuplicate: ambiguous,
		})) {
			return
		}
		delta.Retried = 1
		log.Warn("send failed, message requeued", slog.Time("next_attempt_at", at))
	} else {
		done := m
		done.Status, done.LastError = domain.StatusFailed, lastErr
		done.PossibleDuplicate = done.PossibleDuplicate || ambiguous
		if !w.storeResult(ctx, log, w.write(ctx, done, func(r store.MessageRepo) error {
			return r.MarkFailed(ctx, lease, lastErr, ambiguous)
		})) {
			return
		}
		delta.Failed = 1
		log.Warn("send failed, message failed")
	}
	if ambiguous {
		delta.PossibleDuplicates = 1
		log.Warn("possible duplicate: the device may have accepted the message")
	}
	evKind := EventSendFailed
	if kind == domain.KindAuth {
		delta.AuthErrors = 1
		evKind = EventAuthFailed
		reason := "authentication failed: " + lastErr
		w.update(func(cur *ChannelSpec) {
			cur.Channel.State = domain.ChannelDegraded
			cur.Channel.StateReason = reason
		})
		log.Error("channel degraded after authentication error; not taking messages until restored")
	}
	w.d.stats.update(w.id, delta, func(c *ChannelStats) {
		c.LastErrorAt = now
		c.LastErrorKind = kind
		c.LastError = lastErr
	})
	w.d.emit(Event{Kind: evKind, ChannelID: w.id, MessageID: m.ID, Attempt: m.Attempts,
		ErrorKind: kind, Err: sendErr, At: now})
	if kind != domain.KindPermanent && !interrupted {
		w.report(sendErr)
	}
}

// write runs op against the message repository. For a final status (done
// carries it) and a configured StatusPublisher, op and the event are one
// transaction; the publisher is woken after the commit.
func (w *worker) write(ctx context.Context, done domain.Message, op func(store.MessageRepo) error) error {
	ev := w.d.opts.Events
	if ev == nil {
		return op(w.d.repo)
	}
	err := w.d.opts.Tx.WithTx(ctx, func(tx store.Repos) error {
		if err := op(tx.Messages()); err != nil {
			return err
		}
		if err := ev.PublishStatus(ctx, tx, done); err != nil {
			return fmt.Errorf("publish status event: %w", err)
		}
		return nil
	})
	if err == nil {
		ev.Wake()
	}
	return err
}

// report passes send-health transitions to the SendReporter.
func (w *worker) report(err error) {
	kind := domain.KindOf(err)
	if kind == w.lastKind {
		return
	}
	w.lastKind = kind
	if w.d.opts.Reporter != nil {
		w.d.opts.Reporter.ReportSend(w.id, err)
	}
}

// storeResult handles the error of a result write and reports success.
func (w *worker) storeResult(ctx context.Context, log *slog.Logger, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, domain.ErrLeaseLost):
		w.d.stats.update(w.id, Counters{LeaseLost: 1}, nil)
		log.Warn("lease lost before the result was recorded; leaving the message to its new owner",
			slog.Any("error", err))
	default:
		w.d.stats.update(w.id, Counters{StoreErrors: 1}, nil)
		// The lease expires and RecoverExpiredLeases requeues the message
		// as a possible duplicate (ADR 0002).
		log.Error("recording send result failed", slog.Any("error", err), slog.Bool("ctx_done", ctx.Err() != nil))
	}
	return false
}

// truncate shortens s to at most n bytes without splitting a UTF-8 rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
