// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"math"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
)

// RateLimit is the per-token request budget of the API (SPEC §4.5): a token
// bucket that holds up to Requests requests and refills at Requests per
// Window. A negative Requests disables the limit.
type RateLimit struct {
	Requests int
	Window   time.Duration
}

// Defaults of [RateLimit] (the contract's examples use 120 requests per 60 s).
const (
	DefaultRateRequests = 120
	DefaultRateWindow   = time.Minute
	// limiterSweepSize is the number of tracked tokens above which idle
	// buckets are dropped.
	limiterSweepSize = 4096
)

func (l RateLimit) withDefaults() RateLimit {
	if l.Requests == 0 {
		l.Requests = DefaultRateRequests
	}
	if l.Window <= 0 {
		l.Window = DefaultRateWindow
	}
	return l
}

// rateState is what a response reports in the RateLimit-* headers.
type rateState struct {
	Limit      int
	Remaining  int
	ResetSecs  int // seconds until the bucket is full again
	RetryAfter int // seconds until the next request is admitted (denied only)
	Allowed    bool
}

type bucket struct {
	tokens float64
	last   time.Time
}

// limiter keeps one in-memory token bucket per API token. State is lost on
// restart, which only gives a client a fresh budget.
type limiter struct {
	cfg RateLimit
	clk clock.Clock

	mu      sync.Mutex
	buckets map[domain.ID]*bucket
}

func newLimiter(cfg RateLimit, clk clock.Clock) *limiter {
	return &limiter{cfg: cfg.withDefaults(), clk: clk, buckets: make(map[domain.ID]*bucket)}
}

// take consumes one request of token id's budget.
func (l *limiter) take(id domain.ID) rateState {
	if l.cfg.Requests < 0 {
		return rateState{Allowed: true}
	}
	capacity := float64(l.cfg.Requests)
	rate := capacity / l.cfg.Window.Seconds() // tokens per second
	now := l.clk.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[id]
	if b == nil {
		if len(l.buckets) >= limiterSweepSize {
			l.sweepLocked(now, capacity, rate)
		}
		b = &bucket{tokens: capacity, last: now}
		l.buckets[id] = b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(capacity, b.tokens+el*rate)
	}
	b.last = now

	st := rateState{Limit: l.cfg.Requests}
	if b.tokens >= 1 {
		b.tokens--
		st.Allowed = true
	} else {
		st.RetryAfter = max(1, int(math.Ceil((1-b.tokens)/rate)))
	}
	st.Remaining = int(math.Floor(b.tokens))
	st.ResetSecs = int(math.Ceil((capacity - b.tokens) / rate))
	return st
}

// sweepLocked drops buckets that would be full by now: forgetting them is
// indistinguishable from keeping them.
func (l *limiter) sweepLocked(now time.Time, capacity, rate float64) {
	for id, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*rate >= capacity {
			delete(l.buckets, id)
		}
	}
}
