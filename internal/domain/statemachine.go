// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package domain

import (
	"fmt"
	"strconv"
)

// MessageEvent is something that happens to an outbound message.
type MessageEvent uint8

// Outbound message events. The transition table (SPEC §4.2, ADR 0002):
//
//	queued  --Acquire----------------------------> sending   (Attempts++)
//	sending --SendSucceeded----------------------> sent
//	sending --SendFailedTransient, att <  max----> queued
//	sending --SendFailedTransient, att >= max----> failed
//	sending --SendFailedPermanent----------------> failed
//	sending --LeaseExpired, att <  max-----------> queued    (PossibleDuplicate)
//	sending --LeaseExpired, att >= max-----------> failed    (PossibleDuplicate)
//	queued  --Cancel-----------------------------> cancelled
//	queued  --Expire-----------------------------> failed    (ExpiresAt passed)
//
// Everything else is [ErrInvalidTransition].
const (
	// EventAcquire: a dispatcher worker took a lease on the message.
	EventAcquire MessageEvent = iota + 1
	// EventSendSucceeded: RouterOS accepted the message.
	EventSendSucceeded
	// EventSendFailedTransient: the send failed with a Transient or Auth
	// error (or an unclassified one).
	EventSendFailedTransient
	// EventSendFailedPermanent: the send failed with a Permanent error.
	EventSendFailedPermanent
	// EventLeaseExpired: the lease ran out without a recorded outcome
	// (crash, kill, lost database write). Whether the SMS left the router
	// is unknown.
	EventLeaseExpired
	// EventCancel: a user cancelled the message through the API or UI.
	EventCancel
	// EventExpire: the message is still queued after its ExpiresAt.
	EventExpire
)

var eventNames = [...]string{
	EventAcquire:             "acquire",
	EventSendSucceeded:       "send_succeeded",
	EventSendFailedTransient: "send_failed_transient",
	EventSendFailedPermanent: "send_failed_permanent",
	EventLeaseExpired:        "lease_expired",
	EventCancel:              "cancel",
	EventExpire:              "expire",
}

// String returns the event name.
func (e MessageEvent) String() string {
	if e > 0 && int(e) < len(eventNames) {
		return eventNames[e]
	}
	return "event(" + strconv.Itoa(int(e)) + ")"
}

// EventForSendResult maps the error returned by Connector.Send to the event
// the dispatcher applies: nil -> [EventSendSucceeded], Permanent ->
// [EventSendFailedPermanent], anything else (Transient, Auth, unclassified)
// -> [EventSendFailedTransient].
func EventForSendResult(err error) MessageEvent {
	switch KindOf(err) {
	case "":
		return EventSendSucceeded
	case KindPermanent:
		return EventSendFailedPermanent
	default:
		return EventSendFailedTransient
	}
}

// NextStatus is the outbound message state machine. It returns the status a
// message in status from moves to when ev happens, or an error wrapping
// [ErrInvalidTransition].
//
// attempts is Message.Attempts as stored while the message is sending, i.e.
// already incremented for the attempt that just ended. maxAttempts is the
// retry budget; values below 1 are treated as 1 (no retries).
//
// NextStatus is pure: store implementations and the dispatcher both call it so
// that the rules live in one place.
func NextStatus(from MessageStatus, ev MessageEvent, attempts, maxAttempts int) (MessageStatus, error) {
	maxAttempts = max(maxAttempts, 1)
	retryOrFail := func() MessageStatus {
		if attempts < maxAttempts {
			return StatusQueued
		}
		return StatusFailed
	}

	switch from {
	case StatusQueued:
		switch ev {
		case EventAcquire:
			return StatusSending, nil
		case EventCancel:
			return StatusCancelled, nil
		case EventExpire:
			return StatusFailed, nil
		}
	case StatusSending:
		switch ev {
		case EventSendSucceeded:
			return StatusSent, nil
		case EventSendFailedPermanent:
			return StatusFailed, nil
		case EventSendFailedTransient, EventLeaseExpired:
			return retryOrFail(), nil
		}
	}
	return from, fmt.Errorf("%w: %s from %q", ErrInvalidTransition, ev, from)
}
