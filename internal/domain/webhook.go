// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"slices"
	"time"
)

// WebhookEvent is a kind of event a webhook can subscribe to.
type WebhookEvent string

// WebhookEvent values.
const (
	// EventIncomingSMS: a new inbound message was stored.
	EventIncomingSMS WebhookEvent = "incoming_sms"
	// EventMessageStatus: an outbound message reached sent or failed.
	EventMessageStatus WebhookEvent = "message_status"
)

// AllWebhookEvents lists every event.
var AllWebhookEvents = []WebhookEvent{EventIncomingSMS, EventMessageStatus}

// ParseWebhookEvent parses s as a [WebhookEvent].
func ParseWebhookEvent(s string) (WebhookEvent, error) {
	return parseEnum[WebhookEvent]("webhook event", s)
}

// String returns the event name.
func (e WebhookEvent) String() string { return string(e) }

// Valid reports whether e is a known event.
func (e WebhookEvent) Valid() bool { return e == EventIncomingSMS || e == EventMessageStatus }

// MarshalText implements encoding.TextMarshaler.
func (e WebhookEvent) MarshalText() ([]byte, error) { return marshalEnum("webhook event", e) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (e *WebhookEvent) UnmarshalText(b []byte) error { return unmarshalEnum("webhook event", b, e) }

// WebhookDeliveryStatus is the state of one [WebhookDelivery].
type WebhookDeliveryStatus string

// WebhookDeliveryStatus values.
const (
	// DeliveryPending: not delivered yet; will be (re)tried at NextAttemptAt.
	DeliveryPending WebhookDeliveryStatus = "pending"
	// DeliveryDelivered: the endpoint answered 2xx.
	DeliveryDelivered WebhookDeliveryStatus = "delivered"
	// DeliveryFailed: given up (permanent error or attempts exhausted).
	DeliveryFailed WebhookDeliveryStatus = "failed"
)

// String returns the status name.
func (s WebhookDeliveryStatus) String() string { return string(s) }

// Terminal reports whether no further attempt will be made.
func (s WebhookDeliveryStatus) Terminal() bool { return s == DeliveryDelivered || s == DeliveryFailed }

// Webhook is an endpoint that receives events. The signing secret is stored
// encrypted (SecretEnc) and never leaves the server after creation.
type Webhook struct {
	ID        ID
	Name      string
	URL       string
	SecretEnc []byte
	Events    []WebhookEvent
	Enabled   bool
	CreatedAt time.Time
}

// Subscribes reports whether w is enabled and listens to ev.
func (w Webhook) Subscribes(ev WebhookEvent) bool {
	return w.Enabled && slices.Contains(w.Events, ev)
}

// WebhookDelivery is one event to deliver to one webhook. The payload is
// frozen when the delivery is created, so retries send identical bytes. ID is
// sent as X-Tikker-Delivery and is the receiver's idempotency key.
type WebhookDelivery struct {
	ID        ID
	WebhookID ID
	Event     WebhookEvent
	Payload   []byte // JSON

	Status        WebhookDeliveryStatus
	Attempts      int       // attempts started; incremented on acquire (fencing token)
	NextAttemptAt time.Time // not acquired before this time
	LeaseUntil    time.Time // set while an attempt is in flight
	LastError     string
	LastHTTPCode  int // 0: no HTTP response (network error, timeout)

	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeliveredAt time.Time
}

// DeliveryLease identifies one in-flight attempt; completing it succeeds only
// while Attempt is still the stored attempt count (see ADR 0002 fencing).
type DeliveryLease struct {
	DeliveryID ID
	Attempt    int
}

// Lease returns the lease of the attempt that acquired d.
func (d WebhookDelivery) Lease() DeliveryLease {
	return DeliveryLease{DeliveryID: d.ID, Attempt: d.Attempts}
}
