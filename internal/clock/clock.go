// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package clock abstracts wall-clock time so that time-dependent logic
// (leases, retries, rate limits, health checks) can be tested
// deterministically.
//
// Production code receives a [Clock] through its constructor and never calls
// time.Now or time.After directly. Tests inject a [Fake] and advance it
// explicitly.
package clock

import "time"

// Clock is the source of time for the application.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the current time once at least d
	// has elapsed. A non-positive d fires immediately.
	After(d time.Duration) <-chan time.Time
}

// System returns a [Clock] backed by the operating system clock.
func System() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
