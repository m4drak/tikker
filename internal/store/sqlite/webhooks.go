// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// Webhooks implements [store.Repos].
func (h *handle) Webhooks() store.WebhookRepo { return &webhookRepo{h} }

type webhookRepo struct{ h *handle }

var _ store.WebhookRepo = (*webhookRepo)(nil)

const webhookCols = `id, name, url, secret_enc, events, enabled, created_at`

func scanWebhook(sc scanner) (domain.Webhook, error) {
	var w domain.Webhook
	var id, events string
	var enabled int
	var created int64
	if err := sc.Scan(&id, &w.Name, &w.URL, &w.SecretEnc, &events, &enabled, &created); err != nil {
		return domain.Webhook{}, err
	}
	w.ID = domain.ID(id)
	w.Enabled = enabled != 0
	w.CreatedAt = fromMS(created)
	var names []string
	if err := json.Unmarshal([]byte(events), &names); err != nil {
		return domain.Webhook{}, fmt.Errorf("webhook %s: bad events column: %w", id, err)
	}
	for _, n := range names {
		w.Events = append(w.Events, domain.WebhookEvent(n))
	}
	return w, nil
}

func eventsJSON(evs []domain.WebhookEvent) string {
	names := make([]string, len(evs))
	for i, e := range evs {
		names[i] = string(e)
	}
	return marshalJSON(names)
}

// Create implements [store.WebhookRepo].
func (r *webhookRepo) Create(ctx context.Context, w domain.Webhook) error {
	if w.CreatedAt.IsZero() {
		w.CreatedAt = r.h.s.clk.Now()
	}
	secret := w.SecretEnc
	if secret == nil {
		secret = []byte{}
	}
	return r.h.write(ctx, func(q execer) error {
		_, err := q.ExecContext(ctx,
			`INSERT INTO webhooks (`+webhookCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			string(w.ID), w.Name, w.URL, secret, eventsJSON(w.Events), boolInt(w.Enabled), toMS(w.CreatedAt))
		if err != nil {
			return fmt.Errorf("sqlite: create webhook: %w", mapErr(err))
		}
		return nil
	})
}

// Get implements [store.WebhookRepo].
func (r *webhookRepo) Get(ctx context.Context, id domain.ID) (domain.Webhook, error) {
	return getWebhook(ctx, r.h.r, id)
}

func getWebhook(ctx context.Context, q execer, id domain.ID) (domain.Webhook, error) {
	w, err := scanWebhook(q.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Webhook{}, notFound("webhook", id)
	}
	if err != nil {
		return domain.Webhook{}, fmt.Errorf("sqlite: get webhook: %w", err)
	}
	return w, nil
}

func (r *webhookRepo) query(ctx context.Context, where string, args ...any) ([]domain.Webhook, error) {
	rows, err := r.h.r.QueryContext(ctx, `SELECT `+webhookCols+` FROM webhooks `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list webhooks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list webhooks: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list webhooks: %w", err)
	}
	return out, nil
}

// List implements [store.WebhookRepo].
func (r *webhookRepo) List(ctx context.Context) ([]domain.Webhook, error) {
	return r.query(ctx, "")
}

// Subscribed implements [store.WebhookRepo].
func (r *webhookRepo) Subscribed(ctx context.Context, ev domain.WebhookEvent) ([]domain.Webhook, error) {
	return r.query(ctx,
		`WHERE enabled = 1 AND EXISTS (SELECT 1 FROM json_each(webhooks.events) WHERE value = ?)`, string(ev))
}

// Update implements [store.WebhookRepo]. Changing the URL or disabling the
// webhook bumps its version and cancels the queued deliveries (they were
// meant for the old destination); removing an event cancels the queued
// deliveries of that event. All in one transaction.
func (r *webhookRepo) Update(ctx context.Context, w domain.Webhook) error {
	var secret any // NULL keeps the stored secret
	if w.SecretEnc != nil {
		secret = w.SecretEnc
	}
	return r.h.write(ctx, func(q execer) error {
		old, err := getWebhook(ctx, q, w.ID)
		if err != nil {
			return err
		}
		bump := old.URL != w.URL || (old.Enabled && !w.Enabled)
		_, err = q.ExecContext(ctx,
			`UPDATE webhooks SET name = ?, url = ?, secret_enc = COALESCE(?, secret_enc), events = ?, enabled = ?,
			        version = version + ?
			  WHERE id = ?`,
			w.Name, w.URL, secret, eventsJSON(w.Events), boolInt(w.Enabled), boolInt(bump), string(w.ID))
		if err != nil {
			return fmt.Errorf("sqlite: update webhook: %w", mapErr(err))
		}
		now := toMS(r.h.s.clk.Now())
		const reason = "cancelled: the webhook was changed"
		switch {
		case bump:
			_, err = q.ExecContext(ctx, cancelSQL+` AND webhook_id = ?`, reason, now, string(w.ID))
		case len(w.Events) == 0:
			_, err = q.ExecContext(ctx, cancelSQL+` AND webhook_id = ?`, reason, now, string(w.ID))
		default:
			args := []any{reason, now, string(w.ID)}
			for _, e := range w.Events {
				args = append(args, string(e))
			}
			_, err = q.ExecContext(ctx,
				cancelSQL+` AND webhook_id = ? AND event NOT IN (`+placeholders(len(w.Events))+`)`, args...)
		}
		if err != nil {
			return fmt.Errorf("sqlite: cancel deliveries: %w", err)
		}
		return nil
	})
}

