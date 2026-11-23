// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"encoding/json"
	"testing"
)

func TestWebhookEvent(t *testing.T) {
	tests := []struct {
		in      string
		want    WebhookEvent
		wantErr bool
	}{
		{"incoming_sms", EventIncomingSMS, false},
		{"message_status", EventMessageStatus, false},
		{"", "", true},
		{"INCOMING_SMS", "", true},
		{"sent", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseWebhookEvent(tt.in)
			if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
				t.Fatalf("ParseWebhookEvent(%q) = %q, %v", tt.in, got, err)
			}
			var viaJSON []WebhookEvent
			err = json.Unmarshal([]byte(`["`+tt.in+`"]`), &viaJSON)
			if (err != nil) != tt.wantErr {
				t.Fatalf("UnmarshalJSON(%q) error = %v", tt.in, err)
			}
		})
	}
	for _, e := range AllWebhookEvents {
		if !e.Valid() {
			t.Errorf("%q listed but invalid", e)
		}
	}
}

func TestWebhookSubscribes(t *testing.T) {
	tests := []struct {
		name string
		w    Webhook
		ev   WebhookEvent
		want bool
	}{
		{"listed and enabled", Webhook{Enabled: true, Events: []WebhookEvent{EventIncomingSMS}}, EventIncomingSMS, true},
		{"not listed", Webhook{Enabled: true, Events: []WebhookEvent{EventIncomingSMS}}, EventMessageStatus, false},
		{"disabled", Webhook{Enabled: false, Events: AllWebhookEvents}, EventIncomingSMS, false},
		{"no events", Webhook{Enabled: true}, EventIncomingSMS, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.w.Subscribes(tt.ev); got != tt.want {
				t.Errorf("Subscribes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWebhookDeliveryStatusAndLease(t *testing.T) {
	for s, want := range map[WebhookDeliveryStatus]bool{DeliveryPending: false, DeliveryDelivered: true, DeliveryFailed: true} {
		if s.Terminal() != want {
			t.Errorf("%s.Terminal() = %v", s, !want)
		}
	}
	d := WebhookDelivery{ID: "X", Attempts: 3}
	if l := d.Lease(); l.DeliveryID != "X" || l.Attempt != 3 {
		t.Errorf("Lease = %+v", l)
	}
}
