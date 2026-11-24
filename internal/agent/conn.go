// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/logging"
)

// Conn is the connector of one agent device, a handle on the device's
// mailbox in the [Hub]. It is safe for concurrent use.
type Conn struct {
	hub  *Hub
	s    *session
	caps connector.Capabilities
	clk  clock.Clock
	log  *slog.Logger

	portsMu sync.Mutex
	ports   map[string]chan struct{} // send slots per port

	closed atomic.Bool
}

var _ connector.Connector = (*Conn)(nil)

// Capabilities implements connector.Connector.
func (c *Conn) Capabilities() connector.Capabilities {
	caps := c.caps
	caps.Encodings = append([]domain.Encoding(nil), caps.Encodings...)
	return caps
}

// Close implements connector.Connector. The device's mailbox is dropped when
// its last connector is closed; Sends still waiting on it then fail.
func (c *Conn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.hub.release(c.s)
	}
	return nil
}

func (c *Conn) errClosed(op string) error {
	return &domain.ConnectorError{Kind: domain.KindTransient, Op: op, Err: ErrClosed}
}

func (c *Conn) portSlot(port string) chan struct{} {
	c.portsMu.Lock()
	defer c.portsMu.Unlock()
	s, ok := c.ports[port]
	if !ok {
		s = make(chan struct{}, c.caps.MaxConcurrentSends)
		c.ports[port] = s
	}
	return s
}

// validate checks msg against Capabilities and builds its PDUs, so that a
// message the agent cannot send fails permanently without being queued.
func (c *Conn) validate(ch connector.ChannelRef, msg connector.OutboundSMS) ([]SubmitPDU, error) {
	perm := func(err error) error { return domain.Permanent(opSend, err) }
	if ch.Port == "" {
		return nil, perm(fmt.Errorf("%w: channel has no port", ErrInvalidMessage))
	}
	if msg.MessageID.IsZero() {
		return nil, perm(fmt.Errorf("%w: no message id (needed for deduplication)", ErrInvalidMessage))
	}
	if strings.TrimSpace(msg.To) == "" {
		return nil, perm(fmt.Errorf("%w: no recipient", ErrInvalidMessage))
	}
	enc := msg.Encoding
	if enc == "" {
		enc = domain.EncodingGSM7
	}
	if !c.caps.SupportsEncoding(enc) {
		return nil, perm(fmt.Errorf("%w: encoding %s not enabled for the agent", ErrInvalidMessage, enc))
	}
	pdus, err := BuildSubmitPDUs(msg.To, msg.Body, enc, refOf(msg.MessageID))
	if err != nil {
		return nil, perm(fmt.Errorf("%w: %w", ErrInvalidMessage, err))
	}
	segs := max(msg.Segments, len(pdus))
	if segs > c.caps.MaxSegments {
		return nil, perm(fmt.Errorf("%w: %d segments, at most %d", ErrInvalidMessage, segs, c.caps.MaxSegments))
	}
	return pdus, nil
}

// refOf derives the concatenation reference from the message ID, so that a
// redelivered message carries the same reference.
func refOf(id domain.ID) byte {
	sum := sha256.Sum256([]byte(id))
	return sum[0]
}

