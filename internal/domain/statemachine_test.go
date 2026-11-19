// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"errors"
	"fmt"
	"testing"
)

var allEvents = []MessageEvent{
	EventAcquire, EventSendSucceeded, EventSendFailedTransient, EventSendFailedPermanent,
	EventLeaseExpired, EventCancel, EventExpire,
}

func TestNextStatusAllowed(t *testing.T) {
	tests := []struct {
		from        MessageStatus
		ev          MessageEvent
		attempts    int
		maxAttempts int
		want        MessageStatus
	}{
		{StatusQueued, EventAcquire, 0, 5, StatusSending},
		{StatusQueued, EventAcquire, 4, 5, StatusSending},
		{StatusQueued, EventCancel, 0, 5, StatusCancelled},
		{StatusQueued, EventCancel, 3, 5, StatusCancelled},
		{StatusQueued, EventExpire, 2, 5, StatusFailed},
		{StatusSending, EventSendSucceeded, 1, 5, StatusSent},
		{StatusSending, EventSendSucceeded, 5, 5, StatusSent},
		{StatusSending, EventSendFailedPermanent, 1, 5, StatusFailed},
		{StatusSending, EventSendFailedTransient, 1, 5, StatusQueued},
		{StatusSending, EventSendFailedTransient, 4, 5, StatusQueued},
		{StatusSending, EventSendFailedTransient, 5, 5, StatusFailed},
		{StatusSending, EventSendFailedTransient, 6, 5, StatusFailed},
		{StatusSending, EventLeaseExpired, 1, 5, StatusQueued},
		{StatusSending, EventLeaseExpired, 5, 5, StatusFailed},
		// maxAttempts < 1 means a single attempt.
		{StatusSending, EventSendFailedTransient, 1, 0, StatusFailed},
		{StatusSending, EventLeaseExpired, 1, -3, StatusFailed},
		{StatusSending, EventSendFailedTransient, 0, 1, StatusQueued},
	}
	for _, tt := range tests {
		name := fmt.Sprintf("%s/%s/%d-of-%d", tt.from, tt.ev, tt.attempts, tt.maxAttempts)
		t.Run(name, func(t *testing.T) {
			got, err := NextStatus(tt.from, tt.ev, tt.attempts, tt.maxAttempts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// TestNextStatusMatrix checks every (status, event) pair: exactly the pairs in
// allowed succeed, everything else is ErrInvalidTransition and leaves the
// status unchanged.
func TestNextStatusMatrix(t *testing.T) {
	allowed := map[MessageStatus]map[MessageEvent]bool{
		StatusQueued:  {EventAcquire: true, EventCancel: true, EventExpire: true},
		StatusSending: {EventSendSucceeded: true, EventSendFailedPermanent: true, EventSendFailedTransient: true, EventLeaseExpired: true},
	}
	statuses := append([]MessageStatus{"", "bogus"}, AllMessageStatuses...)
	events := append([]MessageEvent{0, 200}, allEvents...)
	for _, from := range statuses {
		for _, ev := range events {
			t.Run(fmt.Sprintf("%q/%s", from, ev), func(t *testing.T) {
				got, err := NextStatus(from, ev, 1, 5)
				if allowed[from][ev] {
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					return
				}
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("err = %v, want ErrInvalidTransition", err)
				}
				if got != from {
					t.Fatalf("status changed to %q on error", got)
				}
			})
		}
	}
}

func TestTerminalStatusesHaveNoTransitions(t *testing.T) {
	for _, s := range AllMessageStatuses {
		if !s.Terminal() {
			continue
		}
		for _, ev := range allEvents {
			if _, err := NextStatus(s, ev, 0, 5); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("terminal %s accepted %s", s, ev)
			}
		}
	}
}

func TestMessageEventString(t *testing.T) {
	tests := []struct {
		ev   MessageEvent
		want string
	}{
		{EventAcquire, "acquire"},
		{EventSendSucceeded, "send_succeeded"},
		{EventSendFailedTransient, "send_failed_transient"},
		{EventSendFailedPermanent, "send_failed_permanent"},
		{EventLeaseExpired, "lease_expired"},
		{EventCancel, "cancel"},
		{EventExpire, "expire"},
		{0, "event(0)"},
		{200, "event(200)"},
	}
	for _, tt := range tests {
		if got := tt.ev.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.ev, got, tt.want)
		}
	}
}

func TestEventForSendResult(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want MessageEvent
	}{
		{"success", nil, EventSendSucceeded},
		{"transient", Transient("send", errors.New("timeout")), EventSendFailedTransient},
		{"auth", AuthError("send", errors.New("401")), EventSendFailedTransient},
		{"permanent", Permanent("send", errors.New("bad number")), EventSendFailedPermanent},
		{"wrapped permanent", fmt.Errorf("dispatch: %w", Permanent("send", nil)), EventSendFailedPermanent},
		{"unclassified", errors.New("boom"), EventSendFailedTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EventForSendResult(tt.err); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
