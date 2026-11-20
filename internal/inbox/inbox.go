// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package inbox polls the SMS inbox of a device channel and stores what it
// finds (SPEC §4.3, ADR 0002 §4, task T16).
//
// # Cycle
//
// One [Poller] serves one channel. Every poll interval it runs
//
//	FetchInbox -> store in ONE transaction -> commit -> DeleteInbox
//
// so a message is never deleted from the router before it is safe in the
// database. A crash or a failed DeleteInbox leaves the entries on the router;
// the next cycle fetches them again and deduplication absorbs them: no second
// row is created, the entries are just deleted again.
//
// # Why per channel
//
// A connector filters the inbox by the channel's port (RouterOS inbox entries
// may carry no port field; the connector then attributes the whole inbox to
// the port set in /tool sms). Polling per channel keeps the port in the
// dedup hash and lets a failing modem back off without delaying its
// neighbours. RouterOS serialises the requests of one user, so pollers of one
// device share the router's queue anyway; the interval is long compared to a
// request. A device-level poller would add nothing but a second place that
// needs the port filter.
//
// # Deduplication and multi-part messages
//
// The dedup hash is computed per router entry from its raw data
// (domain.InboundDedupHash with the PDU when the connector reports one,
// otherwise the device-decoded text), before any decoding or joining.
//
// An entry whose PDU carries a concatenation header is a part. Parts are
// stored in the store's inbound_parts table (not left on the router, which
// has a small inbox) and joined with pdu.Assemble together with the parts
// still pending from earlier cycles. When a message is complete it is stored
// as one message, and its parts become content-free tombstones, kept for
// [Config.PartRetention] so that a part redelivered after a failed deletion
// is recognised and does not start a new message. Parts that wait longer than
// [Config.IncompleteAfter] are stored as they are, with LastError
// "incomplete: n/m parts".
//
// Entries without a usable PDU (the connector does not report one, the field
// is empty, or it cannot be decoded) are stored with the device's text, which
// RouterOS decodes as GSM-7. A PDU that fails to decode never blocks the other
// entries.
//
// # Flash wear
//
// An empty inbox costs one FetchInbox call and one read of the pending parts;
// nothing is written. A batch of duplicates writes nothing either.
//
// # Channel state
//
// Failures back off exponentially (poll interval doubling up to
// [Config.MaxBackoff]) and are logged on the first failure and then only when
// the failure count passes a power of two. Changes of the failure class are
// passed to a [ChannelStateReporter], so that the health checker can mark a
// channel degraded on Auth errors and recover it later.
package inbox

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

// Defaults for [Config].
const (
	DefaultInterval        = 15 * time.Second // SPEC §4.3 inbox_poll_interval
	DefaultMaxBackoff      = 5 * time.Minute
	DefaultIncompleteAfter = 24 * time.Hour
	DefaultPartRetention   = 7 * 24 * time.Hour

	// deleteChunk bounds the number of entries in one DeleteInbox call.
	deleteChunk = 100
	// purgeEvery is how often tombstones are purged.
	purgeEvery = time.Hour
	// maxBackoffShift bounds the doubling so the shift cannot overflow.
	maxBackoffShift = 20
)

// ChannelStateReporter receives changes of a channel's inbox health. The
// health checker (T17) or the queue's channel state implements it.
//
// ReportInbox is called from the poller goroutine, only on transitions: the
// first failure, a change of failure class (domain.KindOf(err)), and the
// first success after a failure (err == nil). It must not block.
type ChannelStateReporter interface {
	ReportInbox(channelID domain.ID, err error)
}

// ReporterFunc adapts a function to [ChannelStateReporter].
type ReporterFunc func(channelID domain.ID, err error)

// ReportInbox implements [ChannelStateReporter].
func (f ReporterFunc) ReportInbox(channelID domain.ID, err error) { f(channelID, err) }

