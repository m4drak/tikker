// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

type job struct {
	e       *entry
	rec     rec
	persist bool
	push    bool
}

// publish pushes the state of dirty channels to the dispatcher, stores it
// and reports state changes. Calls are serialised; a call that finds nothing
// to do costs no I/O. A failed store write is logged once per outage and
// retried when the next check marks the channel again. It returns ctx's error
// if ctx ended before everything was handled (the rest stays pending).
func (c *Checker) publish(ctx context.Context) error {
	c.pubMu.Lock()
	defer c.pubMu.Unlock()

	c.mu.Lock()
	now := c.clk.Now()
	jobs := make([]job, 0, len(c.dirty))
	for id, e := range c.dirty {
		delete(c.dirty, id)
		cur := e.rec()
		j := job{e: e, rec: cur.clone()}
		j.persist = !cur.equal(e.persisted) ||
			(e.state != domain.ChannelDisabled && now.Sub(e.persistedAt) >= c.opts.RefreshInterval)
		j.push = e.pushed != e.pushRec()
		if j.persist || j.push {
			jobs = append(jobs, j)
		}
	}
	changes := c.changes
	c.changes = nil
	c.mu.Unlock()

	sort.Slice(jobs, func(i, k int) bool { return jobs[i].e.id < jobs[k].e.id })
	for i, j := range jobs {
		if err := ctx.Err(); err != nil {
			c.mu.Lock()
			for _, rest := range jobs[i:] {
				c.dirty[rest.e.id] = rest.e
			}
			c.changes = append(changes, c.changes...)
			c.mu.Unlock()
			return fmt.Errorf("health: publish: %w", err)
		}
		c.publishJob(ctx, j, now)
	}
	for _, ch := range changes {
		c.announce(ch)
	}
	return nil
}

func (c *Checker) publishJob(ctx context.Context, j job, now time.Time) {
	e, id := j.e, j.e.id
	if j.push {
		if c.opts.Dispatcher == nil || c.opts.Dispatcher.SetChannelState(id, j.rec.state, j.rec.reason) {
			c.mu.Lock()
			e.pushed = pushRec{state: j.rec.state, reason: j.rec.reason}
			c.mu.Unlock()
		} // else: the dispatcher does not know the channel (yet); the next check retries
	}
	if !j.persist {
		return
	}
	err := c.repo.UpdateHealth(ctx, id, store.ChannelHealthUpdate{
		State: j.rec.state, StateReason: j.rec.reason, At: now,
		Signal: copySignal(j.rec.signal), Operator: j.rec.operator,
	})
	c.mu.Lock()
	switch {
	case err == nil || errors.Is(err, domain.ErrNotFound):
		// A channel missing from the store (deleted meanwhile) is not worth
		// retrying; SetChannels will drop it.
		e.persisted, e.persistedAt = j.rec, now
		recovered := e.persistErr
		e.persistErr = false
		c.mu.Unlock()
		if recovered {
			c.log.Info("storing channel health works again", slog.String("channel_id", id.String()))
		}
	default:
		first := !e.persistErr
		e.persistErr = true
		c.mu.Unlock()
		if first && ctx.Err() == nil {
			c.log.Error("storing channel health failed", slog.String("channel_id", id.String()),
				slog.Any("error", err))
		}
	}
}

// announce logs a state change and passes it to OnStateChange.
func (c *Checker) announce(ch StateChange) {
	level := slog.LevelWarn
	if ch.To == domain.ChannelOK || ch.To == domain.ChannelDisabled {
		level = slog.LevelInfo
	}
	c.log.Log(context.Background(), level, "channel state changed",
		slog.String("channel_id", ch.ChannelID.String()),
		slog.String("device_id", ch.DeviceID.String()),
		slog.String("from", string(ch.From)), slog.String("to", string(ch.To)),
		slog.String("reason", ch.Reason))
	if c.opts.OnStateChange != nil {
		c.opts.OnStateChange(ch)
	}
}

// ---------------------------------------------------------------------------
// Signal quantization

// apply quantizes raw. prev is the signal published so far: a metric keeps
// its published value while the raw one stays within one step of it (a dead
// band, so a value hovering around a rounding boundary does not flip).
// A step of 1 or less leaves the metric exact.
func (q Quantization) apply(prev, raw *domain.Signal) *domain.Signal {
	if raw == nil {
		return nil
	}
	var p domain.Signal
	if prev != nil {
		p = *prev
	}
	out := domain.Signal{
		RSSI: quantize(p.RSSI, raw.RSSI, q.RSSI),
		RSRP: quantize(p.RSRP, raw.RSRP, q.RSRP),
		RSRQ: quantize(p.RSRQ, raw.RSRQ, q.RSRQ),
		SINR: quantize(p.SINR, raw.SINR, q.SINR),
	}
	if out.RSSI == nil && out.RSRP == nil && out.RSRQ == nil && out.SINR == nil {
		return nil
	}
	return &out
}

func quantize(prev, raw *int, step int) *int {
	if raw == nil {
		return nil
	}
	v := *raw
	switch {
	case step <= 1:
	case prev != nil && abs(v-*prev) < step:
		v = *prev
	default:
		v = int(math.Round(float64(v)/float64(step))) * step
	}
	return &v
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func copySignal(s *domain.Signal) *domain.Signal {
	if s == nil {
		return nil
	}
	cp := func(p *int) *int {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	return &domain.Signal{RSSI: cp(s.RSSI), RSRP: cp(s.RSRP), RSRQ: cp(s.RSRQ), SINR: cp(s.SINR)}
}

func equalSignal(a, b *domain.Signal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return equalInt(a.RSSI, b.RSSI) && equalInt(a.RSRP, b.RSRP) && equalInt(a.RSRQ, b.RSRQ) && equalInt(a.SINR, b.SINR)
}

func equalInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