// cancelSQL fails pending deliveries and drops their payload (args: reason,
// now; the caller appends conditions).
const cancelSQL = `UPDATE webhook_deliveries SET status = 'failed', lease_until = 0, payload = x'',
	last_error = ?, updated_at = ? WHERE status = 'pending'`

// Delete implements [store.WebhookRepo].
func (r *webhookRepo) Delete(ctx context.Context, id domain.ID) error {
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, string(id))
		if err != nil {
			return fmt.Errorf("sqlite: delete webhook: %w", err)
		}
		n, err := rowsAffected(res)
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("webhook", id)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Deliveries

const deliveryCols = `id, webhook_id, event, payload, status, attempts, next_attempt_at, lease_until,
	last_error, last_http_code, created_at, updated_at, delivered_at`

func scanDelivery(sc scanner) (domain.WebhookDelivery, error) {
	var d domain.WebhookDelivery
	var id, hook, event, status string
	var next, lease, created, updated, delivered int64
	err := sc.Scan(&id, &hook, &event, &d.Payload, &status, &d.Attempts, &next, &lease,
		&d.LastError, &d.LastHTTPCode, &created, &updated, &delivered)
	if err != nil {
		return domain.WebhookDelivery{}, err
	}
	d.ID, d.WebhookID = domain.ID(id), domain.ID(hook)
	d.Event = domain.WebhookEvent(event)
	d.Status = domain.WebhookDeliveryStatus(status)
	d.NextAttemptAt, d.LeaseUntil = fromMS(next), fromMS(lease)
	d.CreatedAt, d.UpdatedAt, d.DeliveredAt = fromMS(created), fromMS(updated), fromMS(delivered)
	return d, nil
}

// AddDeliveries implements [store.WebhookRepo].
func (r *webhookRepo) AddDeliveries(ctx context.Context, ds []domain.WebhookDelivery) error {
	if len(ds) == 0 {
		return nil
	}
	return r.h.write(ctx, func(q execer) error {
		for _, d := range ds {
			created := d.CreatedAt
			if created.IsZero() {
				created = r.h.s.clk.Now()
			}
			next := d.NextAttemptAt
			if next.IsZero() {
				next = created
			}
			_, err := q.ExecContext(ctx,
				`INSERT INTO webhook_deliveries (id, webhook_id, event, payload, webhook_version, status, attempts,
				        next_attempt_at, lease_until, last_error, last_http_code, created_at, updated_at, delivered_at)
				 VALUES (?1, ?2, ?3, ?4, COALESCE((SELECT version FROM webhooks WHERE id = ?2), 1), 'pending', 0, ?5, 0, '', 0, ?6, ?6, 0)`,
				string(d.ID), string(d.WebhookID), string(d.Event), d.Payload, toMS(next), toMS(created))
			if err != nil {
				return fmt.Errorf("sqlite: add delivery: %w", mapErr(err))
			}
		}
		return nil
	})
}

// A delivery is due when pending, its webhook is enabled, the retry time has
// come and no attempt holds a live lease.
const deliveryDueSQL = `d.status = 'pending' AND d.next_attempt_at <= ?1 AND d.lease_until <= ?1
	AND EXISTS (SELECT 1 FROM webhooks w WHERE w.id = d.webhook_id AND w.enabled = 1 AND w.version = d.webhook_version)`

// AcquireDelivery implements [store.WebhookRepo].
func (r *webhookRepo) AcquireDelivery(ctx context.Context, now, leaseUntil time.Time) (store.DeliveryJob, bool, error) {
	// Cheap read-only probe, so an idle poll takes no write lock at all.
	if r.h.tx == nil {
		var one int
		err := r.h.r.QueryRowContext(ctx,
			`SELECT 1 FROM webhook_deliveries d WHERE `+deliveryDueSQL+` LIMIT 1`, toMS(now)).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return store.DeliveryJob{}, false, nil
		}
		if err != nil {
			return store.DeliveryJob{}, false, fmt.Errorf("sqlite: acquire delivery probe: %w", err)
		}
	}
	var job store.DeliveryJob
	err := r.h.write(ctx, func(q execer) error {
		d, err := scanDelivery(q.QueryRowContext(ctx,
			`UPDATE webhook_deliveries
			    SET attempts = attempts + 1, lease_until = ?2, updated_at = ?1
			  WHERE id = (SELECT d.id FROM webhook_deliveries d WHERE `+deliveryDueSQL+`
			               ORDER BY d.next_attempt_at, d.id LIMIT 1)
			RETURNING `+deliveryCols, toMS(now), toMS(leaseUntil)))
		if err != nil {
			return err
		}
		w, err := getWebhook(ctx, q, d.WebhookID)
		if err != nil {
			return err
		}
		job = store.DeliveryJob{Delivery: d, Webhook: w}
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return store.DeliveryJob{}, false, nil
	}
	if err != nil {
		return store.DeliveryJob{}, false, fmt.Errorf("sqlite: acquire delivery: %w", err)
	}
	return job, true, nil
}

