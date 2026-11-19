// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGCRABurstAndRefill(t *testing.T) {
	g := newGCRA(Limit{Burst: 3, Every: 10 * time.Second}, 100)
	now := t0
	for i := 0; i < 3; i++ {
		if _, ok := g.check("k", now); !ok {
			t.Fatalf("attempt %d refused", i)
		}
		g.take("k", now)
	}
	wait, ok := g.check("k", now)
	if ok || wait != 10*time.Second {
		t.Fatalf("4th attempt: ok %v wait %v", ok, wait)
	}
	if _, ok := g.check("other", now); !ok {
		t.Fatal("other key limited")
	}
	now = now.Add(4 * time.Second)
	if wait, ok := g.check("k", now); ok || wait != 6*time.Second {
		t.Fatalf("after 4s: ok %v wait %v", ok, wait)
	}
	now = now.Add(6 * time.Second)
	if _, ok := g.check("k", now); !ok {
		t.Fatal("not refilled after Every")
	}
	g.take("k", now)
	if _, ok := g.check("k", now); ok {
		t.Fatal("only one token should have been refilled")
	}

	// Refund gives one back; refunding a full bucket drops the entry.
	g.refund("k", now)
	if _, ok := g.check("k", now); !ok {
		t.Fatal("refund did not help")
	}
	g.refund("missing", now)
	later := now.Add(time.Hour)
	g.refund("k", later)
	if _, exists := g.tat["k"]; exists {
		t.Fatal("full bucket kept after refund")
	}

	g.take("a", now)
	g.sweep(now.Add(time.Hour))
	if len(g.tat) != 0 {
		t.Fatalf("sweep left %d entries", len(g.tat))
	}
}

func TestGCRAEviction(t *testing.T) {
	g := newGCRA(Limit{Burst: 1, Every: time.Minute}, 3)
	for i := 0; i < 3; i++ {
		g.take(fmt.Sprint(i), t0.Add(time.Duration(i)*time.Second))
	}
	// Full map, all still limited: the entry closest to full ("0") goes.
	g.take("new", t0.Add(3*time.Second))
	if len(g.tat) != 3 {
		t.Fatalf("size %d", len(g.tat))
	}
	if _, ok := g.tat["0"]; ok {
		t.Fatal("oldest entry not evicted")
	}
	// Once entries are idle, sweeping makes room without eviction.
	g.take("later", t0.Add(time.Hour))
	if len(g.tat) != 1 {
		t.Fatalf("after sweep size %d", len(g.tat))
	}
}

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter(Limit{Burst: 4, Every: time.Minute}, Limit{Burst: 2, Every: time.Minute}, 100)
	now := t0
	tests := []struct {
		ip, user string
		ok       bool
	}{
		{"ip1", "alice", true},
		{"ip1", "alice", true},
		{"ip1", "alice", false}, // user bucket empty
		{"ip1", "bob", true},
		{"ip1", "bob", true},
		{"ip1", "carol", false}, // ip bucket empty
		{"ip2", "carol", true},
	}
	for i, tc := range tests {
		if _, ok := l.allow(tc.ip, tc.user, now); ok != tc.ok {
			t.Fatalf("step %d (%s, %s): ok = %v", i, tc.ip, tc.user, ok)
		}
	}
	// A refusal consumes nothing: ip2 still has 3 attempts.
	ips, users := l.size()
	if ips != 2 || users != 3 {
		t.Fatalf("size = %d, %d", ips, users)
	}
	wait, ok := l.allow("ip3", "alice", now)
	if ok || wait != time.Minute {
		t.Fatalf("alice wait %v", wait)
	}
	// Success clears the user's failures and returns the IP attempt.
	l.succeeded("ip2", "carol", now)
	for i := 0; i < 2; i++ {
		if _, ok := l.allow("ip2", "carol", now); !ok {
			t.Fatalf("carol attempt %d after success refused", i)
		}
	}
	l.sweep(now.Add(time.Hour))
	if ips, users := l.size(); ips != 0 || users != 0 {
		t.Fatalf("after sweep = %d, %d", ips, users)
	}
}

func TestLimiterKeys(t *testing.T) {
	if userKey("Admin") != "admin" || len(userKey(strings.Repeat("x", 500))) != 128 {
		t.Fatal("userKey")
	}
	tests := []struct{ in, want string }{
		{"192.0.2.1", "192.0.2.1"},
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"garbage", "garbage"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := limiterIP(tc.in); got != tc.want {
			t.Errorf("limiterIP(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRateLimitError(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want int
	}{{0, 1}, {time.Millisecond, 1}, {time.Second, 1}, {1500 * time.Millisecond, 2}, {time.Minute, 60}}
	for _, tc := range tests {
		e := &RateLimitError{RetryAfter: tc.d}
		if got := e.RetryAfterSeconds(); got != tc.want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", tc.d, got, tc.want)
		}
		if !strings.Contains(e.Error(), "too many") {
			t.Errorf("Error() = %q", e.Error())
		}
	}
}
