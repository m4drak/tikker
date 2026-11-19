// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

// maxErrorLen bounds ChannelStatus.LastError.
const maxErrorLen = 200

type obsKind int

const (
	obsGood         obsKind = iota // modem registered
	obsBad                         // unreachable, not running, not registered
	obsAuth                        // credentials rejected
	obsInconclusive                // Health worked, Probe did not confirm
)

// obs is the interpretation of one Health (and Probe) call.
type obs struct {
	kind    obsKind
	reason  string // obsBad: stable, human-readable cause
	err     error
	health  connector.ChannelHealth
	hasData bool // the device answered (obsBad from a Health snapshot)
	probed  bool // Probe confirmed the credentials
}

// check runs one check of channel id and publishes the outcome. It returns
// ErrUnknownChannel if the channel is not (or no longer) configured, and
// ctx.Err() without recording anything if ctx ends during the call.
func (c *Checker) check(ctx context.Context, id domain.ID) (ChannelStatus, error) {
	c.mu.Lock()
	e := c.entries[id]
	if e == nil || e.grp == nil {
		c.mu.Unlock()
		return ChannelStatus{}, ErrUnknownChannel
	}
	g := e.grp
	c.mu.Unlock()

	g.checkMu.Lock()
	defer g.checkMu.Unlock()

	c.mu.Lock()
	if c.entries[id] != e || e.grp != g {
		c.mu.Unlock()
		return ChannelStatus{}, ErrUnknownChannel
	}
	if !e.spec.Channel.Enabled {
		st := e.statusLocked()
		c.mu.Unlock()
		return st, nil
	}
	conn := e.spec.Connector
	ref := connector.ChannelRef{ChannelID: e.id, Port: e.spec.Channel.Port}
	probe := e.authPending
	c.mu.Unlock()

	o := c.collect(ctx, conn, ref, probe)
	if err := ctx.Err(); err != nil {
		return ChannelStatus{}, err // shutdown or caller gave up: not a failure
	}

	c.mu.Lock()
	now := c.clk.Now()
	if c.entries[id] != e || e.spec.Connector != conn || e.spec.Channel.Port != ref.Port {
		// Reconfigured during the call: the result is about another device.
		if c.entries[id] == e {
			e.next = now
			g.poke()
		}
		st := e.statusLocked()
		c.mu.Unlock()
		return st, nil
	}
	prev := e.state
	c.applyLocked(e, o, now)
	st := e.statusLocked()
	c.mu.Unlock()

	if o.kind == obsBad || o.kind == obsInconclusive {
		// Failures are routine while a modem is down: debug only. State
		// changes are logged (once) by publish.
		c.log.Debug("health check failed", slog.String("channel_id", id.String()),
			slog.String("state", string(prev)), slog.String("reason", o.reason),
			slog.Any("error", o.err))
	}
	_ = c.publish(ctx)
	if c.hooks.checked != nil {
		c.hooks.checked(id)
	}
	return st, nil
}

// collect calls Health and, when the channel has an unresolved Auth error
// and Health succeeded, Probe.
func (c *Checker) collect(ctx context.Context, conn connector.Connector, ref connector.ChannelRef, probe bool) obs {
	if conn == nil {
		return obs{kind: obsBad, reason: reasonNoConnector}
	}
	var h connector.ChannelHealth
	err := c.call(ctx, func(cx context.Context) error {
		var err error
		h, err = conn.Health(cx, ref)
		return err
	})
	if err != nil {
		return failedObs(err)
	}
	o := classify(h)
	if o.kind != obsGood || !probe {
		return o
	}
	err = c.call(ctx, func(cx context.Context) error {
		_, err := conn.Probe(cx)
		return err
	})
	switch {
	case err == nil:
		o.probed = true
	case domain.KindOf(err) == domain.KindAuth:
		return obs{kind: obsAuth, err: err}
	default:
		return obs{kind: obsInconclusive, err: err, health: h}
	}
	return o
}

// call runs fn with the check timeout, measured on the injected clock. The
// timer is armed before fn starts.
func (c *Checker) call(ctx context.Context, fn func(context.Context) error) error {
	cctx, cancel := context.WithCancel(ctx)
	timer := c.clk.After(c.opts.Timeout)
	expired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-timer:
			close(expired)
			cancel()
		case <-cctx.Done():
		}
	}()
	err := fn(cctx)
	cancel()
	<-done
	if err != nil {
		select {
		case <-expired:
			return domain.Transient("health", fmt.Errorf("%w after %s", context.DeadlineExceeded, c.opts.Timeout))
		default:
		}
	}
	return err
}

// failedObs interprets an error from Health or Probe.
func failedObs(err error) obs {
	if domain.KindOf(err) == domain.KindAuth {
		return obs{kind: obsAuth, err: err}
	}
	return obs{kind: obsBad, reason: errReason(err), err: err}
}

// errReason is a stable description of err: it must not vary between
// identical failures, because a changed reason is written to the store.
func errReason(err error) string {
	code := domain.CodeOf(err)
	switch {
	case errors.Is(err, context.DeadlineExceeded), code == domain.CodeTimeout:
		return "health check timed out"
	case code != "":
		return "health check failed: " + string(code)
	}
	return "health check failed (" + string(domain.KindOf(err)) + ")"
}

// classify interprets a successful Health call.
func classify(h connector.ChannelHealth) obs {
	o := obs{kind: obsBad, health: h, hasData: true}
	reg := strings.ToLower(strings.TrimSpace(h.Registration))
	switch {
	case strings.Contains(reg, "no-sim") || strings.Contains(reg, "no sim") ||
		strings.Contains(reg, "nosim") || strings.Contains(reg, "sim-missing") || strings.Contains(reg, "sim missing"):
		o.reason = reasonNoSIM
	case !h.Running:
		o.reason = reasonNotRunning
	case !h.Registered:
		o.reason = reasonNotRegistered
		if s := sanitize(reg); s != "" {
			o.reason += " (" + s + ")"
		}
	default:
		o.kind = obsGood
	}
	return o
}

var unsafeChars = regexp.MustCompile(`[^a-z0-9 _.-]+`)

// sanitize keeps a device-supplied word fit for a stored reason.
func sanitize(s string) string {
	s = unsafeChars.ReplaceAllString(s, "")
	return truncate(s, 32)
}

// truncate shortens s to at most n bytes without splitting a UTF-8 rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
