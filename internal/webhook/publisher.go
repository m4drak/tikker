// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package webhook

import (
	"context"
	"fmt"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// Waker is told that new deliveries were committed. *Sender implements it.
type Waker interface{ Wake() }

// Publisher turns events into pending deliveries. Its Publish methods take
// the repositories of the transaction that stores the event, so the event and
// its deliveries commit together or not at all. Call [Publisher.Wake] after
// the commit.
type Publisher struct {
	clk   clock.Clock
	ids   domain.IDGenerator
	waker Waker
}

// NewPublisher returns a publisher. waker may be nil (tests).
func NewPublisher(clk clock.Clock, ids domain.IDGenerator, waker Waker) *Publisher {
	if clk == nil {
		clk = clock.System()
	}
	if ids == nil {
		ids = domain.NewULIDGenerator(clk)
	}
	return &Publisher{clk: clk, ids: ids, waker: waker}
}

// PublishIncoming records the event "incoming_sms" for message m.
func (p *Publisher) PublishIncoming(ctx context.Context, tx store.Repos, m domain.Message) error {
	return p.publish(ctx, tx, domain.EventIncomingSMS, m)
}

// PublishStatus records the event "message_status" for outbound message m,
// which must carry its new status.
func (p *Publisher) PublishStatus(ctx context.Context, tx store.Repos, m domain.Message) error {
	return p.publish(ctx, tx, domain.EventMessageStatus, m)
}

func (p *Publisher) publish(ctx context.Context, tx store.Repos, ev domain.WebhookEvent, m domain.Message) error {
	hooks, err := tx.Webhooks().Subscribed(ctx, ev)
	if err != nil {
		return fmt.Errorf("webhook: list subscribers: %w", err)
	}
	if len(hooks) == 0 {
		return nil
	}
	now := p.clk.Now()
	ds := make([]domain.WebhookDelivery, len(hooks))
	for i, h := range hooks {
		id := p.ids.NewID()
		payload, err := BuildPayload(id, ev, m, now) // the ID is signed with the body
		if err != nil {
			return err
		}
		ds[i] = domain.WebhookDelivery{
			ID: id, WebhookID: h.ID, Event: ev, Payload: payload, CreatedAt: now, NextAttemptAt: now,
		}
	}
	if err := tx.Webhooks().AddDeliveries(ctx, ds); err != nil {
		return fmt.Errorf("webhook: store deliveries: %w", err)
	}
	return nil
}

// Wake tells the sender that deliveries were committed. It never blocks.
func (p *Publisher) Wake() {
	if p.waker != nil {
		p.waker.Wake()
	}
}
