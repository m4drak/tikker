// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import "time"

// MailNotification is one inbound SMS to forward by e-mail (ADR 0004). It
// references the message instead of copying it: the text is read when the
// e-mail is sent, so the SMS retention applies to it as well. Delivery
// follows the webhook model (ADR 0002): at-least-once, an attempt holds a
// lease and the attempt counter is the fencing token.
type MailNotification struct {
	ID        ID
	MessageID ID // the inbound message

	// Status uses the delivery states: pending, delivered (the relay
	// accepted the e-mail) or failed.
	Status        WebhookDeliveryStatus
	Attempts      int       // attempts started; incremented on acquire
	NextAttemptAt time.Time // not acquired before this time
	LeaseUntil    time.Time // set while an attempt is in flight
	LastError     string

	CreatedAt time.Time
	UpdatedAt time.Time
	SentAt    time.Time
}

// Lease returns the lease of the attempt that acquired n.
func (n MailNotification) Lease() DeliveryLease {
	return DeliveryLease{DeliveryID: n.ID, Attempt: n.Attempts}
}
