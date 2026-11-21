// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package health

import (
	"sort"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// Reasons that are not derived from a check result.
const (
	reasonAwaiting      = "awaiting first health check"
	reasonNoConnector   = "no connector for the device"
	reasonNotRunning    = "interface not running"
	reasonNoSIM         = "no SIM card"
	reasonNotRegistered = "not registered to a network"
)

// rec is the part of a channel's health that is stored and compared.
type rec struct {
	state    domain.ChannelState
	reason   string
	operator string
	signal   *domain.Signal
}

func (r rec) equal(o rec) bool {
	return r.state == o.state && r.reason == o.reason && r.operator == o.operator &&
		equalSignal(r.signal, o.signal)
}

func (r rec) clone() rec {
	r.signal = copySignal(r.signal)
	return r
}

// pushRec is what the dispatcher knows.
type pushRec struct {
	state  domain.ChannelState
	reason string
}

// entry is the checker's view of one channel. Guarded by Checker.mu.
type entry struct {
	id   domain.ID
	spec ChannelSpec
	grp  *group

	checked    bool // a check was applied since start / enabling
	state      domain.ChannelState
	reason     string
	operator   string
	technology string
	signal     *domain.Signal // quantized

	authPending bool // an Auth error is not yet cleared by Health + Probe
	failStreak  int
	okStreak    int

	lastCheck, lastSuccess, lastFailure time.Time
	lastError                           string
	next                                time.Time // next scheduled check

	persisted   rec
	persistedAt time.Time
	persistErr  bool // the last UpdateHealth failed
	pushed      pushRec
}

func (e *entry) rec() rec {
	return rec{state: e.state, reason: e.reason, operator: e.operator, signal: e.signal}
}

func (e *entry) pushRec() pushRec { return pushRec{state: e.state, reason: e.reason} }

// group is the set of channels of one device. One goroutine checks them one
// after another, so a device never sees two health requests at once.
type group struct {
	key      string
	checkMu  sync.Mutex // held during a check (loop or CheckNow)
	members  map[domain.ID]*entry
	wake     chan struct{} // cap 1
	quit     chan struct{}
	quitOnce sync.Once
	launched bool
}

func newGroup(key string) *group {
	return &group{
		key:     key,
		members: make(map[domain.ID]*entry),
		wake:    make(chan struct{}, 1),
		quit:    make(chan struct{}),
	}
}

func (g *group) stop() { g.quitOnce.Do(func() { close(g.quit) }) }

func (g *group) poke() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// groupKey groups channels by device; a channel without a device gets its
// own group.
func groupKey(ch domain.Channel) string {
	if ch.DeviceID != "" {
		return "device:" + ch.DeviceID.String()
	}
	return "channel:" + ch.ID.String()
}

func (c *Checker) newEntryLocked(s ChannelSpec, now time.Time) *entry {
	ch := s.Channel
	e := &entry{id: ch.ID, spec: s, next: now}
	st, reason := ch.State, ch.StateReason
	switch {
	case !ch.Enabled:
		st, reason = domain.ChannelDisabled, ""
	case !st.Valid() || st == domain.ChannelDisabled:
		st, reason = domain.ChannelDown, reasonAwaiting
	}
	e.state, e.reason = st, reason
	e.operator = ch.Operator
	e.signal = copySignal(ch.Signal)
	e.authPending = st == domain.ChannelDegraded
	// The store and the dispatcher got the channel as the caller read it.
	e.persisted = rec{state: ch.State, reason: ch.StateReason, operator: ch.Operator, signal: copySignal(ch.Signal)}
	e.persistedAt = ch.LastHealthAt
	e.pushed = pushRec{state: ch.State, reason: ch.StateReason}
	c.markLocked(e, now)
	return e
}

// updateEntryLocked applies a new spec to a known channel and reports
// whether its group has to re-plan.
func (c *Checker) updateEntryLocked(e *entry, s ChannelSpec, now time.Time) (changed bool) {
	old := e.spec
	e.spec = s
	ch := s.Channel
	switch {
	case !ch.Enabled:
		e.checked = false
		e.failStreak, e.okStreak = 0, 0
		c.setStateLocked(e, domain.ChannelDisabled, "", now)
		changed = old.Channel.Enabled
	case !old.Channel.Enabled:
		e.checked = false
		e.next = now
		c.setStateLocked(e, domain.ChannelDown, reasonAwaiting, now)
		changed = true
	case old.Connector != s.Connector || old.Channel.Port != ch.Port:
		e.next = now
		changed = true
	}
	c.markLocked(e, now)
	return changed
}

// attachLocked puts e into the group of its device, creating it if needed.
func (c *Checker) attachLocked(e *entry, key string) {
	if e.grp != nil && e.grp.key != key {
		c.detachLocked(e)
	}
	if e.grp == nil {
		g := c.groups[key]
		if g == nil {
			g = newGroup(key)
			c.groups[key] = g
			if c.started {
				c.launchLocked(g)
			}
		}
		g.members[e.id] = e
		e.grp = g
		g.poke()
	}
}

func (c *Checker) detachLocked(e *entry) {
	g := e.grp
	if g == nil {
		return
	}
	delete(g.members, e.id)
	e.grp = nil
	if len(g.members) == 0 {
		g.stop()
		delete(c.groups, g.key)
		return
	}
	g.poke()
}

func (c *Checker) launchLocked(g *group) {
	if g.launched {
		return
	}
	g.launched = true
	c.wg.Add(1)
	go c.runGroup(g)
}

// setStateLocked changes state and reason, records a StateChange for a new
// state and queues the entry for publishing.
func (c *Checker) setStateLocked(e *entry, st domain.ChannelState, reason string, now time.Time) {
	if e.state == st && e.reason == reason {
		return
	}
	if e.state != st {
		c.changes = append(c.changes, StateChange{
			ChannelID: e.id, DeviceID: e.spec.Channel.DeviceID, From: e.state, To: st, Reason: reason, At: now,
		})
		if len(c.changes) > maxChanges {
			c.changes = append([]StateChange(nil), c.changes[len(c.changes)-maxChanges:]...)
		}
	}
	e.state, e.reason = st, reason
	c.dirty[e.id] = e
}

// markLocked queues e for publishing when the store or the dispatcher is
// behind, or the stored timestamp is due for a refresh.
func (c *Checker) markLocked(e *entry, now time.Time) {
	if !e.rec().equal(e.persisted) || e.pushed != e.pushRec() ||
		(e.state != domain.ChannelDisabled && now.Sub(e.persistedAt) >= c.opts.RefreshInterval) {
		c.dirty[e.id] = e
	}
}

// authLocked degrades the channel after an Auth error, bypassing hysteresis.
func (c *Checker) authLocked(e *entry, source string, now time.Time) {
	e.authPending = true
	e.failStreak, e.okStreak = 0, 0
	e.lastFailure = now
	e.lastError = "authentication failed (" + source + ")"
	if e.state != domain.ChannelDegraded {
		c.setStateLocked(e, domain.ChannelDegraded, e.lastError, now)
	}
}

// requestCheckLocked brings the next check forward, but not closer than
// RetryInterval to the previous one.
func (c *Checker) requestCheckLocked(e *entry, now time.Time) {
	t := e.lastCheck.Add(c.opts.RetryInterval)
	if t.Before(now) {
		t = now
	}
	if t.Before(e.next) {
		e.next = t
		if e.grp != nil {
			e.grp.poke()
		}
	}
}

// scheduleLocked sets the time of the next check: quickly while a state
// change is pending, otherwise after Interval.
func (c *Checker) scheduleLocked(e *entry, now time.Time) {
	d := c.opts.Interval
	if (e.failStreak > 0 && e.state != domain.ChannelDown) || (e.okStreak > 0 && e.state != domain.ChannelOK) {
		d = c.opts.RetryInterval
	}
	e.next = now.Add(d)
}

// applyLocked folds one check result into the channel's state.
func (c *Checker) applyLocked(e *entry, o obs, now time.Time) {
	first := !e.checked
	e.checked = true
	e.lastCheck = now
	switch o.kind {
	case obsAuth:
		c.authLocked(e, "health check", now)

	case obsInconclusive:
		// Health worked, but Probe could not confirm the credentials.
		e.okStreak = 0
		e.lastError = truncate(o.err.Error(), maxErrorLen)

	case obsBad:
		e.okStreak = 0
		e.failStreak = min(e.failStreak+1, maxStreak)
		e.lastFailure = now
		e.lastError = o.reason
		if o.err != nil {
			e.lastError = truncate(o.err.Error(), maxErrorLen)
		}
		goingDown := e.state != domain.ChannelDown && (first || e.failStreak >= c.opts.FailThreshold)
		if goingDown || e.state == domain.ChannelDown {
			if o.hasData {
				// The device answered: show what it reports.
				if o.health.Operator != "" {
					e.operator = o.health.Operator
				}
				e.technology = o.health.Technology
				e.signal = c.opts.Quantization.apply(e.signal, o.health.Signal)
			} else {
				e.signal = nil // unreachable: no figures
			}
			c.setStateLocked(e, domain.ChannelDown, o.reason, now)
		}

	case obsGood:
		e.failStreak = 0
		e.lastSuccess = now
		e.lastError = ""
		if o.health.Operator != "" {
			e.operator = o.health.Operator
		}
		e.technology = o.health.Technology
		e.signal = c.opts.Quantization.apply(e.signal, o.health.Signal)
		if e.state == domain.ChannelOK {
			e.okStreak = min(e.okStreak+1, maxStreak)
			break
		}
		if e.authPending && !o.probed {
			e.okStreak = 0 // an Auth error arrived during the check
			break
		}
		e.okStreak = min(e.okStreak+1, maxStreak)
		needed := c.opts.RecoverThreshold
		if e.authPending {
			needed = c.opts.AuthRecoverThreshold
		}
		if first {
			needed = 1
		}
		if e.okStreak >= needed {
			e.authPending = false
			e.okStreak = 0
			c.setStateLocked(e, domain.ChannelOK, "", now)
		}
	}
	c.scheduleLocked(e, now)
	c.markLocked(e, now)
}

// nextDue returns the enabled channel of g whose check is due, or how long
// to wait for the earliest one (negative: nothing to wait for).
func (c *Checker) nextDue(g *group) (domain.ID, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clk.Now()
	ids := make([]domain.ID, 0, len(g.members))
	for id, e := range g.members {
		if e.spec.Channel.Enabled {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "", -1
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := g.members[ids[i]], g.members[ids[j]]
		if !a.next.Equal(b.next) {
			return a.next.Before(b.next)
		}
		return ids[i] < ids[j]
	})
	first := g.members[ids[0]]
	if !first.next.After(now) {
		return ids[0], 0
	}
	return "", first.next.Sub(now)
}

// runGroup checks the channels of one device in turn.
func (c *Checker) runGroup(g *group) {
	defer c.wg.Done()
	for {
		select {
		case <-g.quit:
			return
		case <-c.ctx.Done():
			return
		default:
		}
		select { // a poke before the plan below is covered by it
		case <-g.wake:
		default:
		}
		id, wait := c.nextDue(g)
		if id != "" {
			if _, err := c.check(c.ctx, id); err != nil && c.ctx.Err() != nil {
				return
			}
			continue
		}
		var timer <-chan time.Time
		if wait >= 0 {
			timer = c.clk.After(wait)
		}
		if c.hooks.sleep != nil {
			c.hooks.sleep(g.key, wait)
		}
		select {
		case <-g.quit:
		case <-c.ctx.Done():
		case <-g.wake:
		case <-timer:
		}
		if c.hooks.wake != nil {
			c.hooks.wake(g.key)
		}
	}
}
