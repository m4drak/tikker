// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package webhook delivers tikker events to HTTP endpoints configured by the
// administrator: a new inbound SMS and the final status (sent or failed) of an
// outbound message.
//
// # Guarantees
//
// Delivery is at-least-once, like the message queue (ADR 0002). A [Publisher]
// stores one delivery row per subscribed webhook in the same database
// transaction that stores the event (the inbound message, or the result of a
// send), so an event is never lost between the commit and the notification. A
// [Sender] leases due deliveries, POSTs the frozen payload and records the
// outcome; a crash mid-attempt makes the lease expire and the delivery is
// retried. The receiver deduplicates with the X-Tikker-Delivery header, which
// is the same for every attempt of a delivery.
//
// # Request
//
//	POST <url>
//	Content-Type: application/json
//	X-Tikker-Event: incoming_sms | message_status
//	X-Tikker-Delivery: <delivery id, stable across retries; equals "id" in the body>
//	X-Tikker-Signature: sha256=<hex HMAC-SHA256(secret, body)>
//
// The body carries the delivery ID, so the signature covers it; a receiver
// calls [VerifyRequest] to check the signature and that the header matches,
// then deduplicates on the ID.
//
// # Outcome
//
//	2xx                       delivered
//	5xx, 408, 429, timeout,
//	connection error          retried with exponential backoff, then failed
//	other 4xx, 3xx            failed at once (redirects are not followed)
package webhook

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// Headers of a delivery request.
const (
	HeaderEvent     = "X-Tikker-Event"
	HeaderDelivery  = "X-Tikker-Delivery"
	HeaderSignature = "X-Tikker-Signature"
	// SignaturePrefix precedes the hex digest in HeaderSignature.
	SignaturePrefix = "sha256="
)

// Envelope is the JSON body of every delivery. ID is the delivery ID, the
// receiver's idempotency key. It is inside the signed bytes, and the
// X-Tikker-Delivery header must equal it: see [VerifyRequest].
type Envelope struct {
	ID         string              `json:"id"`
	Event      domain.WebhookEvent `json:"event"`
	OccurredAt string              `json:"occurred_at"`
	Message    MessageData         `json:"message"`
}

// MessageData is the message part of an [Envelope]. Body is present for
// inbound messages only: status events carry no text.
type MessageData struct {
	ID                string  `json:"id"`
	Direction         string  `json:"direction"`
	Status            string  `json:"status"`
	ChannelID         *string `json:"channel_id"`
	Peer              string  `json:"peer"`
	Body              *string `json:"body,omitempty"`
	Segments          int     `json:"segments"`
	Attempts          int     `json:"attempts,omitempty"`
	LastError         *string `json:"last_error"`
	PossibleDuplicate bool    `json:"possible_duplicate"`
	ClientRef         *string `json:"client_ref"`
	CreatedAt         string  `json:"created_at"`
	SentAt            *string `json:"sent_at"`
	ReceivedAt        *string `json:"received_at"`
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := rfc3339(t)
	return &s
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// BuildPayload renders the body of delivery id for event ev about message m
// at time at.
func BuildPayload(id domain.ID, ev domain.WebhookEvent, m domain.Message, at time.Time) ([]byte, error) {
	d := MessageData{
		ID: m.ID.String(), Direction: m.Direction.String(), Status: m.Status.String(),
		ChannelID: optString(m.ChannelID.String()), Peer: m.Peer, Segments: m.Segments,
		Attempts: m.Attempts, LastError: optString(m.LastError), PossibleDuplicate: m.PossibleDuplicate,
		ClientRef: optString(m.ClientRef), CreatedAt: rfc3339(m.CreatedAt),
		SentAt: optTime(m.SentAt), ReceivedAt: optTime(m.ReceivedAt),
	}
	if ev == domain.EventIncomingSMS {
		body := m.Body
		d.Body = &body
	}
	b, err := json.Marshal(Envelope{ID: id.String(), Event: ev, OccurredAt: rfc3339(at), Message: d})
	if err != nil {
		return nil, fmt.Errorf("webhook: marshal payload: %w", err)
	}
	return b, nil
}
