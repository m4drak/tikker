// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"errors"
	"math/rand/v2"
	"time"
)

// RandFunc returns a pseudo-random number in [0, 1). Inject a constant
// function in tests to make jitter deterministic.
type RandFunc func() float64

// DefaultRand is the production [RandFunc]. Jitter does not need a
// cryptographic source.
func DefaultRand() float64 { return rand.Float64() } //nolint:gosec // jitter only

// RetryPolicy is the retry schedule for outbound messages (SPEC §4.2):
// exponential backoff with symmetric jitter.
//
// The nominal delay after the n-th failed attempt (n >= 1) is
//
//	min(Initial * Multiplier^(n-1), Max)
//
// and the actual delay is the nominal one scaled by a random factor in
// [1-Jitter, 1+Jitter). With [DefaultRetryPolicy] the nominal delays are
// 30 s, 2 min, 8 min and 30 min for the four retries allowed by
// MaxAttempts=5.
type RetryPolicy struct {
	MaxAttempts int           // total attempts including the first; >= 1
	Initial     time.Duration // nominal delay after the first failure; > 0
	Multiplier  float64       // growth factor; >= 1
	Max         time.Duration // cap on the nominal delay; >= Initial
	Jitter      float64       // relative jitter in [0, 1)
}

// DefaultRetryPolicy returns the default policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 5,
		Initial:     30 * time.Second,
		Multiplier:  4,
		Max:         30 * time.Minute,
		Jitter:      0.2,
	}
}

// ErrInvalidRetryPolicy is returned by [RetryPolicy.Validate].
var ErrInvalidRetryPolicy = errors.New("invalid retry policy")

// Validate checks the policy's invariants.
func (p RetryPolicy) Validate() error {
	switch {
	case p.MaxAttempts < 1,
		p.Initial <= 0,
		p.Multiplier < 1,
		p.Max < p.Initial,
		p.Jitter < 0 || p.Jitter >= 1:
		return ErrInvalidRetryPolicy
	}
	return nil
}

// Delay returns the delay before the next attempt after failedAttempts
// attempts have failed. failedAttempts below 1 is treated as 1. rnd may be
// nil, in which case [DefaultRand] is used.
func (p RetryPolicy) Delay(failedAttempts int, rnd RandFunc) time.Duration {
	if rnd == nil {
		rnd = DefaultRand
	}
	nominal := float64(p.Initial)
	for i := 1; i < failedAttempts && nominal < float64(p.Max); i++ {
		nominal *= p.Multiplier
	}
	nominal = min(nominal, float64(p.Max))

	r := rnd()
	if r < 0 || r >= 1 { // defend against a misbehaving source
		r = 0.5
	}
	d := nominal * (1 + p.Jitter*(2*r-1))
	return max(time.Duration(d), 0)
}

// NextAttemptAt returns now + Delay(failedAttempts, rnd).
func (p RetryPolicy) NextAttemptAt(now time.Time, failedAttempts int, rnd RandFunc) time.Time {
	return now.Add(p.Delay(failedAttempts, rnd))
}
