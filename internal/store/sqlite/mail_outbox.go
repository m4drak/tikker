// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// MailOutbox implements [store.Repos].
func (h *handle) MailOutbox() store.MailOutboxRepo { return &mailOutboxRepo{h} }

type mailOutboxRepo struct{ h *handle }

const mailCols = `id, message_id, status, attempts, next_attempt_at, lease_until, last_error, created_at, updated_at, sent_at`

func scanMail(sc scanner) (domain.MailNotification, error) {
	var n domain.MailNotification
	var id, msg, status string
	var next, lease, created, updated, sent int64
	if err := sc.Scan(&id, &msg, &status, &n.Attempts, &next, &lease, &n.LastError, &created, &updated, &sent); err != nil {
		return domain.MailNotification{}, err
	}
	n.ID, n.MessageID, n.Status = domain.ID(id), domain.ID(msg), domain.WebhookDeliveryStatus(status)
	n.NextAttemptAt, n.LeaseUntil = fromMS(next), fromMS(lease)
	n.CreatedAt, n.UpdatedAt, n.SentAt = fromMS(created), fromMS(updated), fromMS(sent)
	return n, nil
}

// Add implements [store.MailOutboxRepo].
func (r *mailOutboxRepo) Add(ctx context.Context, n domain.MailNotification) error {
	created := n.CreatedAt
	if created.IsZero() {
		created = r.h.s.clk.Now()
	}
	next := n.NextAttemptAt
	if next.IsZero() {
		next = created
	}
	return r.h.write(ctx, func(q execer) error {
		_, err := q.ExecContext(ctx,
			`INSERT INTO mail_outbox (id, message_id, status, attempts, next_attempt_at, lease_until, last_error, created_at, updated_at, sent_at)
			 VALUES (?, ?, 'pending', 0, ?, 0, '', ?, ?, 0)`,
			string(n.ID), string(n.MessageID), toMS(next), toMS(created), toMS(created))
		if err != nil {
			return fmt.Errorf("sqlite: add mail notification: %w", mapErr(err))
		}
		return nil
	})
}

const mailDueSQL = `status = 'pending' AND next_attempt_at <= ?1 AND lease_until <= ?1`

// Acquire implements [store.MailOutboxRepo].
func (r *mailOutboxRepo) Acquire(ctx context.Context, now, leaseUntil time.Time) (domain.MailNotification, bool, error) {
	// Cheap read-only probe, so an idle poll takes no write lock.
	if r.h.tx == nil {
		var one int
		err := r.h.r.QueryRowContext(ctx, `SELECT 1 FROM mail_outbox WHERE `+mailDueSQL+` LIMIT 1`, toMS(now)).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.MailNotification{}, false, nil
		}
		if err != nil {
			return domain.MailNotification{}, false, fmt.Errorf("sqlite: acquire mail probe: %w", err)
		}
	}
	var n domain.MailNotification
	err := r.h.write(ctx, func(q execer) error {
		var err error
		n, err = scanMail(q.QueryRowContext(ctx,
			`UPDATE mail_outbox SET attempts = attempts + 1, lease_until = ?2, updated_at = ?1
			  WHERE id = (SELECT id FROM mail_outbox WHERE `+mailDueSQL+` ORDER BY next_attempt_at, id LIMIT 1)
			RETURNING `+mailCols, toMS(now), toMS(leaseUntil)))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MailNotification{}, false, nil
	}
	if err != nil {
		return domain.MailNotification{}, false, fmt.Errorf("sqlite: acquire mail: %w", err)
	}
	return n, true, nil
}

// NextDue implements [store.MailOutboxRepo].
func (r *mailOutboxRepo) NextDue(ctx context.Context) (time.Time, bool, error) {
	var at sql.NullInt64
	err := r.h.r.QueryRowContext(ctx,
		`SELECT MIN(MAX(next_attempt_at, lease_until)) FROM mail_outbox WHERE status = 'pending'`).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("sqlite: next mail due: %w", err)
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	return fromMS(at.Int64), true, nil
}

// complete applies set while the attempt is current.
func (r *mailOutboxRepo) complete(ctx context.Context, l domain.DeliveryLease, set string, args ...any) error {
	args = append(args, string(l.DeliveryID), l.Attempt)
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`UPDATE mail_outbox SET `+set+` WHERE id = ? AND status = 'pending' AND attempts = ?`, args...)
		if err != nil {
			return fmt.Errorf("sqlite: complete mail: %w", err)
		}
		n, err := rowsAffected(res)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: mail notification %s attempt %d", domain.ErrLeaseLost, l.DeliveryID, l.Attempt)
		}
		return nil
	})
}

// MarkSent implements [store.MailOutboxRepo].
func (r *mailOutboxRepo) MarkSent(ctx context.Context, l domain.DeliveryLease, at time.Time) error {
	return r.complete(ctx, l, `status = 'delivered', lease_until = 0, last_error = '', updated_at = ?, sent_at = ?`,
		toMS(at), toMS(at))
}

// MarkFailed implements [store.MailOutboxRepo].
func (r *mailOutboxRepo) MarkFailed(ctx context.Context, l domain.DeliveryLease, at time.Time, lastErr string) error {
	return r.complete(ctx, l, `status = 'failed', lease_until = 0, last_error = ?, updated_at = ?`, lastErr, toMS(at))
}

// Reschedule implements [store.MailOutboxRepo].
func (r *mailOutboxRepo) Reschedule(ctx context.Context, l domain.DeliveryLease, at, next time.Time, lastErr string) error {
	return r.complete(ctx, l, `lease_until = 0, next_attempt_at = ?, last_error = ?, updated_at = ?`,
		toMS(next), lastErr, toMS(at))
}

// Get implements [store.MailOutboxRepo].
func (r *mailOutboxRepo) Get(ctx context.Context, id domain.ID) (domain.MailNotification, error) {
	n, err := scanMail(r.h.r.QueryRowContext(ctx, `SELECT `+mailCols+` FROM mail_outbox WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MailNotification{}, notFound("mail notification", id)
	}
	if err != nil {
		return domain.MailNotification{}, fmt.Errorf("sqlite: get mail notification: %w", err)
	}
	return n, nil
}

// CountPending implements [store.MailOutboxRepo].
func (r *mailOutboxRepo) CountPending(ctx context.Context) (int, error) {
	var n int
	if err := r.h.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_outbox WHERE status = 'pending'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: count pending mail: %w", err)
	}
	return n, nil
}

// Purge implements [store.MailOutboxRepo].
func (r *mailOutboxRepo) Purge(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`DELETE FROM mail_outbox WHERE status IN ('delivered', 'failed') AND updated_at < ?`, toMS(cutoff))
		if err != nil {
			return fmt.Errorf("sqlite: purge mail: %w", err)
		}
		n, err = rowsAffected(res)
		return err
	})
	return int(n), err
}
