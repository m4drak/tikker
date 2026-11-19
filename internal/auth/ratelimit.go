// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"strings"
	"sync"
	"time"
)

// Limit is a token bucket: Burst attempts at once, refilled with one
// attempt every Every.
type Limit struct {
	Burst int
	Every time.Duration
}

// Default login limits. Per username: 5 attempts, then one per minute
// (at most ~65 guesses in the first hour against one account). Per client
// IP: 20 attempts, then one every 6 s (10/min), which leaves room for a few
// users behind one NAT but slows password spraying across accounts.
var (
	DefaultLoginPerUser = Limit{Burst: 5, Every: time.Minute}
	DefaultLoginPerIP   = Limit{Burst: 20, Every: 6 * time.Second}
)

// DefaultMaxLimiterEntries bounds the memory of the login limiter per
// dimension (an entry is a few dozen bytes).
const DefaultMaxLimiterEntries = 10000

// gcra is one keyed rate limiter using the generic cell rate algorithm, an
// exact token bucket that stores a single timestamp per key: tat, the
// "theoretical arrival time" at which the bucket would be full again. An
// entry whose tat is not after now carries no state and can be dropped.
type gcra struct {
	limit Limit
	tau   time.Duration // burst tolerance: (Burst-1) * Every
	max   int
	tat   map[string]time.Time
}

func newGCRA(l Limit, maxEntries int) *gcra {
	return &gcra{limit: l, tau: time.Duration(l.Burst-1) * l.Every, max: maxEntries, tat: map[string]time.Time{}}
}

// check reports whether one attempt for key is allowed at now, without
// taking it; when not, wait is the time until it would be.
func (g *gcra) check(key string, now time.Time) (wait time.Duration, ok bool) {
	tat := g.tat[key]
	if tat.Before(now) {
		tat = now
	}
	if d := tat.Sub(now) - g.tau; d > 0 {
		return d, false
	}
	return 0, true
}

// take consumes one attempt; the caller checked it first.
func (g *gcra) take(key string, now time.Time) {
	tat, exists := g.tat[key]
	if tat.Before(now) {
		tat = now
	}
	if !exists && len(g.tat) >= g.max {
		g.evict(now)
	}
	g.tat[key] = tat.Add(g.limit.Every)
}

// refund gives one attempt back.
func (g *gcra) refund(key string, now time.Time) {
	tat, ok := g.tat[key]
	if !ok {
		return
	}
	tat = tat.Add(-g.limit.Every)
	if !tat.After(now) {
		delete(g.tat, key)
		return
	}
	g.tat[key] = tat
}

// sweep drops entries that are back to a full bucket.
func (g *gcra) sweep(now time.Time) {
	for k, tat := range g.tat {
		if !tat.After(now) {
			delete(g.tat, k)
		}
	}
}

// evict makes room for one entry: first full buckets go; if every entry
// is still limited, the one closest to full is dropped. Dropping state
// loosens the limit for that key only, whereas refusing new keys would let
// an attacker who sprays usernames lock everybody out.
func (g *gcra) evict(now time.Time) {
	g.sweep(now)
	if len(g.tat) < g.max {
		return
	}
	var victim string
	var oldest time.Time
	for k, tat := range g.tat {
		if victim == "" || tat.Before(oldest) {
			victim, oldest = k, tat
		}
	}
	delete(g.tat, victim)
}

// loginLimiter combines the per-IP and per-username limits. An attempt is
// taken from both buckets atomically, before the password is verified, so
// that concurrent guesses cannot all pass the check.
type loginLimiter struct {
	mu     sync.Mutex
	perIP  *gcra
	perUsr *gcra
}

func newLoginLimiter(perIP, perUser Limit, maxEntries int) *loginLimiter {
	return &loginLimiter{perIP: newGCRA(perIP, maxEntries), perUsr: newGCRA(perUser, maxEntries)}
}

// userKey normalises a username for limiting: usernames are
// case-insensitive, and the key length is bounded.
func userKey(username string) string {
	k := strings.ToLower(username)
	if len(k) > 128 {
		k = k[:128]
	}
	return k
}

// allow takes one attempt for (ip, user). When either bucket is empty
// nothing is taken and retryAfter is the longer of the waits.
func (l *loginLimiter) allow(ip, user string, now time.Time) (retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w1, ok1 := l.perIP.check(ip, now)
	w2, ok2 := l.perUsr.check(user, now)
	if !ok1 || !ok2 {
		return max(w1, w2), false
	}
	l.perIP.take(ip, now)
	l.perUsr.take(user, now)
	return 0, true
}

// succeeded clears the username's failures and returns the IP attempt.
func (l *loginLimiter) succeeded(ip, user string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perIP.refund(ip, now)
	delete(l.perUsr.tat, user)
}

func (l *loginLimiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perIP.sweep(now)
	l.perUsr.sweep(now)
}

func (l *loginLimiter) size() (ips, users int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.perIP.tat), len(l.perUsr.tat)
}
