// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

type inboundPartRepo struct{ h *handle }

var _ store.InboundPartRepo = (*inboundPartRepo)(nil)

// InboundParts implements [store.Repos].
func (h *handle) InboundParts() store.InboundPartRepo { return &inboundPartRepo{h} }

// Add implements [store.InboundPartRepo].
func (r *inboundPartRepo) Add(ctx context.Context, p store.InboundPart) (bool, error) {
	if p.ID.IsZero() || p.ChannelID.IsZero() || p.DedupHash == "" {
		return false, errors.New("sqlite: inbound part needs ID, ChannelID and DedupHash")
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = r.h.s.clk.Now()
	}
	var created bool
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`INSERT INTO inbound_parts
			   (id, channel_id, dedup_hash, phone, raw_timestamp, received_at, body, pdu, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (dedup_hash) DO NOTHING`,
			string(p.ID), string(p.ChannelID), p.DedupHash, p.Phone, p.RawTimestamp,
			toMS(p.ReceivedAt), p.Body, p.PDU, toMS(p.CreatedAt))
		if err != nil {
			return fmt.Errorf("sqlite: add inbound part: %w", mapErr(err))
		}
		n, err := rowsAffected(res)
		created = n > 0
		return err
	})
	return created, err
}

// ListPending implements [store.InboundPartRepo].
func (r *inboundPartRepo) ListPending(ctx context.Context, channelID domain.ID) ([]store.InboundPart, error) {
	rows, err := r.h.r.QueryContext(ctx,
		`SELECT id, channel_id, dedup_hash, phone, raw_timestamp, received_at, body, pdu, created_at
		   FROM inbound_parts WHERE channel_id = ? AND message_id = ''
		  ORDER BY created_at, id`, string(channelID))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list inbound parts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.InboundPart
	for rows.Next() {
		var (
			p                     store.InboundPart
			id, ch                string
			receivedAt, createdAt int64
		)
		if err := rows.Scan(&id, &ch, &p.DedupHash, &p.Phone, &p.RawTimestamp, &receivedAt, &p.Body, &p.PDU, &createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan inbound part: %w", err)
		}
		p.ID, p.ChannelID = domain.ID(id), domain.ID(ch)
		p.ReceivedAt, p.CreatedAt = fromMS(receivedAt), fromMS(createdAt)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list inbound parts: %w", err)
	}
	return out, nil
}

// Consume implements [store.InboundPartRepo].
func (r *inboundPartRepo) Consume(ctx context.Context, ids []domain.ID, messageID domain.ID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	if messageID.IsZero() {
		return errors.New("sqlite: consume inbound parts without message ID")
	}
	return r.h.write(ctx, func(q execer) error {
		args := []any{string(messageID), toMS(at)}
		for _, id := range ids {
			args = append(args, string(id))
		}
		_, err := q.ExecContext(ctx,
			`UPDATE inbound_parts SET message_id = ?, consumed_at = ?, pdu = '', body = ''
			  WHERE message_id = '' AND id IN (`+placeholders(len(ids))+`)`, args...)
		if err != nil {
			return fmt.Errorf("sqlite: consume inbound parts: %w", err)
		}
		return nil
	})
}

// PurgeConsumed implements [store.InboundPartRepo].
func (r *inboundPartRepo) PurgeConsumed(ctx context.Context, before time.Time) (int, error) {
	var n int64
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx,
			`DELETE FROM inbound_parts WHERE message_id <> '' AND consumed_at < ?`, toMS(before))
		if err != nil {
			return fmt.Errorf("sqlite: purge inbound parts: %w", err)
		}
		n, err = rowsAffected(res)
		return err
	})
	return int(n), err
}
