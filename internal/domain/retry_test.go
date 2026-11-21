// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"errors"
	"testing"
	"time"
)

func constRand(v float64) RandFunc { return func() float64 { return v } }

func TestRetryPolicyDelayDefault(t *testing.T) {
	p := DefaultRetryPolicy()
	if err := p.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	tests := []struct {
		failed int
		r      float64
		want   time.Duration
	}{
		// r = 0.5 means no jitter: nominal schedule.
		{1, 0.5, 30 * time.Second},
		{2, 0.5, 2 * time.Minute},
		{3, 0.5, 8 * time.Minute},
		{4, 0.5, 30 * time.Minute}, // 32 min capped
		{5, 0.5, 30 * time.Minute},
		{1000, 0.5, 30 * time.Minute}, // no overflow
		{0, 0.5, 30 * time.Second},    // treated as 1
		{-1, 0.5, 30 * time.Second},
		// Jitter bounds: [0.8, 1.2) of nominal.
		{1, 0, 24 * time.Second},
		{1, 0.75, 33 * time.Second},
		{2, 0, 96 * time.Second},
		{4, 0, 24 * time.Minute},
		// Misbehaving source falls back to the nominal delay.
		{1, 1, 30 * time.Second},
		{1, -0.1, 30 * time.Second},
	}
	for _, tt := range tests {
		got := p.Delay(tt.failed, constRand(tt.r))
		if got != tt.want {
			t.Errorf("Delay(%d, r=%v) = %v, want %v", tt.failed, tt.r, got, tt.want)
		}
	}
}

func TestRetryPolicyDelayJitterRange(t *testing.T) {
	p := DefaultRetryPolicy()
	lo, hi := 24*time.Second, 36*time.Second
	for i := 0; i < 1000; i++ {
		d := p.Delay(1, nil) // DefaultRand
		if d < lo || d >= hi {
			t.Fatalf("Delay(1) = %v outside [%v, %v)", d, lo, hi)
		}
	}
}

func TestRetryPolicyNoJitter(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 3, Initial: time.Second, Multiplier: 2, Max: time.Hour}
	for _, r := range []float64{0, 0.3, 0.99} {
		if got := p.Delay(3, constRand(r)); got != 4*time.Second {
			t.Errorf("Delay(3, %v) = %v, want 4s", r, got)
		}
	}
}

func TestRetryPolicyNextAttemptAt(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	got := DefaultRetryPolicy().NextAttemptAt(now, 2, constRand(0.5))
	if want := now.Add(2 * time.Minute); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRetryPolicyValidate(t *testing.T) {
	base := DefaultRetryPolicy()
	tests := []struct {
		name   string
		mutate func(*RetryPolicy)
		ok     bool
	}{
		{"default", func(*RetryPolicy) {}, true},
		{"single attempt", func(p *RetryPolicy) { p.MaxAttempts = 1 }, true},
		{"zero attempts", func(p *RetryPolicy) { p.MaxAttempts = 0 }, false},
		{"zero initial", func(p *RetryPolicy) { p.Initial = 0 }, false},
		{"shrinking", func(p *RetryPolicy) { p.Multiplier = 0.5 }, false},
		{"max below initial", func(p *RetryPolicy) { p.Max = time.Second }, false},
		{"negative jitter", func(p *RetryPolicy) { p.Jitter = -0.1 }, false},
		{"full jitter", func(p *RetryPolicy) { p.Jitter = 1 }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			tt.mutate(&p)
			err := p.Validate()
			if tt.ok != (err == nil) {
				t.Fatalf("Validate() = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidRetryPolicy) {
				t.Fatalf("error %v is not ErrInvalidRetryPolicy", err)
			}
		})
	}
}
