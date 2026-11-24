// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package store defines the persistence contract of tikker: repository
// interfaces, filters, pagination and transactions. It contains no database
// code. The SQLite implementation lives in internal/store/sqlite (T11); a
// PostgreSQL one may follow after the MVP. See ADR 0002 for the delivery
// semantics the message repository must provide and ADR 0003 for the choice
// of package layout.
//
// Conventions for every implementation:
//
//   - Methods return errors wrapping domain.ErrNotFound, domain.ErrConflict,
//     domain.ErrInvalidTransition or domain.ErrLeaseLost where documented;
//     callers test them with errors.Is.
//   - Time is passed in by the caller (from its clock.Clock); repositories
//     never call time.Now, except to fill CreatedAt when the caller left it
//     zero.
//   - Reads never write. In particular a poll that finds nothing to do
//     (AcquireNext, NextDue, an empty inbox) must not touch the database
//     file, to spare the flash storage of a router (SPEC §2.2).
//   - The caller assigns IDs (domain.IDGenerator) before Create/Enqueue.
package store

import (
	"context"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

// Repos gives access to all repositories. Inside [Store.WithTx] the
// repositories share one transaction.
type Repos interface {
	Messages() MessageRepo
	InboundParts() InboundPartRepo
	Devices() DeviceRepo
	Channels() ChannelRepo
	Users() UserRepo
	Tokens() TokenRepo
	Sessions() SessionRepo
	Audit() AuditRepo
	Webhooks() WebhookRepo
	MailOutbox() MailOutboxRepo
}

// Store is the root of the persistence layer.
type Store interface {
	Repos

	// WithTx runs fn in a single transaction. If fn returns an error or
	// panics, the transaction is rolled back and the error (or panic) is
	// propagated; otherwise it is committed. The Repos passed to fn must
	// not be used after fn returns. WithTx must not be nested: calling it on
	// tx's store from within fn is not supported.
	//
	// Individual repository methods are already atomic on their own;
	// WithTx is for multi-step units such as storing a whole inbox batch or
	// creating a device with its channels plus an audit entry.
	WithTx(ctx context.Context, fn func(tx Repos) error) error

	// Ping checks that the database is usable (for /readyz).
	Ping(ctx context.Context) error

	// Close releases the database.
	Close() error
}

// ---------------------------------------------------------------------------
// Messages

// MessageRepo stores outbound and inbound messages and implements the
// outbound queue. State changes follow domain.NextStatus.
type MessageRepo interface {
	// Enqueue atomically stores one or more new outbound messages (a single
	// POST /messages or a whole POST /messages/batch) in StatusQueued with
	// Attempts 0. Each message must have ID, Peer, Body, Encoding, Segments
	// and CreatedBy set, and all must share the same CreatedBy;
	// NextAttemptAt defaults to CreatedAt. If Route pins a channel,
	// ChannelID is set to it. Either all messages are stored or none.
	//
	// Idempotency (API Idempotency-Key, one key per request): when idem.Key
	// is not empty, the store keeps a record (CreatedBy, Key) ->
	// (RequestHash, message IDs). If a record created at or after idem.Since
	// exists:
	//   - with the same RequestHash, nothing is stored and the original
	//     messages are returned in their current state with Replayed true;
	//   - with a different RequestHash, the error wraps
	//     domain.ErrIdempotencyMismatch.
	// The check and the insert happen in one transaction, so two concurrent
	// requests with the same key create the messages once.
	Enqueue(ctx context.Context, msgs []domain.Message, idem Idempotency) (EnqueueResult, error)

	// Replay looks up the idempotency record (createdBy, idem.Key) without
	// storing anything. ok is true when a record created at or after
	// idem.Since exists with the same RequestHash; the result is then what
	// Enqueue would return for a replay (Replayed true, the original
	// messages in their current state, the stored Groups). A record with a
	// different RequestHash is domain.ErrIdempotencyMismatch. The API calls
	// it before validating a request, so a replay returns the original
	// response even if the request would no longer be accepted (for
	// example because its channel was disabled in the meantime).
	Replay(ctx context.Context, createdBy domain.Actor, idem Idempotency) (res EnqueueResult, ok bool, err error)

	// AcquireNext atomically picks the next outbound message the channel in
	// req may send, moves it to StatusSending, increments Attempts, sets
	// ChannelID = req.ChannelID and LeaseUntil = req.LeaseUntil, and
	// returns it. ok is false when nothing is eligible (and nothing was
	// written).
	//
	// Eligible: status queued, NextAttemptAt <= req.Now, ExpiresAt zero or
	// after req.Now, and either Route.ChannelID == req.ChannelID, or
	// Route.ChannelID empty and every Route.Tags member is in
	// req.ChannelTags. Order: NextAttemptAt, then ID (FIFO). Two concurrent
	// calls never return the same message.
	AcquireNext(ctx context.Context, req AcquireRequest) (m domain.Message, ok bool, err error)

	// NextDue returns the earliest NextAttemptAt among queued messages the
	// channel could acquire (same eligibility as AcquireNext, ignoring
	// time). The dispatcher sleeps until then instead of polling. ok is
	// false when there are none.
	NextDue(ctx context.Context, channelID domain.ID, channelTags []string) (at time.Time, ok bool, err error)

	// MarkSent completes lease with StatusSent and SentAt = sentAt. It
	// returns domain.ErrLeaseLost if the message is no longer sending under
	// this lease (status sending, ID and Attempts must match).
	MarkSent(ctx context.Context, lease domain.Lease, sentAt time.Time) error

	// MarkFailed completes lease with StatusFailed and LastError. It
	// returns domain.ErrLeaseLost like MarkSent. possibleDuplicate is OR-ed
	// into Message.PossibleDuplicate.
	MarkFailed(ctx context.Context, lease domain.Lease, lastErr string, possibleDuplicate bool) error

	// Requeue returns a leased message to StatusQueued after a retryable
	// failure: it clears LeaseUntil, sets NextAttemptAt, LastError and ORs
	// in PossibleDuplicate. If Route.ChannelID is empty, ChannelID is
	// cleared so any matching channel may pick the message up. Returns
	// domain.ErrLeaseLost like MarkSent. The caller decides between Requeue
	// and MarkFailed with domain.NextStatus.
	Requeue(ctx context.Context, lease domain.Lease, p RequeueParams) error

	// Cancel moves a queued outbound message to StatusCancelled and returns
	// it. Messages in any other status yield domain.ErrInvalidTransition
	// (the returned message then shows the current status); unknown IDs
	// domain.ErrNotFound.
	Cancel(ctx context.Context, id domain.ID) (domain.Message, error)

	// RecoverExpiredLeases handles every message in StatusSending whose
	// LeaseUntil <= now: per domain.NextStatus(sending, EventLeaseExpired,
	// Attempts, maxAttempts) it goes back to queued (NextAttemptAt = now,
	// ChannelID cleared unless pinned) or to failed, and in both cases gets
	// PossibleDuplicate = true and LastError explaining the recovery. It
	// returns what it changed so the caller can log each possible duplicate
	// (ADR 0002). Called at startup and periodically by the dispatcher.
	RecoverExpiredLeases(ctx context.Context, now time.Time, maxAttempts int) ([]RecoveredLease, error)

	// ExpireQueued moves queued messages with a non-zero ExpiresAt <= now to
	// StatusFailed with LastError domain.LastErrorExpired and returns how
	// many it changed.
	ExpireQueued(ctx context.Context, now time.Time) (int, error)

	// ExpireQueuedMessages is ExpireQueued returning the messages it
	// changed (with their new status), so the caller can record an event for
	// each of them in the same transaction.
	ExpireQueuedMessages(ctx context.Context, now time.Time) ([]domain.Message, error)

	// Get returns one message or domain.ErrNotFound.
	Get(ctx context.Context, id domain.ID) (domain.Message, error)

	// List returns messages matching f, newest first (descending ID), one
	// page at a time.
	List(ctx context.Context, f MessageFilter, page PageRequest) (Page[domain.Message], error)

	// Count returns the number of messages matching f (dashboard, channel
	// queue_depth).
	Count(ctx context.Context, f MessageFilter) (int, error)

	// InsertInbound stores an inbound message (Direction in, Status
	// received) unless one with the same DedupHash exists. created reports
	// whether a row was inserted. A duplicate is not an error: it is the
	// expected outcome after a crash between storing and deleting on the
	// router (SPEC §4.3).
	InsertInbound(ctx context.Context, m domain.Message) (created bool, err error)

	// PurgeBodies empties Body of terminal messages created before cutoff
	// (retention, SPEC §7) and returns how many it changed. Metadata stays.
	PurgeBodies(ctx context.Context, cutoff time.Time) (int, error)

	// PurgeIdempotency deletes idempotency records created before the given
	// time (callers pass now - 24 h) and returns how many it removed.
	PurgeIdempotency(ctx context.Context, before time.Time) (int, error)
}

// Idempotency identifies a client request for [MessageRepo.Enqueue].
// The zero value disables the check.
type Idempotency struct {
	Key string
	// RequestHash is a digest of the canonicalised request body, computed
	// by the API layer.
	RequestHash string
	// Since is the start of the validity window (now - 24 h).
	Since time.Time
	// Groups optionally partitions the enqueued messages into consecutive
	// groups (one per item of a batch request; a group has several messages
	// when a text was split). The sizes are positive and add up to the
	// number of messages. They are stored with the record and returned on a
	// replay; nil means one group.
	Groups []int
}

// EnqueueResult is returned by [MessageRepo.Enqueue] and
// [MessageRepo.Replay].
type EnqueueResult struct {
	Messages []domain.Message // in request order
	Replayed bool             // true: an earlier request with the same key
	// Groups are the group sizes of the request (Idempotency.Groups); nil
	// means one group.
	Groups []int
}

// AcquireRequest asks for the next message for one channel.
type AcquireRequest struct {
	ChannelID   domain.ID
	ChannelTags []string
	Now         time.Time
	LeaseUntil  time.Time // must be after Now by more than the send timeout
}

// RequeueParams carries the outcome of a retryable failure.
type RequeueParams struct {
	NextAttemptAt     time.Time
	LastError         string
	PossibleDuplicate bool
}

// RecoveredLease reports one message handled by RecoverExpiredLeases.
type RecoveredLease struct {
	MessageID domain.ID
	ChannelID domain.ID // channel that held the lease
	Attempts  int
	NewStatus domain.MessageStatus // queued or failed
}

// MessageFilter selects messages for List and Count. Zero fields do not
// filter.
type MessageFilter struct {
	Direction domain.Direction
	Statuses  []domain.MessageStatus // any of
	Peer      string                 // exact E.164 match
	ChannelID domain.ID
	CreatedBy domain.Actor
	Since     time.Time // CreatedAt >= Since
	Until     time.Time // CreatedAt < Until
}

// InboundPartRepo keeps the parts of inbound concatenated SMS while the inbox
// poller waits for the rest of the message (SPEC §4.3, ADR 0002 §4).
//
// Every part ever seen keeps a row, identified by its DedupHash: pending
// while the message is incomplete, afterwards as a tombstone without content.
// A part redelivered by the router after a failed deletion therefore finds
// its hash and is not mistaken for a new message.
type InboundPartRepo interface {
	// Add stores a pending part unless a row with the same DedupHash exists
	// (pending or consumed). created reports whether a row was inserted.
	// A duplicate is not an error. The part must have ID, ChannelID and
	// DedupHash; CreatedAt defaults to the store's clock.
	Add(ctx context.Context, p InboundPart) (created bool, err error)

	// ListPending returns the pending parts of a channel, oldest first (by
	// CreatedAt, then ID). It never writes.
	ListPending(ctx context.Context, channelID domain.ID) ([]InboundPart, error)

	// Consume marks pending parts as belonging to the stored message
	// messageID: it empties their PDU and Body (the content now lives in the
	// message) and keeps the rows as tombstones. Unknown IDs are ignored.
	Consume(ctx context.Context, ids []domain.ID, messageID domain.ID, at time.Time) error

	// PurgeConsumed deletes tombstones consumed before the given time and
	// returns how many it removed. It writes nothing when there are none.
	PurgeConsumed(ctx context.Context, before time.Time) (int, error)
}

// InboundPart is one received part of a concatenated SMS.
type InboundPart struct {
	ID        domain.ID
	ChannelID domain.ID
	// DedupHash is domain.InboundDedupHash of the part's raw data.
	DedupHash    string
	Phone        string
	RawTimestamp string
	ReceivedAt   time.Time // device timestamp, or fetch time if unknown
	Body         string    // device-decoded text (GSM-7 guess)
	PDU          string    // raw PDU as reported by the device
	CreatedAt    time.Time // when the part was first stored
}

// ---------------------------------------------------------------------------
// Devices and channels

// DeviceRepo stores devices.
type DeviceRepo interface {
	// Create stores d; a duplicate name yields domain.ErrConflict.
	Create(ctx context.Context, d domain.Device) error
	// Get returns one device or domain.ErrNotFound.
	Get(ctx context.Context, id domain.ID) (domain.Device, error)
	// List returns all devices ordered by name.
	List(ctx context.Context) ([]domain.Device, error)
	// Update replaces all mutable fields of d (everything but ID and
	// CreatedAt). A nil PasswordEnc keeps the stored password, a nil
	// AgentTokenEnc the stored agent token.
	Update(ctx context.Context, d domain.Device) error
	// Delete removes the device and its channels. It fails with
	// domain.ErrConflict, deleting nothing, while any outbound message is
	// queued or sending on (or pinned to) one of its channels. Messages
	// keep their ChannelID for history.
	Delete(ctx context.Context, id domain.ID) error
}

// ChannelRepo stores channels and their health.
type ChannelRepo interface {
	// Create stores c; a duplicate (DeviceID, Port) yields domain.ErrConflict.
	Create(ctx context.Context, c domain.Channel) error
	// Get returns one channel or domain.ErrNotFound.
	Get(ctx context.Context, id domain.ID) (domain.Channel, error)
	// List returns channels matching f ordered by name.
	List(ctx context.Context, f ChannelFilter) ([]domain.Channel, error)
	// Update replaces the configuration fields of c (Name, Tags,
	// RatePerMin, MaxSegments, Transliterate, Enabled); DeviceID, Port and
	// health fields are left alone.
	Update(ctx context.Context, c domain.Channel) error
	// UpdateHealth stores a health check result. Implementations should
	// skip the write when nothing but the timestamp changed and the stored
	// LastHealthAt is recent, to limit flash writes.
	UpdateHealth(ctx context.Context, id domain.ID, h ChannelHealthUpdate) error
	// Delete removes a channel. It fails with domain.ErrConflict while any
	// outbound message is queued or sending on, or pinned to, the channel.
	Delete(ctx context.Context, id domain.ID) error
}

// ChannelFilter selects channels. Zero fields do not filter.
type ChannelFilter struct {
	DeviceID    domain.ID
	EnabledOnly bool // channel and its device both enabled
	States      []domain.ChannelState
	Tags        []string // channel must carry all
}

// ChannelHealthUpdate is the result of one health check.
type ChannelHealthUpdate struct {
	State       domain.ChannelState
	StateReason string
	At          time.Time
	Signal      *domain.Signal
	Operator    string
}

// ---------------------------------------------------------------------------
// Users, tokens, audit

// UserRepo stores panel users.
type UserRepo interface {
	// Create stores u; a duplicate username yields domain.ErrConflict.
	Create(ctx context.Context, u domain.User) error
	// Get returns one user or domain.ErrNotFound.
	Get(ctx context.Context, id domain.ID) (domain.User, error)
	// GetByUsername returns one user or domain.ErrNotFound. Usernames are
	// compared case-insensitively.
	GetByUsername(ctx context.Context, username string) (domain.User, error)
	// List returns all users ordered by username.
	List(ctx context.Context) ([]domain.User, error)
	// Update replaces Username, PasswordHash, Role and Disabled. A change
	// that would leave no enabled admin (demoting or disabling the last one)
	// is refused with domain.ErrConflict, atomically with the write.
	Update(ctx context.Context, u domain.User) error
	// Count returns the number of users (first-run wizard).
	Count(ctx context.Context) (int, error)
	// CountActiveAdmins returns the number of enabled admins, so that the
	// last one cannot be demoted or disabled (API 409).
	CountActiveAdmins(ctx context.Context) (int, error)
	// Users are never deleted, only disabled (audit trail).
}

// TokenRepo stores API tokens.
type TokenRepo interface {
	// Create stores t; a duplicate prefix yields domain.ErrConflict.
	Create(ctx context.Context, t domain.APIToken) error
	// GetByPrefix returns the token with the given public prefix or
	// domain.ErrNotFound. The caller verifies the hash in constant time.
	GetByPrefix(ctx context.Context, prefix string) (domain.APIToken, error)
	// List returns tokens of userID, or all tokens if userID is empty,
	// newest first.
	List(ctx context.Context, userID domain.ID) ([]domain.APIToken, error)
	// Delete revokes a token.
	Delete(ctx context.Context, id domain.ID) error
	// TouchLastUsed sets LastUsedAt. Implementations may coalesce writes
	// (e.g. at most once per minute per token) to limit flash wear.
	TouchLastUsed(ctx context.Context, id domain.ID, at time.Time) error
}

// SessionRepo stores server-side sessions of the web panel (T20). A session
// is identified by the hex SHA-256 of the random token kept in the browser
// cookie, so a database leak does not reveal usable cookies. Expiry is
// decided by the caller (internal/auth); the repository only stores
// timestamps.
type SessionRepo interface {
	// Create stores s; a duplicate ID yields domain.ErrConflict and an
	// unknown UserID domain.ErrNotFound. LastSeenAt defaults to CreatedAt.
	Create(ctx context.Context, s Session) error
	// Get returns one session or domain.ErrNotFound. It never writes.
	Get(ctx context.Context, id string) (Session, error)
	// List returns the sessions of userID, newest first (active sessions
	// view). It never writes.
	List(ctx context.Context, userID domain.ID) ([]Session, error)
	// Touch sets LastSeenAt or returns domain.ErrNotFound. Callers limit
	// how often they call it (e.g. once a minute per session) to spare
	// the flash storage.
	Touch(ctx context.Context, id string, at time.Time) error
	// Delete removes one session or returns domain.ErrNotFound.
	Delete(ctx context.Context, id string) error
	// DeleteByUser removes every session of userID (password, role or
	// status change) and returns how many it removed.
	DeleteByUser(ctx context.Context, userID domain.ID) (int, error)
	// DeleteExpired removes sessions with ExpiresAt <= now or LastSeenAt
	// <= idleBefore and returns how many it removed. It writes nothing
	// when there are none.
	DeleteExpired(ctx context.Context, now, idleBefore time.Time) (int, error)
}

// Session is a logged-in panel session.
type Session struct {
	ID         string // hex SHA-256 of the cookie token; never the token itself
	UserID     domain.ID
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time // absolute expiry
	UserAgent  string    // truncated, informational
	IP         string    // truncated client network, informational
}

// AuditRepo stores the audit log. It is append-only.
type AuditRepo interface {
	// Append stores e.
	Append(ctx context.Context, e domain.AuditLog) error
	// List returns entries matching f, newest first.
	List(ctx context.Context, f AuditFilter, page PageRequest) (Page[domain.AuditLog], error)
}

// AuditFilter selects audit entries. Zero fields do not filter.
type AuditFilter struct {
	Actor  domain.Actor
	Action domain.AuditAction
	Since  time.Time
	Until  time.Time
}

// WebhookRepo stores webhook subscriptions and their delivery queue. Delivery
// semantics mirror MessageRepo (ADR 0002): at-least-once, an attempt holds a
// lease, the attempt counter is the fencing token.
type WebhookRepo interface {
	// Create stores w. Errors: domain.ErrConflict (duplicate name).
	Create(ctx context.Context, w domain.Webhook) error
	// Get returns one webhook. Errors: domain.ErrNotFound.
	Get(ctx context.Context, id domain.ID) (domain.Webhook, error)
	// List returns all webhooks ordered by ID.
	List(ctx context.Context) ([]domain.Webhook, error)
	// Update replaces name, URL, events and the enabled flag; SecretEnc
	// replaces the stored secret only when non-nil. Errors: ErrNotFound,
	// ErrConflict.
	Update(ctx context.Context, w domain.Webhook) error
	// Delete removes the webhook and its deliveries. Errors: ErrNotFound.
	Delete(ctx context.Context, id domain.ID) error

	// Subscribed returns the enabled webhooks that listen to ev. It is a
	// read; use it inside the transaction that stores the event.
	Subscribed(ctx context.Context, ev domain.WebhookEvent) ([]domain.Webhook, error)
	// AddDeliveries stores pending deliveries (status, attempts and lease
	// fields are ignored). Call it in the transaction of the event.
	AddDeliveries(ctx context.Context, ds []domain.WebhookDelivery) error

	// AcquireDelivery atomically picks the oldest due pending delivery of an
	// enabled webhook, increments its attempt counter and leases it until
	// leaseUntil. Two concurrent calls never return the same attempt. ok is
	// false (and nothing is written) when nothing is due.
	AcquireDelivery(ctx context.Context, now, leaseUntil time.Time) (DeliveryJob, bool, error)
	// NextDeliveryDue returns when the earliest pending delivery of an
	// enabled webhook becomes available (retry time or lease expiry).
	NextDeliveryDue(ctx context.Context) (at time.Time, ok bool, err error)
	// MarkDelivered, MarkDeliveryFailed and RescheduleDelivery complete an
	// attempt. They return domain.ErrLeaseLost when the attempt is no longer
	// the current one (lease recovered by a newer attempt, or the delivery
	// was deleted).
	MarkDelivered(ctx context.Context, l domain.DeliveryLease, at time.Time, httpCode int) error
	MarkDeliveryFailed(ctx context.Context, l domain.DeliveryLease, at time.Time, lastErr string, httpCode int) error
	RescheduleDelivery(ctx context.Context, l domain.DeliveryLease, at, nextAttempt time.Time, lastErr string, httpCode int) error
	// GetDelivery returns one delivery. Errors: ErrNotFound.
	GetDelivery(ctx context.Context, id domain.ID) (domain.WebhookDelivery, error)
	// ListDeliveries returns the deliveries of a webhook, newest first.
	ListDeliveries(ctx context.Context, webhookID domain.ID, limit int) ([]domain.WebhookDelivery, error)
	// ExpireDeliveries fails the pending deliveries created before cutoff and
	// drops their payload (a payload must not outlive the SMS retention), and
	// returns their number.
	ExpireDeliveries(ctx context.Context, cutoff, now time.Time) (int, error)
	// PurgeDeliveries deletes delivered and failed deliveries last updated
	// before cutoff and returns their number.
	PurgeDeliveries(ctx context.Context, cutoff time.Time) (int, error)
}

// MailOutboxRepo is the durable queue of e-mail notifications of inbound
// SMS (ADR 0004), with the delivery semantics of WebhookRepo.
type MailOutboxRepo interface {
	// Add stores a pending notification (status, attempts and lease fields
	// are ignored). Call it in the transaction that stores the message.
	Add(ctx context.Context, n domain.MailNotification) error
	// Acquire atomically picks the oldest due pending notification,
	// increments its attempt counter and leases it until leaseUntil. ok is
	// false (and nothing is written) when nothing is due.
	Acquire(ctx context.Context, now, leaseUntil time.Time) (n domain.MailNotification, ok bool, err error)
	// NextDue returns when the earliest pending notification becomes
	// available (retry time or lease expiry).
	NextDue(ctx context.Context) (at time.Time, ok bool, err error)
	// MarkSent, MarkFailed and Reschedule complete an attempt; they return
	// domain.ErrLeaseLost when the attempt is no longer the current one.
	MarkSent(ctx context.Context, l domain.DeliveryLease, at time.Time) error
	MarkFailed(ctx context.Context, l domain.DeliveryLease, at time.Time, lastErr string) error
	Reschedule(ctx context.Context, l domain.DeliveryLease, at, next time.Time, lastErr string) error
	// Get returns one notification. Errors: domain.ErrNotFound.
	Get(ctx context.Context, id domain.ID) (domain.MailNotification, error)
	// Purge deletes delivered and failed notifications last updated before
	// cutoff and returns their number.
	Purge(ctx context.Context, cutoff time.Time) (int, error)
}

// DeliveryJob is a leased delivery together with its target.
type DeliveryJob struct {
	Delivery domain.WebhookDelivery // Attempts already incremented
	Webhook  domain.Webhook
}