// Config configures a [Poller].
type Config struct {
	// Channel is the channel to poll (ID, DeviceID and Port are used).
	Channel domain.Channel
	// Conn is the connector of the channel's device.
	Conn  connector.Connector
	Store store.Store
	// Clock drives the poll timer and part expiry (default clock.System()).
	Clock clock.Clock
	// IDs creates message and part IDs (default ULIDs from Clock).
	IDs domain.IDGenerator
	// Logger receives poller logs (default slog.Default()). Message bodies
	// are never logged and numbers are masked.
	Logger *slog.Logger

	// Interval is the time between polls (inbox_poll_interval; default
	// DefaultInterval).
	Interval time.Duration
	// MaxBackoff caps the delay after consecutive failures (default
	// DefaultMaxBackoff, never below Interval).
	MaxBackoff time.Duration
	// IncompleteAfter is how long the parts of an incomplete multi-part
	// message wait for the rest (default DefaultIncompleteAfter).
	IncompleteAfter time.Duration
	// PartRetention is how long consumed parts are remembered for
	// deduplication (default DefaultPartRetention). It should exceed the
	// longest outage after which a router may still hold undeleted entries.
	PartRetention time.Duration

	// Reporter, if set, is told about channel state changes.
	Reporter ChannelStateReporter
	// OnMessage, if set, is called for every newly stored message, in
	// storage order, right after the transaction commits and before the
	// entries are deleted from the router (hook for webhooks and the UI,
	// not implemented here). It must not block. A crash between commit and
	// call loses the notification, never the message.
	OnMessage func(domain.Message)
}

// CycleResult describes one poll cycle.
type CycleResult struct {
	Fetched    int // entries returned by the device
	Stored     int // new messages stored
	Duplicates int // entries recognised as already stored
	Pending    int // parts stored that still wait for their message
	Expired    int // incomplete messages stored because their parts timed out
	Deleted    int // entries deleted from the device
}

// Poller polls one channel's inbox. Create it with [New].
type Poller struct {
	ch     domain.Channel
	ref    connector.ChannelRef
	conn   connector.Connector
	st     store.Store
	clk    clock.Clock
	ids    domain.IDGenerator
	log    *slog.Logger
	usePDU bool // connector reports PDUs

	interval, maxBackoff, incompleteAfter, partRetention time.Duration

	reporter  ChannelStateReporter
	onMessage func(domain.Message)

	mu        sync.Mutex // serialises cycles
	failures  int
	lastKind  domain.ErrorKind // class of the last reported failure
	lastPurge time.Time
}

// New validates cfg and returns a Poller.
func New(cfg Config) (*Poller, error) {
	switch {
	case cfg.Conn == nil:
		return nil, errors.New("inbox: no connector")
	case cfg.Store == nil:
		return nil, errors.New("inbox: no store")
	case cfg.Channel.ID.IsZero() || cfg.Channel.DeviceID.IsZero() || cfg.Channel.Port == "":
		return nil, errors.New("inbox: channel needs ID, DeviceID and Port")
	case cfg.Interval < 0 || cfg.MaxBackoff < 0 || cfg.IncompleteAfter < 0 || cfg.PartRetention < 0:
		return nil, errors.New("inbox: negative duration")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System()
	}
	if cfg.IDs == nil {
		cfg.IDs = domain.NewULIDGenerator(cfg.Clock)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	cfg.MaxBackoff = max(cfg.MaxBackoff, cfg.Interval)
	if cfg.IncompleteAfter == 0 {
		cfg.IncompleteAfter = DefaultIncompleteAfter
	}
	if cfg.PartRetention == 0 {
		cfg.PartRetention = DefaultPartRetention
	}
	return &Poller{
		ch:   cfg.Channel,
		ref:  connector.ChannelRef{ChannelID: cfg.Channel.ID, Port: cfg.Channel.Port},
		conn: cfg.Conn,
		st:   cfg.Store,
		clk:  cfg.Clock,
		ids:  cfg.IDs,
		log: cfg.Logger.With(
			slog.String("component", "inbox"),
			slog.String("channel", string(cfg.Channel.ID)),
			slog.String("port", cfg.Channel.Port)),
		usePDU:          cfg.Conn.Capabilities().InboundPDU,
		interval:        cfg.Interval,
		maxBackoff:      cfg.MaxBackoff,
		incompleteAfter: cfg.IncompleteAfter,
		partRetention:   cfg.PartRetention,
		reporter:        cfg.Reporter,
		onMessage:       cfg.OnMessage,
	}, nil
}

// Run polls immediately and then repeatedly until ctx ends, and returns
// ctx.Err(). Failures are handled inside (back-off, logging, reporting).
func (p *Poller) Run(ctx context.Context) error {
	for {
		delay := p.step(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.clk.After(delay):
		}
	}
}

// step runs one cycle with the bookkeeping of Run and returns the delay
// before the next one.
func (p *Poller) step(ctx context.Context) time.Duration {
	res, err := p.PollOnce(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return p.interval // shutting down, not a channel failure
		}
		p.failures++
		p.noteFailure(err)
		return p.backoff()
	}
	if p.failures > 0 {
		p.log.Info("inbox recovered", slog.Int("failures", p.failures))
		p.failures = 0
		p.lastKind = ""
		p.report(nil)
	}
	if res.Stored+res.Expired > 0 || res.Pending > 0 {
		p.log.Info("inbox cycle",
			slog.Int("fetched", res.Fetched), slog.Int("stored", res.Stored),
			slog.Int("duplicates", res.Duplicates), slog.Int("pending_parts", res.Pending),
			slog.Int("expired", res.Expired))
	}
	p.purgeTombstones(ctx)
	return p.interval
}

