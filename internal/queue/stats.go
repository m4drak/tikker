// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"slices"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// Counters are monotonically increasing totals since the dispatcher was
// created.
type Counters struct {
	// Sent: sends accepted by the device and recorded as sent.
	Sent int64
	// Failed: messages moved to failed after a send (permanent error or
	// retry budget exhausted).
	Failed int64
	// Retried: messages returned to the queue after a failed send.
	Retried int64
	// PossibleDuplicates: outcomes recorded with the possible-duplicate
	// flag (ambiguous send errors, sends interrupted by shutdown, recovered
	// leases).
	PossibleDuplicates int64
	// LeaseLost: results that could not be recorded because the lease had
	// already been recovered (ADR 0002 fencing).
	LeaseLost int64
	// StoreErrors: failed store calls (acquire, next due, result writes).
	StoreErrors int64
	// AuthErrors: sends rejected with an Auth error (channel degraded).
	AuthErrors int64
}

func (c *Counters) add(o Counters) {
	c.Sent += o.Sent
	c.Failed += o.Failed
	c.Retried += o.Retried
	c.PossibleDuplicates += o.PossibleDuplicates
	c.LeaseLost += o.LeaseLost
	c.StoreErrors += o.StoreErrors
	c.AuthErrors += o.AuthErrors
}

// ChannelStats is the dispatcher's view of one channel.
type ChannelStats struct {
	ChannelID domain.ID
	// State is the effective state used by the worker: the state last
	// given by SetChannels/SetChannelState, or degraded after an Auth
	// error.
	State       domain.ChannelState
	StateReason string
	Enabled     bool
	// Active is true when the worker may take leases (enabled, state ok,
	// connector set).
	Active bool
	// Sending is true while a Send is in flight.
	Sending bool
	Counters
	LastAttemptAt time.Time        // start of the latest send attempt
	LastSentAt    time.Time        // latest successful send
	LastErrorAt   time.Time        // latest failed send
	LastErrorKind domain.ErrorKind // kind of the latest failed send
	LastError     string           // text of the latest failed send (no bodies)
	LastMessageID domain.ID        // message of the latest send attempt
}

// Stats is a point-in-time snapshot for diagnostics (T23) and the UI (T31).
type Stats struct {
	Started bool
	Closed  bool
	// Channels lists the channels currently configured, ordered by ID.
	Channels []ChannelStats
	// Totals covers all channels, including removed ones, plus lease
	// recoveries in PossibleDuplicates.
	Totals Counters
	// RecoveredLeases counts messages handled by RecoverExpiredLeases.
	RecoveredLeases int64
	// Expired counts messages failed by ExpireQueued.
	Expired int64
	// LastRecoverAt and LastExpireAt are the latest janitor runs.
	LastRecoverAt time.Time
	LastExpireAt  time.Time
}

// statsBook holds the mutable counters. Channel entries survive worker
// restarts; Stats reports only configured channels.
type statsBook struct {
	mu        sync.Mutex
	channels  map[domain.ID]*ChannelStats
	totals    Counters
	recovered int64
	expired   int64
	recoverAt time.Time
	expireAt  time.Time
}

func newStatsBook() *statsBook {
	return &statsBook{channels: make(map[domain.ID]*ChannelStats)}
}

// update applies fn to the entry of id and adds delta to its counters and to
// the totals.
func (s *statsBook) update(id domain.ID, delta Counters, fn func(*ChannelStats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.channels[id]
	if c == nil {
		c = &ChannelStats{ChannelID: id}
		s.channels[id] = c
	}
	c.add(delta)
	s.totals.add(delta)
	if fn != nil {
		fn(c)
	}
}

func (s *statsBook) janitor(fn func(s *statsBook)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// snapshot builds Stats for the given channel views (state from workers).
func (s *statsBook) snapshot(views []ChannelStats) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Stats{
		Totals:          s.totals,
		RecoveredLeases: s.recovered,
		Expired:         s.expired,
		LastRecoverAt:   s.recoverAt,
		LastExpireAt:    s.expireAt,
	}
	for _, v := range views {
		if c := s.channels[v.ChannelID]; c != nil {
			v.Counters = c.Counters
			v.Sending = c.Sending
			v.LastAttemptAt = c.LastAttemptAt
			v.LastSentAt = c.LastSentAt
			v.LastErrorAt = c.LastErrorAt
			v.LastErrorKind = c.LastErrorKind
			v.LastError = c.LastError
			v.LastMessageID = c.LastMessageID
		}
		out.Channels = append(out.Channels, v)
	}
	slices.SortFunc(out.Channels, func(a, b ChannelStats) int {
		switch {
		case a.ChannelID < b.ChannelID:
			return -1
		case a.ChannelID > b.ChannelID:
			return 1
		}
		return 0
	})
	return out
}
