// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package mailgw

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Limit is a token bucket: Burst attempts at once, then one every Every.
type Limit struct {
	Burst int
	Every time.Duration
}

// DefaultAuthLimit bounds failed AUTH attempts per client address: 10, then
// one every 30 s. A token has ~190 bits of entropy, so this is not what
// keeps it safe; it keeps a scanner from burning CPU and filling the logs.
var DefaultAuthLimit = Limit{Burst: 10, Every: 30 * time.Second}

// maxLimiterEntries bounds the memory of the limiter.
const maxLimiterEntries = 10000

// authLimiter is a GCRA token bucket per client address (the theoretical
// arrival time is the only state per key). An attempt is taken before the
// token is checked, so concurrent guesses on many connections cannot all
// pass, and given back when the token was valid.
type authLimiter struct {
	mu    sync.Mutex
	limit Limit
	tau   time.Duration
	tat   map[string]time.Time
}

func newAuthLimiter(l Limit) *authLimiter {
	return &authLimiter{limit: l, tau: time.Duration(l.Burst-1) * l.Every, tat: map[string]time.Time{}}
}

// take consumes one attempt for key; when none is left it returns false
// and the time until one is.
func (l *authLimiter) take(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tat, exists := l.tat[key]
	if tat.Before(now) {
		tat = now
	}
	if d := tat.Sub(now) - l.tau; d > 0 {
		return d, false
	}
	if !exists && len(l.tat) >= maxLimiterEntries {
		l.evict(now)
	}
	l.tat[key] = tat.Add(l.limit.Every)
	return 0, true
}

// refund gives an attempt back (the credentials were valid).
func (l *authLimiter) refund(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tat, ok := l.tat[key]
	if !ok {
		return
	}
	if tat = tat.Add(-l.limit.Every); !tat.After(now) {
		delete(l.tat, key)
		return
	}
	l.tat[key] = tat
}

// evict drops full buckets, or the entry closest to full when every entry
// is still limited (dropping state loosens the limit for one key only).
func (l *authLimiter) evict(now time.Time) {
	var victim string
	var oldest time.Time
	for k, tat := range l.tat {
		if !tat.After(now) {
			delete(l.tat, k)
			continue
		}
		if victim == "" || tat.Before(oldest) {
			victim, oldest = k, tat
		}
	}
	if len(l.tat) >= maxLimiterEntries && victim != "" {
		delete(l.tat, victim)
	}
}

// clientKey is the limiter key of a remote address: the IP, or its /64 for
// IPv6 (one host usually owns a whole /64).
func clientKey(addr net.Addr) string {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return addr.String()
	}
	ip := ap.Addr().Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

// isLoopback reports whether addr is a loopback address.
func isLoopback(addr net.Addr) bool {
	ap, err := netip.ParseAddrPort(addr.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}
