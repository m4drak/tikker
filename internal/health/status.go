// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package health

import (
	"sort"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// ChannelStatus is the live health of one channel (T23 /readyz, T31 UI).
type ChannelStatus struct {
	ChannelID domain.ID
	DeviceID  domain.ID
	Port      string
	Enabled   bool

	State  domain.ChannelState
	Reason string
	// Operator and Technology are the last values reported by the modem.
	Operator   string
	Technology string
	// Signal is quantized; nil when unknown.
	Signal *domain.Signal

	// Checked is true once a check was applied since start (or enabling).
	Checked       bool
	LastCheckAt   time.Time
	LastSuccessAt time.Time
	LastFailureAt time.Time
	// LastError describes the last failed check or Auth report (short, no
	// secrets); empty after a success.
	LastError string
	// ConsecutiveFailures and ConsecutiveSuccesses are the running streaks
	// the hysteresis counts.
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	// AuthPending is true while an Auth error is not yet cleared by a
	// successful Health and Probe.
	AuthPending bool
}

// Usable reports whether the channel takes messages.
func (s ChannelStatus) Usable() bool { return s.Enabled && s.State == domain.ChannelOK }

func (e *entry) statusLocked() ChannelStatus {
	return ChannelStatus{
		ChannelID: e.id, DeviceID: e.spec.Channel.DeviceID, Port: e.spec.Channel.Port,
		Enabled: e.spec.Channel.Enabled,
		State:   e.state, Reason: e.reason,
		Operator: e.operator, Technology: e.technology, Signal: copySignal(e.signal),
		Checked: e.checked, LastCheckAt: e.lastCheck, LastSuccessAt: e.lastSuccess,
		LastFailureAt: e.lastFailure, LastError: e.lastError,
		ConsecutiveFailures: e.failStreak, ConsecutiveSuccesses: e.okStreak,
		AuthPending: e.authPending,
	}
}

// Snapshot returns the state of every channel, ordered by channel ID.
func (c *Checker) Snapshot() []ChannelStatus {
	c.mu.Lock()
	out := make([]ChannelStatus, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.statusLocked())
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out
}

// Status returns the state of one channel.
func (c *Checker) Status(id domain.ID) (ChannelStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[id]
	if e == nil {
		return ChannelStatus{}, false
	}
	return e.statusLocked(), true
}

// ApplyTo overlays the live health on ch (State, StateReason, Operator,
// Signal), for building dispatcher specs from rows read from the store. It
// reports whether the channel is known to the checker.
func (c *Checker) ApplyTo(ch *domain.Channel) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[ch.ID]
	if e == nil {
		return false
	}
	ch.State, ch.StateReason = e.state, e.reason
	ch.Operator = e.operator
	ch.Signal = copySignal(e.signal)
	if e.persistedAt.After(ch.LastHealthAt) {
		ch.LastHealthAt = e.persistedAt
	}
	return true
}
