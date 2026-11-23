// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/webhook"
)

type countingWaker struct{ n atomic.Int32 }

func (w *countingWaker) Wake() { w.n.Add(1) }

func (e *env) hook() domain.Webhook {
	e.t.Helper()
	w := domain.Webhook{
		ID: "01JHOOK00000000000000000AA", Name: "crm", URL: "https://example.test/hook", SecretEnc: []byte("x"),
		Events: []domain.WebhookEvent{domain.EventIncomingSMS}, Enabled: true, CreatedAt: t0,
	}
	if err := e.st.Webhooks().Create(e.ctx, w); err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) deliveries(w domain.Webhook) []domain.WebhookDelivery {
	e.t.Helper()
	ds, err := e.st.Webhooks().ListDeliveries(e.ctx, w.ID, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	return ds
}

func TestIncomingMessagesProduceOneEventEach(t *testing.T) {
	waker := &countingWaker{}
	e := newEnv(t, func(c *Config) { c.Events = webhook.NewPublisher(nil, nil, waker) })
	w := e.hook()

	e.inject("first")
	e.injectParts("A long message that does not fit in one segment, "+
		"so the router delivers it as several parts that tikker joins into one text again. "+
		"It needs to be long enough to need at least two parts of 153 characters each, so here is more text.", 7)
	if res := e.poll(); res.Stored != 2 {
		t.Fatalf("result = %+v", res)
	}
	ds := e.deliveries(w)
	if len(ds) != 2 {
		t.Fatalf("%d deliveries for 2 stored messages", len(ds))
	}
	stored := e.inbound()
	bodies := map[string]bool{}
	for _, d := range ds {
		var env webhook.Envelope
		if err := json.Unmarshal(d.Payload, &env); err != nil {
			t.Fatal(err)
		}
		if d.Event != domain.EventIncomingSMS || env.Message.Body == nil || env.Message.Peer != sender {
			t.Errorf("payload = %s", d.Payload)
		}
		bodies[*env.Message.Body] = true
	}
	for _, m := range stored {
		if !bodies[m.Body] {
			t.Errorf("no event carries the text of message %s", m.ID)
		}
	}
	if waker.n.Load() != 1 {
		t.Errorf("sender woken %d times, want once per batch", waker.n.Load())
	}

	// Entries seen again (a crash before the router delete) are duplicates
	// and must not produce a second event.
	e.srv.Reset()
	e.inject("first")
	if res := e.poll(); res.Stored != 0 || res.Duplicates != 1 {
		t.Fatalf("second poll = %+v, want 1 duplicate", res)
	}
	if got := len(e.deliveries(w)); got != 2 {
		t.Fatalf("deliveries = %d after a duplicate, want 2", got)
	}
}

// A failed event rolls the message back and leaves the entry on the router, so
// the next cycle stores both.
func TestFailedEventRollsBackTheMessage(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	e := newEnv(t, func(c *Config) {
		realPub := webhook.NewPublisher(nil, nil, nil)
		c.Events = switchable{realPub, &fail}
	})
	w := e.hook()
	e.inject("keep me")
	if _, err := e.p.PollOnce(e.ctx); err == nil {
		t.Fatal("poll succeeded although the event could not be stored")
	}
	if n := len(e.inbound()); n != 0 {
		t.Fatalf("%d messages stored without their event", n)
	}
	if n := e.routerInbox(); n != 1 {
		t.Fatalf("router inbox holds %d entries, want 1 (not deleted)", n)
	}
	fail.Store(false)
	if res := e.poll(); res.Stored != 1 {
		t.Fatalf("retry result = %+v", res)
	}
	if n := len(e.deliveries(w)); n != 1 {
		t.Fatalf("%d deliveries, want 1", n)
	}
}

type switchable struct {
	MessagePublisher
	fail *atomic.Bool
}

func (s switchable) PublishIncoming(ctx context.Context, tx store.Repos, m domain.Message) error {
	if s.fail.Load() {
		return errors.New("event store down")
	}
	return s.MessagePublisher.PublishIncoming(ctx, tx, m)
}
