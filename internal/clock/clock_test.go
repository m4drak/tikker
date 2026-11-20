// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package clock

import (
	"testing"
	"time"
)

var start = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func TestSystem(t *testing.T) {
	c := System()
	before := time.Now()
	if got := c.Now(); got.Before(before) {
		t.Fatalf("Now() = %v, want >= %v", got, before)
	}
	select {
	case <-c.After(0):
	case <-time.After(5 * time.Second):
		t.Fatal("After(0) did not fire")
	}
}

func TestFakeAfter(t *testing.T) {
	tests := []struct {
		name      string
		after     time.Duration
		advance   time.Duration
		wantFired bool
	}{
		{"zero fires immediately", 0, 0, true},
		{"negative fires immediately", -time.Second, 0, true},
		{"not yet due", 10 * time.Second, 9 * time.Second, false},
		{"exactly due", 10 * time.Second, 10 * time.Second, true},
		{"overdue", 10 * time.Second, time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFake(start)
			ch := f.After(tt.after)
			f.Advance(tt.advance)
			select {
			case got := <-ch:
				if !tt.wantFired {
					t.Fatalf("fired unexpectedly at %v", got)
				}
			default:
				if tt.wantFired {
					t.Fatal("did not fire")
				}
				if f.Waiters() != 1 {
					t.Fatalf("Waiters() = %d, want 1", f.Waiters())
				}
			}
		})
	}
}

func TestFakeSetAndNow(t *testing.T) {
	f := NewFake(start)
	ch := f.After(time.Hour)
	f.Set(start.Add(-time.Hour)) // backwards: nothing fires
	if f.Waiters() != 1 {
		t.Fatalf("Waiters() = %d, want 1", f.Waiters())
	}
	f.Set(start.Add(2 * time.Hour))
	if got := <-ch; !got.Equal(start.Add(2 * time.Hour)) {
		t.Fatalf("fired with %v", got)
	}
	if got := f.Now(); !got.Equal(start.Add(2 * time.Hour)) {
		t.Fatalf("Now() = %v", got)
	}
}

func TestFakeBlockUntil(t *testing.T) {
	f := NewFake(start)
	done := make(chan struct{})
	go func() {
		<-f.After(time.Minute)
		close(done)
	}()
	f.BlockUntil(1)
	f.Advance(time.Minute)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine was not woken")
	}
}
