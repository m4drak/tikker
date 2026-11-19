// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

type messageRepo struct{ h *handle }

var _ store.MessageRepo = (*messageRepo)(nil)

const messageCols = `id, direction, channel_id, route_channel_id, route_tags, peer, body, encoding,
	segments, status, attempts, next_attempt_at, lease_until, last_error, expires_at,
	possible_duplicate, idempotency_key, client_ref, created_by, created_at, sent_at,
	received_at, dedup_hash`

const insertMessageSQL = `INSERT INTO messages (` + messageCols + `) VALUES (` +
	`?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// scanner is implemented by *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanMessage(sc scanner) (domain.Message, error) {
	var (
		m                                                            domain.Message
		direction, status, encoding, routeTags, createdBy            string
		channelID, routeChannel, id                                  string
		nextAt, leaseUntil, expiresAt, createdAt, sentAt, receivedAt int64
		possibleDup                                                  int
		dedup                                                        sql.NullString
	)
	err := sc.Scan(&id, &direction, &channelID, &routeChannel, &routeTags, &m.Peer, &m.Body, &encoding,
		&m.Segments, &status, &m.Attempts, &nextAt, &leaseUntil, &m.LastError, &expiresAt,
		&possibleDup, &m.IdempotencyKey, &m.ClientRef, &createdBy, &createdAt, &sentAt,
		&receivedAt, &dedup)
	if err != nil {
		return domain.Message{}, err
	}
	m.ID = domain.ID(id)
	m.Direction = domain.Direction(direction)
	m.ChannelID = domain.ID(channelID)
	m.Route.ChannelID = domain.ID(routeChannel)
	if m.Route.Tags, err = parseTags(routeTags); err != nil {
		return domain.Message{}, err
	}
	m.Encoding = domain.Encoding(encoding)
	m.Status = domain.MessageStatus(status)
	m.NextAttemptAt = fromMS(nextAt)
	m.LeaseUntil = fromMS(leaseUntil)
	m.ExpiresAt = fromMS(expiresAt)
	m.PossibleDuplicate = possibleDup != 0
	if m.CreatedBy, err = parseActor(createdBy); err != nil {
		return domain.Message{}, err
	}
	m.CreatedAt = fromMS(createdAt)
	m.SentAt = fromMS(sentAt)
	m.ReceivedAt = fromMS(receivedAt)
	m.DedupHash = dedup.String
	return m, nil
}

func messageArgs(m domain.Message) []any {
	var dedup any
	if m.DedupHash != "" {
		dedup = m.DedupHash
	}
	return []any{
		string(m.ID), string(m.Direction), string(m.ChannelID), string(m.Route.ChannelID),
		tagsJSON(m.Route.Tags), m.Peer, m.Body, string(m.Encoding), m.Segments, string(m.Status),
		m.Attempts, toMS(m.NextAttemptAt), toMS(m.LeaseUntil), m.LastError, toMS(m.ExpiresAt),
		boolInt(m.PossibleDuplicate), m.IdempotencyKey, m.ClientRef, actorString(m.CreatedBy),
		toMS(m.CreatedAt), toMS(m.SentAt), toMS(m.ReceivedAt), dedup,
	}
}

func actorString(a domain.Actor) string {
	if a.IsZero() {
		return ""
	}
	return a.String()
}

// normalizeMessage returns m as it will read back from the database.
func normalizeMessage(m domain.Message) domain.Message {
	m.NextAttemptAt = trunc(m.NextAttemptAt)
	m.LeaseUntil = trunc(m.LeaseUntil)
	m.ExpiresAt = trunc(m.ExpiresAt)
	m.CreatedAt = trunc(m.CreatedAt)
	m.SentAt = trunc(m.SentAt)
	m.ReceivedAt = trunc(m.ReceivedAt)
	if len(m.Route.Tags) == 0 {
		m.Route.Tags = nil
	}
	return m
}

// ---------------------------------------------------------------------------
// Enqueue

// Enqueue implements [store.MessageRepo].
func (r *messageRepo) Enqueue(ctx context.Context, msgs []domain.Message, idem store.Idempotency) (store.EnqueueResult, error) {
	if len(msgs) == 0 {
		return store.EnqueueResult{}, errors.New("sqlite: enqueue: no messages")
	}
	now := r.h.s.clk.Now()
	prepared := make([]domain.Message, len(msgs))
	for i, m := range msgs {
		switch {
		case m.ID.IsZero():
			return store.EnqueueResult{}, errors.New("sqlite: enqueue: message without ID")
		case m.CreatedBy != msgs[0].CreatedBy:
			return store.EnqueueResult{}, errors.New("sqlite: enqueue: messages of one request must share CreatedBy")
		}
		m.Direction = domain.DirectionOut
		m.Status = domain.StatusQueued
		m.Attempts = 0
		m.LeaseUntil = time.Time{}
		m.SentAt = time.Time{}
		m.ReceivedAt = time.Time{}
		m.DedupHash = ""
		if m.CreatedAt.IsZero() {
			m.CreatedAt = now
		}
		if m.NextAttemptAt.IsZero() {
			m.NextAttemptAt = m.CreatedAt
		}
		if !m.Route.ChannelID.IsZero() {
			m.ChannelID = m.Route.ChannelID
		}
		if idem.Key != "" && m.IdempotencyKey == "" {
			m.IdempotencyKey = idem.Key
		}
		prepared[i] = normalizeMessage(m)
	}

	var res store.EnqueueResult
	err := r.h.write(ctx, func(q execer) error {
		if idem.Key != "" {
			replay, ok, err := checkIdempotency(ctx, q, prepared[0].CreatedBy, idem)
			if err != nil {
				return err
			}
			if ok {
				res = store.EnqueueResult{Messages: replay, Replayed: true}
				return nil
			}
		}
		ids := make([]string, len(prepared))
		for i, m := range prepared {
			if _, err := q.ExecContext(ctx, insertMessageSQL, messageArgs(m)...); err != nil {
				return fmt.Errorf("sqlite: insert message %s: %w", m.ID, mapErr(err))
			}
			ids[i] = string(m.ID)
		}
		if idem.Key != "" {
			if _, err := q.ExecContext(ctx,
				`INSERT INTO idempotency_keys (created_by, key, request_hash, message_ids, created_at)
				 VALUES (?, ?, ?, ?, ?)`,
				actorString(prepared[0].CreatedBy), idem.Key, idem.RequestHash, marshalJSON(ids),
				toMS(prepared[0].CreatedAt)); err != nil {
				return fmt.Errorf("sqlite: insert idempotency key: %w", mapErr(err))
			}
		}
		res = store.EnqueueResult{Messages: prepared}
		return nil
	})
	if err != nil {
		return store.EnqueueResult{}, err
	}
	return res, nil
}

// checkIdempotency looks up (createdBy, key). It returns the original
// messages when a valid record with the same hash exists; a record with a
// different hash is an error; a record older than idem.Since is deleted.
func checkIdempotency(ctx context.Context, q execer, createdBy domain.Actor, idem store.Idempotency) ([]domain.Message, bool, error) {
	by := actorString(createdBy)
	var hash, idsJSON string
	var created int64
	err := q.QueryRowContext(ctx,
		`SELECT request_hash, message_ids, created_at FROM idempotency_keys WHERE created_by = ? AND key = ?`,
		by, idem.Key).Scan(&hash, &idsJSON, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: read idempotency key: %w", err)
	}
	if created < toMS(idem.Since) {
		if _, err := q.ExecContext(ctx,
			`DELETE FROM idempotency_keys WHERE created_by = ? AND key = ?`, by, idem.Key); err != nil {
			return nil, false, fmt.Errorf("sqlite: delete stale idempotency key: %w", err)
		}
		return nil, false, nil
	}
	if hash != idem.RequestHash {
		return nil, false, fmt.Errorf("%w: key %q", domain.ErrIdempotencyMismatch, idem.Key)
	}
	var ids []string
	if err := json.Unmarshal([]byte(idsJSON), &ids); err != nil {
		return nil, false, fmt.Errorf("sqlite: corrupt idempotency record: %w", err)
	}
	msgs := make([]domain.Message, 0, len(ids))
	for _, id := range ids {
		m, err := getMessage(ctx, q, domain.ID(id))
		if err != nil {
			return nil, false, err
		}
		msgs = append(msgs, m)
	}
	return msgs, true, nil
}

func getMessage(ctx context.Context, q execer, id domain.ID) (domain.Message, error) {
	m, err := scanMessage(q.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Message{}, notFound("message", id)
	}
	if err != nil {
		return domain.Message{}, fmt.Errorf("sqlite: get message: %w", err)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Queue

// eligibleSQL selects queued messages the channel (args: channel ID, channel
// tags as JSON) may send, ignoring time.
const eligibleSQL = `m.status = 'queued' AND (m.route_channel_id = ?1 OR (m.route_channel_id = '' AND
	NOT EXISTS (SELECT 1 FROM json_each(m.route_tags) rt
	            WHERE rt.value NOT IN (SELECT value FROM json_each(?2)))))`

const dueSQL = `m.next_attempt_at <= ?3 AND (m.expires_at = 0 OR m.expires_at > ?3)`

// AcquireNext implements [store.MessageRepo].
func (r *messageRepo) AcquireNext(ctx context.Context, req store.AcquireRequest) (domain.Message, bool, error) {
	tags := tagsJSON(req.ChannelTags)
	now := toMS(req.Now)

	// Cheap read-only probe, so an idle poll takes no write lock at all.
	if r.h.tx == nil {
		var one int
		err := r.h.r.QueryRowContext(ctx,
			`SELECT 1 FROM messages m WHERE `+eligibleSQL+` AND `+dueSQL+` LIMIT 1`,
			string(req.ChannelID), tags, now).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Message{}, false, nil
		}
		if err != nil {
			return domain.Message{}, false, fmt.Errorf("sqlite: acquire probe: %w", err)
		}
	}

	var m domain.Message
	err := r.h.write(ctx, func(q execer) error {
		var err error
		m, err = scanMessage(q.QueryRowContext(ctx,
			`UPDATE messages
			    SET status = 'sending', attempts = attempts + 1, channel_id = ?1, lease_until = ?4
			  WHERE id = (SELECT m.id FROM messages m
			               WHERE `+eligibleSQL+` AND `+dueSQL+`
			               ORDER BY m.next_attempt_at, m.id LIMIT 1)
			RETURNING `+messageCols,
			string(req.ChannelID), tags, now, toMS(req.LeaseUntil)))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Message{}, false, nil
	}
	if err != nil {
		return domain.Message{}, false, fmt.Errorf("sqlite: acquire: %w", err)
	}
	return m, true, nil
}

// NextDue implements [store.MessageRepo].
func (r *messageRepo) NextDue(ctx context.Context, channelID domain.ID, channelTags []string) (time.Time, bool, error) {
	var at sql.NullInt64
	err := r.h.r.QueryRowContext(ctx,
		`SELECT MIN(m.next_attempt_at) FROM messages m WHERE `+eligibleSQL,
		string(channelID), tagsJSON(channelTags)).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("sqlite: next due: %w", err)
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(at.Int64).UTC(), true, nil
}

// complete finishes a lease: it applies set (assignments and their args) to
// the message when it is still sending under lease.
func (r *messageRepo) complete(ctx context.Context, lease domain.Lease, set string, args ...any) error {
	args = append(args, string(lease.MessageID), lease.Attempt)
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`UPDATE messages SET `+set+` WHERE id = ? AND status = 'sending' AND attempts = ?`, args...)
		if err != nil {
			return fmt.Errorf("sqlite: complete lease: %w", err)
		}
		n, err := rowsAffected(res)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: message %s attempt %d", domain.ErrLeaseLost, lease.MessageID, lease.Attempt)
		}
		return nil
	})
}

// MarkSent implements [store.MessageRepo].
func (r *messageRepo) MarkSent(ctx context.Context, lease domain.Lease, sentAt time.Time) error {
	return r.complete(ctx, lease, `status = 'sent', sent_at = ?, lease_until = 0`, toMS(sentAt))
}

// MarkFailed implements [store.MessageRepo].
func (r *messageRepo) MarkFailed(ctx context.Context, lease domain.Lease, lastErr string, possibleDuplicate bool) error {
	return r.complete(ctx, lease,
		`status = 'failed', last_error = ?, possible_duplicate = possible_duplicate OR ?, lease_until = 0`,
		lastErr, boolInt(possibleDuplicate))
}

// Requeue implements [store.MessageRepo].
func (r *messageRepo) Requeue(ctx context.Context, lease domain.Lease, p store.RequeueParams) error {
	return r.complete(ctx, lease,
		`status = 'queued', lease_until = 0, next_attempt_at = ?, last_error = ?,
		 possible_duplicate = possible_duplicate OR ?, channel_id = route_channel_id`,
		toMS(p.NextAttemptAt), p.LastError, boolInt(p.PossibleDuplicate))
}

// Cancel implements [store.MessageRepo].
func (r *messageRepo) Cancel(ctx context.Context, id domain.ID) (domain.Message, error) {
	var m domain.Message
	err := r.h.write(ctx, func(q execer) error {
		var err error
		m, err = scanMessage(q.QueryRowContext(ctx,
			`UPDATE messages SET status = 'cancelled', lease_until = 0
			  WHERE id = ? AND status = 'queued' AND direction = 'out' RETURNING `+messageCols, string(id)))
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: cancel: %w", err)
		}
		cur, gerr := getMessage(ctx, q, id)
		if gerr != nil {
			return gerr
		}
		m = cur
		_, terr := domain.NextStatus(cur.Status, domain.EventCancel, cur.Attempts, 1)
		if terr == nil { // an inbound message cannot be queued, but stay safe
			terr = domain.ErrInvalidTransition
		}
		return terr
	})
	return m, err
}

// RecoverExpiredLeases implements [store.MessageRepo].
func (r *messageRepo) RecoverExpiredLeases(ctx context.Context, now time.Time, maxAttempts int) ([]store.RecoveredLease, error) {
	var out []store.RecoveredLease
	err := r.h.write(ctx, func(q execer) error {
		out = nil
		rows, err := q.QueryContext(ctx,
			`SELECT id, channel_id, attempts FROM messages
			  WHERE status = 'sending' AND lease_until <= ? ORDER BY id`, toMS(now))
		if err != nil {
			return fmt.Errorf("sqlite: recover: %w", err)
		}
		var found []store.RecoveredLease
		for rows.Next() {
			var id, ch string
			var attempts int
			if err := rows.Scan(&id, &ch, &attempts); err != nil {
				_ = rows.Close()
				return fmt.Errorf("sqlite: recover: %w", err)
			}
			found = append(found, store.RecoveredLease{MessageID: domain.ID(id), ChannelID: domain.ID(ch), Attempts: attempts})
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("sqlite: recover: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("sqlite: recover: %w", err)
		}
		for _, f := range found {
			next, err := domain.NextStatus(domain.StatusSending, domain.EventLeaseExpired, f.Attempts, maxAttempts)
			if err != nil {
				return err
			}
			f.NewStatus = next
			msg := fmt.Sprintf("lease expired without result after attempt %d; delivery status unknown", f.Attempts)
			if next == domain.StatusQueued {
				_, err = q.ExecContext(ctx,
					`UPDATE messages SET status = 'queued', lease_until = 0, next_attempt_at = ?,
					        channel_id = route_channel_id, possible_duplicate = 1, last_error = ?
					  WHERE id = ?`, toMS(now), msg, string(f.MessageID))
			} else {
				_, err = q.ExecContext(ctx,
					`UPDATE messages SET status = 'failed', lease_until = 0,
					        possible_duplicate = 1, last_error = ?
					  WHERE id = ?`, msg, string(f.MessageID))
			}
			if err != nil {
				return fmt.Errorf("sqlite: recover %s: %w", f.MessageID, err)
			}
			out = append(out, f)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ExpireQueued implements [store.MessageRepo].
func (r *messageRepo) ExpireQueued(ctx context.Context, now time.Time) (int, error) {
	var n int64
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`UPDATE messages SET status = 'failed', last_error = ?
			  WHERE status = 'queued' AND expires_at <> 0 AND expires_at <= ?`,
			domain.LastErrorExpired, toMS(now))
		if err != nil {
			return fmt.Errorf("sqlite: expire: %w", err)
		}
		n, err = rowsAffected(res)
		return err
	})
	return int(n), err
}

// ---------------------------------------------------------------------------
// Reads

// Get implements [store.MessageRepo].
func (r *messageRepo) Get(ctx context.Context, id domain.ID) (domain.Message, error) {
	return getMessage(ctx, r.h.r, id)
}

func messageWhere(f store.MessageFilter) (string, []any) {
	var conds []string
	var args []any
	if f.Direction != "" {
		conds = append(conds, "direction = ?")
		args = append(args, string(f.Direction))
	}
	if len(f.Statuses) > 0 {
		conds = append(conds, "status IN ("+placeholders(len(f.Statuses))+")")
		for _, s := range f.Statuses {
			args = append(args, string(s))
		}
	}
	if f.Peer != "" {
		conds = append(conds, "peer = ?")
		args = append(args, f.Peer)
	}
	if !f.ChannelID.IsZero() {
		conds = append(conds, "channel_id = ?")
		args = append(args, string(f.ChannelID))
	}
	if !f.CreatedBy.IsZero() {
		conds = append(conds, "created_by = ?")
		args = append(args, f.CreatedBy.String())
	}
	if !f.Since.IsZero() {
		conds = append(conds, "created_at >= ?")
		args = append(args, toMS(f.Since))
	}
	if !f.Until.IsZero() {
		conds = append(conds, "created_at < ?")
		args = append(args, toMS(f.Until))
	}
	return strings.Join(conds, " AND "), args
}

// List implements [store.MessageRepo].
func (r *messageRepo) List(ctx context.Context, f store.MessageFilter, page store.PageRequest) (store.Page[domain.Message], error) {
	page = page.Normalized()
	cursor, err := pageCursor(page.Cursor)
	if err != nil {
		return store.Page[domain.Message]{}, err
	}
	where, args := messageWhere(f)
	if cursor != "" {
		if where != "" {
			where += " AND "
		}
		where += "id < ?"
		args = append(args, cursor)
	}
	query := `SELECT ` + messageCols + ` FROM messages`
	if where != "" {
		query += " WHERE " + where
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, page.Limit+1)

	rows, err := r.h.r.QueryContext(ctx, query, args...)
	if err != nil {
		return store.Page[domain.Message]{}, fmt.Errorf("sqlite: list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var items []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return store.Page[domain.Message]{}, fmt.Errorf("sqlite: list messages: %w", err)
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return store.Page[domain.Message]{}, fmt.Errorf("sqlite: list messages: %w", err)
	}
	return paginate(items, page.Limit, func(m domain.Message) domain.ID { return m.ID }), nil
}

// Count implements [store.MessageRepo].
func (r *messageRepo) Count(ctx context.Context, f store.MessageFilter) (int, error) {
	where, args := messageWhere(f)
	query := `SELECT COUNT(*) FROM messages`
	if where != "" {
		query += " WHERE " + where
	}
	var n int
	if err := r.h.r.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: count messages: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Inbound and retention

// InsertInbound implements [store.MessageRepo].
func (r *messageRepo) InsertInbound(ctx context.Context, m domain.Message) (bool, error) {
	if m.ID.IsZero() {
		return false, errors.New("sqlite: inbound message without ID")
	}
	m.Direction = domain.DirectionIn
	m.Status = domain.StatusReceived
	m.CreatedBy = domain.Actor{}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = r.h.s.clk.Now()
	}
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = m.CreatedAt
	}
	m = normalizeMessage(m)
	var created bool
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx, insertMessageSQL+` ON CONFLICT (dedup_hash) DO NOTHING`, messageArgs(m)...)
		if err != nil {
			return fmt.Errorf("sqlite: insert inbound: %w", mapErr(err))
		}
		n, err := rowsAffected(res)
		created = n > 0
		return err
	})
	return created, err
}

// PurgeBodies implements [store.MessageRepo].
func (r *messageRepo) PurgeBodies(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`UPDATE messages SET body = ''
			  WHERE created_at < ? AND body <> ''
			    AND status IN ('sent', 'failed', 'cancelled', 'received')`, toMS(cutoff))
		if err != nil {
			return fmt.Errorf("sqlite: purge bodies: %w", err)
		}
		n, err = rowsAffected(res)
		return err
	})
	return int(n), err
}

// PurgeIdempotency implements [store.MessageRepo].
func (r *messageRepo) PurgeIdempotency(ctx context.Context, before time.Time) (int, error) {
	var n int64
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < ?`, toMS(before))
		if err != nil {
			return fmt.Errorf("sqlite: purge idempotency: %w", err)
		}
		n, err = rowsAffected(res)
		return err
	})
	return int(n), err
}