// backoff returns interval * 2^failures, capped at maxBackoff.
func (p *Poller) backoff() time.Duration {
	d := p.interval << min(p.failures, maxBackoffShift)
	if d <= 0 || d > p.maxBackoff {
		return p.maxBackoff
	}
	return d
}

func (p *Poller) noteFailure(err error) {
	kind := domain.KindOf(err)
	// Log the first failure and then only each time the count doubles.
	if p.failures&(p.failures-1) == 0 {
		p.log.Warn("inbox poll failed",
			slog.String("kind", kind.String()), slog.Int("failures", p.failures),
			slog.Duration("retry_in", p.backoff()), slog.String("error", err.Error()))
	} else {
		p.log.Debug("inbox poll failed again", slog.Int("failures", p.failures))
	}
	if kind != p.lastKind {
		p.lastKind = kind
		p.report(err)
	}
}

func (p *Poller) report(err error) {
	if p.reporter != nil {
		p.reporter.ReportInbox(p.ch.ID, err)
	}
}

// purgeTombstones drops consumed parts older than the retention, at most once
// per purgeEvery. It writes only when there is something to remove.
func (p *Poller) purgeTombstones(ctx context.Context) {
	now := p.clk.Now()
	if !p.lastPurge.IsZero() && now.Sub(p.lastPurge) < purgeEvery {
		return
	}
	p.lastPurge = now
	n, err := p.st.InboundParts().PurgeConsumed(ctx, now.Add(-p.partRetention))
	switch {
	case err != nil && ctx.Err() == nil:
		p.log.Warn("purging consumed inbox parts failed", slog.String("error", err.Error()))
	case n > 0:
		p.log.Debug("purged consumed inbox parts", slog.Int("count", n))
	}
}

// PollOnce runs one cycle: fetch, store, delete. It does no back-off and no
// reporting; Run does. Cycles of one Poller never overlap.
func (p *Poller) PollOnce(ctx context.Context) (CycleResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var res CycleResult
	items, err := p.conn.FetchInbox(ctx, p.ref)
	if err != nil {
		return res, fmt.Errorf("inbox: fetch: %w", err)
	}
	res.Fetched = len(items)

	proc, err := p.process(ctx, items)
	res.Stored, res.Duplicates, res.Pending, res.Expired = proc.stored, proc.duplicates, proc.pending, proc.expired
	if err != nil {
		return res, err
	}
	if p.onMessage != nil {
		for _, m := range proc.created {
			p.onMessage(m)
		}
	}

	// Every fetched entry is now either a stored message, a stored part or a
	// recognised duplicate, so all of them may leave the router.
	n, err := p.deleteRemote(ctx, items)
	res.Deleted = n
	return res, err
}

// deleteRemote deletes the entries from the device in chunks.
func (p *Poller) deleteRemote(ctx context.Context, items []connector.InboundSMS) (int, error) {
	seen := make(map[string]bool, len(items))
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it.RemoteID != "" && !seen[it.RemoteID] {
			seen[it.RemoteID] = true
			ids = append(ids, it.RemoteID)
		}
	}
	deleted := 0
	for len(ids) > 0 {
		chunk := ids[:min(deleteChunk, len(ids))]
		if err := p.conn.DeleteInbox(ctx, p.ref, chunk); err != nil {
			return deleted, fmt.Errorf("inbox: delete: %w", err)
		}
		deleted += len(chunk)
		ids = ids[len(chunk):]
	}
	return deleted, nil
}