// NextDeliveryDue implements [store.WebhookRepo].
func (r *webhookRepo) NextDeliveryDue(ctx context.Context) (time.Time, bool, error) {
	var at sql.NullInt64
	err := r.h.r.QueryRowContext(ctx,
		`SELECT MIN(MAX(d.next_attempt_at, d.lease_until)) FROM webhook_deliveries d
		  WHERE d.status = 'pending'
		    AND EXISTS (SELECT 1 FROM webhooks w WHERE w.id = d.webhook_id AND w.enabled = 1 AND w.version = d.webhook_version)`).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("sqlite: next delivery due: %w", err)
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(at.Int64).UTC(), true, nil
}

// completeDelivery applies set to the delivery while the attempt is current.
func (r *webhookRepo) completeDelivery(ctx context.Context, l domain.DeliveryLease, set string, args ...any) error {
	args = append(args, string(l.DeliveryID), l.Attempt)
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`UPDATE webhook_deliveries SET `+set+` WHERE id = ? AND status = 'pending' AND attempts = ?`, args...)
		if err != nil {
			return fmt.Errorf("sqlite: complete delivery: %w", err)
		}
		n, err := rowsAffected(res)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: delivery %s attempt %d", domain.ErrLeaseLost, l.DeliveryID, l.Attempt)
		}
		return nil
	})
}

// MarkDelivered implements [store.WebhookRepo].
func (r *webhookRepo) MarkDelivered(ctx context.Context, l domain.DeliveryLease, at time.Time, httpCode int) error {
	return r.completeDelivery(ctx, l,
		`status = 'delivered', lease_until = 0, payload = x'', last_error = '', last_http_code = ?, updated_at = ?, delivered_at = ?`,
		httpCode, toMS(at), toMS(at))
}

// MarkDeliveryFailed implements [store.WebhookRepo].
func (r *webhookRepo) MarkDeliveryFailed(ctx context.Context, l domain.DeliveryLease, at time.Time, lastErr string, httpCode int) error {
	return r.completeDelivery(ctx, l,
		`status = 'failed', lease_until = 0, payload = x'', last_error = ?, last_http_code = ?, updated_at = ?`,
		lastErr, httpCode, toMS(at))
}

// RescheduleDelivery implements [store.WebhookRepo].
func (r *webhookRepo) RescheduleDelivery(ctx context.Context, l domain.DeliveryLease, at, next time.Time, lastErr string, httpCode int) error {
	return r.completeDelivery(ctx, l,
		`lease_until = 0, next_attempt_at = ?, last_error = ?, last_http_code = ?, updated_at = ?`,
		toMS(next), lastErr, httpCode, toMS(at))
}

// GetDelivery implements [store.WebhookRepo].
func (r *webhookRepo) GetDelivery(ctx context.Context, id domain.ID) (domain.WebhookDelivery, error) {
	d, err := scanDelivery(r.h.r.QueryRowContext(ctx,
		`SELECT `+deliveryCols+` FROM webhook_deliveries WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WebhookDelivery{}, notFound("webhook delivery", id)
	}
	if err != nil {
		return domain.WebhookDelivery{}, fmt.Errorf("sqlite: get delivery: %w", err)
	}
	return d, nil
}

// ListDeliveries implements [store.WebhookRepo].
func (r *webhookRepo) ListDeliveries(ctx context.Context, webhookID domain.ID, limit int) ([]domain.WebhookDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.h.r.QueryContext(ctx,
		`SELECT `+deliveryCols+` FROM webhook_deliveries WHERE webhook_id = ? ORDER BY id DESC LIMIT ?`,
		string(webhookID), limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.WebhookDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list deliveries: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list deliveries: %w", err)
	}
	return out, nil
}

// PurgeDeliveries implements [store.WebhookRepo].
func (r *webhookRepo) PurgeDeliveries(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`DELETE FROM webhook_deliveries WHERE status IN ('delivered', 'failed') AND updated_at < ?`, toMS(cutoff))
		if err != nil {
			return fmt.Errorf("sqlite: purge deliveries: %w", err)
		}
		n, err = rowsAffected(res)
		return err
	})
	return int(n), err
}

// ExpireDeliveries implements [store.WebhookRepo].
func (r *webhookRepo) ExpireDeliveries(ctx context.Context, cutoff, now time.Time) (int, error) {
	var n int64
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx, cancelSQL+` AND created_at < ?`,
			"expired: retention period passed before delivery", toMS(now), toMS(cutoff))
		if err != nil {
			return fmt.Errorf("sqlite: expire deliveries: %w", err)
		}
		n, err = rowsAffected(res)
		return err
	})
	return int(n), err
}
