// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import "time"

// tokenBucket limits sends per channel to ratePerMin with a burst of up to
// burst sends. It is implemented as GCRA (generic cell rate algorithm): one
// "theoretical arrival time" replaces the token count, so the arithmetic is
// exact integer nanoseconds and needs no refill timer.
//
// The bucket is used by a single worker goroutine and is not safe for
// concurrent use. Rate and burst are passed on every call so that a channel
// update takes effect immediately.
type tokenBucket struct {
	tat time.Time // theoretical arrival time of the next send
}

// interval returns the spacing between sends, rounded up so that the rate is
// never exceeded. A non-positive rate means unlimited (0).
func interval(ratePerMin int) time.Duration {
	if ratePerMin <= 0 {
		return 0
	}
	r := time.Duration(ratePerMin)
	return (time.Minute + r - 1) / r
}

// wait returns how long to wait at now before a send is allowed (0: now).
func (b *tokenBucket) wait(now time.Time, ratePerMin, burst int) time.Duration {
	iv := interval(ratePerMin)
	if iv == 0 || b.tat.IsZero() {
		return 0
	}
	allowAt := b.tat.Add(-time.Duration(max(burst, 1)-1) * iv)
	if d := allowAt.Sub(now); d > 0 {
		return d
	}
	return 0
}

// take records a send at now. Callers check wait first.
func (b *tokenBucket) take(now time.Time, ratePerMin int) {
	iv := interval(ratePerMin)
	if iv == 0 {
		return
	}
	start := b.tat
	if start.Before(now) {
		start = now
	}
	b.tat = start.Add(iv)
}