// Send implements connector.Connector. It queues msg in the device's mailbox
// and waits until the agent reports a result, ctx ends or SendTimeout
// passes. A command the agent never fetched fails as a plain Transient error
// (nothing was sent); one it fetched without reporting fails as
// TransientAmbiguous. A message ID with a remembered final result (accepted
// or permanent) returns that result at once: the redelivery after an
// ambiguous attempt does not reach the router again.
func (c *Conn) Send(ctx context.Context, ch connector.ChannelRef, msg connector.OutboundSMS) (connector.SendResult, error) {
	log := c.log.With(
		slog.String("port", ch.Port), slog.String("channel_id", ch.ChannelID.String()),
		slog.String("message_id", msg.MessageID.String()), logging.PhoneAttr("to", msg.To),
	)
	pdus, err := c.validate(ch, msg)
	if err != nil {
		log.Warn("sms rejected before sending", slog.Any("error", err))
		return connector.SendResult{}, err
	}
	if c.closed.Load() {
		return connector.SendResult{}, c.errClosed(opSend)
	}
	if err := ctx.Err(); err != nil {
		return connector.SendResult{}, domain.Transient(opSend, err)
	}
	slot := c.portSlot(ch.Port)
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return connector.SendResult{}, &domain.ConnectorError{Kind: domain.KindTransient, Op: opSend, Code: domain.CodeTimeout,
			Err: fmt.Errorf("waiting for port %s: %w", ch.Port, ctx.Err())}
	}
	defer func() { <-slot }()

	msg.Encoding = cmpOr(msg.Encoding, domain.EncodingGSM7)
	msg.Segments = max(msg.Segments, len(pdus))
	cmd, res, ok, err := c.s.enqueue(ch.Port, msg, pdus)
	if err != nil {
		return connector.SendResult{}, c.errClosed(opSend)
	}
	if ok {
		log.Info("sms already handled by the agent; using the remembered result", slog.String("status", string(res.status)))
		return c.outcome(log, res)
	}
	log.Debug("sms queued for the agent")

	timer := c.clk.After(c.caps.SendTimeout)
	var cause error
	select {
	case <-cmd.done:
		return c.outcome(log, cmd.out)
	case <-ctx.Done():
		cause = ctx.Err()
	case <-timer:
		cause = context.DeadlineExceeded
	case <-c.s.done:
		cause = ErrClosed
	}
	out, resolved, fetched := c.s.abandon(cmd)
	if resolved {
		return c.outcome(log, out)
	}
	if !fetched {
		err := &domain.ConnectorError{Kind: domain.KindTransient, Op: opSend, Code: domain.CodeTimeout,
			Err: fmt.Errorf("%w: %w", ErrNotFetched, cause)}
		log.Warn("sms not sent: the agent did not poll", slog.Any("error", err))
		return connector.SendResult{}, err
	}
	err = &domain.ConnectorError{Kind: domain.KindTransient, Op: opSend, Code: domain.CodeTimeout, Ambiguous: true,
		Err: fmt.Errorf("%w: %w", ErrNoResult, cause)}
	log.Warn("sms fate unknown: the agent fetched it but did not report", slog.Any("error", err))
	return connector.SendResult{}, err
}

