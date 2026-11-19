// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package clock

import (
	"sync"
	"time"
)

// Fake is a manually driven [Clock] for tests. Time only moves when [Fake.Set]
// or [Fake.Advance] is called. It is safe for concurrent use.
//
// Typical use in a test of a goroutine that sleeps via After:
//
//	clk := clock.NewFake(start)
//	go worker.Run(ctx)       // calls clk.After(30 * time.Second)
//	clk.BlockUntil(1)        // wait until the worker is sleeping
//	clk.Advance(30 * time.Second)
type Fake struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

// NewFake returns a [Fake] clock set to start.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Now returns the fake current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel that fires once the fake time reaches Now()+d.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.waiters = append(f.waiters, fakeWaiter{deadline: f.now.Add(d), ch: ch})
	f.cond.Broadcast()
	return ch
}

// Advance moves the fake time forward by d and fires due waiters.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setLocked(f.now.Add(d))
}

// Set moves the fake time to t and fires due waiters. Moving backwards is
// allowed but never fires anything.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setLocked(t)
}

func (f *Fake) setLocked(t time.Time) {
	f.now = t
	pending := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.deadline.After(t) {
			w.ch <- t // buffered, never blocks
			continue
		}
		pending = append(pending, w)
	}
	f.waiters = pending
	f.cond.Broadcast()
}

// Waiters returns the number of pending After channels.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// BlockUntil blocks until at least n After channels are pending. It lets a
// test wait for a goroutine to go to sleep without using time.Sleep.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.waiters) < n {
		f.cond.Wait()
	}
}