func cmpOr[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// outcome turns the agent's result into Send's return values.
//
// A command handed to the agent more than once (redelivered after
// RedeliverAfter) may have been sent twice by an agent that does not
// deduplicate, so every outcome of it is ambiguous (ADR 0002): accepted and
// transient become TransientAmbiguous, permanent stays permanent with the
// ambiguous flag. The dispatcher then records the possible duplicate; for
// accepted, the retry completes at once from the remembered result.
func (c *Conn) outcome(log *slog.Logger, o outcome) (connector.SendResult, error) {
	cause := errors.New("agent reported " + string(o.status))
	if o.reason != "" {
		cause = fmt.Errorf("agent reported %s: %s", o.status, o.reason)
	}
	if o.deliveries > 1 {
		err := &domain.ConnectorError{Kind: domain.KindTransient, Op: opSend, Ambiguous: true,
			Err: fmt.Errorf("%w: command handed to the agent %d times: %w", ErrRedelivered, o.deliveries, cause)}
		if o.status == statusPermanent {
			err.Kind = domain.KindPermanent
		}
		log.Warn("possible duplicate: the command was handed to the agent more than once",
			slog.String("status", string(o.status)), slog.Int("deliveries", o.deliveries), slog.Any("error", err))
		return connector.SendResult{}, err
	}
	var err error
	switch o.status {
	case statusAccepted:
		at := o.at
		if at.IsZero() {
			at = c.clk.Now()
		}
		log.Info("sms accepted by the agent")
		return connector.SendResult{RemoteRef: o.remoteRef, AcceptedAt: at}, nil
	case statusPermanent:
		err = domain.Permanent(opSend, cause)
	case statusAmbiguous:
		err = domain.TransientAmbiguous(opSend, cause)
	default:
		err = domain.Transient(opSend, cause)
	}
	log.Warn("sms send failed", slog.Any("error", err), slog.String("kind", domain.KindOf(err).String()),
		slog.Bool("ambiguous", domain.IsAmbiguous(err)))
	return connector.SendResult{}, err
}

// FetchInbox implements connector.Connector: the SMS the agent pushed for
// ch.Port and tikker has not deleted yet, in arrival order.
func (c *Conn) FetchInbox(_ context.Context, ch connector.ChannelRef) ([]connector.InboundSMS, error) {
	if c.closed.Load() {
		return nil, c.errClosed(opFetchInbox)
	}
	out, err := c.s.fetch(ch.Port)
	if err != nil {
		return nil, c.errClosed(opFetchInbox)
	}
	if len(out) == 0 {
		return nil, nil
	}
	c.log.Debug("inbox fetched", slog.String("port", ch.Port), slog.Int("count", len(out)))
	return out, nil
}

// DeleteInbox implements connector.Connector. The entries leave the mailbox
// and are listed to the agent, which removes them on the router and
// acknowledges (POST /agent/v1/inbox/ack).
func (c *Conn) DeleteInbox(_ context.Context, ch connector.ChannelRef, remoteIDs []string) error {
	if c.closed.Load() {
		return c.errClosed(opDeleteInbox)
	}
	if err := c.s.markDeleted(ch.Port, remoteIDs); err != nil {
		if errors.Is(err, ErrClosed) {
			return c.errClosed(opDeleteInbox)
		}
		c.log.Warn("inbox deletions pending on the agent are at their limit; entries stay buffered",
			slog.String("port", ch.Port), slog.Any("error", err))
		return domain.Transient(opDeleteInbox, err)
	}
	return nil
}

// Health implements connector.Connector from the last check-in. Before the
// first check-in, or when it is older than StaleAfter, the channel is down
// (Transient, code unreachable).
func (c *Conn) Health(_ context.Context, ch connector.ChannelRef) (connector.ChannelHealth, error) {
	if c.closed.Load() {
		return connector.ChannelHealth{}, c.errClosed(opHealth)
	}
	r, err := c.s.state()
	if err != nil {
		return connector.ChannelHealth{}, c.stateError(opHealth, err)
	}
	for _, p := range r.ports {
		if p.Port == ch.Port {
			return p.Health, nil
		}
	}
	return connector.ChannelHealth{}, domain.Permanent(opHealth, fmt.Errorf("%w: %q", ErrUnknownPort, ch.Port))
}

func (c *Conn) stateError(op string, err error) error {
	if errors.Is(err, ErrClosed) {
		return c.errClosed(op)
	}
	return &domain.ConnectorError{Kind: domain.KindTransient, Op: op, Code: domain.CodeUnreachable, Err: err}
}

// Probe implements connector.Connector with what the agent reported at its
// last check-in. A stale check-in is returned together with a Transient
// error.
func (c *Conn) Probe(_ context.Context) (connector.DeviceProbe, error) {
	if c.closed.Load() {
		return connector.DeviceProbe{}, domain.WithCode(c.errClosed(opProbe), domain.CodeUnreachable)
	}
	r, err := c.s.state()
	var p connector.DeviceProbe
	if r != nil {
		p = connector.DeviceProbe{RouterOSVersion: r.version, BoardName: r.board, Ports: r.ports}
	}
	if err != nil {
		err = domain.WithCode(c.stateError(opProbe, err), domain.CodeUnreachable)
		c.log.Info("device probe failed", slog.Any("error", err))
		return p, err
	}
	return p, nil
}

// LastCheckin returns the time of the last check-in (zero before the first).
func (c *Conn) LastCheckin() time.Time { return c.s.snapshot().lastCheckin }
