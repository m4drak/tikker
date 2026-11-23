// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/webhook"
)

// flakyEvents fails PublishStatus while fail is set.
type flakyEvents struct {
	StatusPublisher
	fail atomic.Bool
}

func (f *flakyEvents) PublishStatus(ctx context.Context, tx store.Repos, m domain.Message) error {
	if f.fail.Load() {
		return errors.New("event store down")
	}
	return f.StatusPublisher.PublishStatus(ctx, tx, m)
}

type countingWaker struct{ n atomic.Int32 }

func (w *countingWaker) Wake() { w.n.Add(1) }

// failingEvents is a StatusPublisher whose PublishStatus always fails.
type failingEvents struct{ StatusPublisher }

func (failingEvents) PublishStatus(context.Context, store.Repos, domain.Message) error {
	return errors.New("event store down")
}

func (e *env) hook(events ...domain.WebhookEvent) domain.Webhook {
	e.t.Helper()
	w := domain.Webhook{
		ID: e.ids.NewID(), Name: "crm", URL: "https://example.test/hook", SecretEnc: []byte("x"),
		Events: events, Enabled: true, CreatedAt: e.clk.Now(),
	}
	if err := e.st.Webhooks().Create(e.ctx, w); err != nil {
		e.t.Fatal(err)
	}
	return w
}

func TestStatusEventsAreStoredWithTheResult(t *testing.T) {
	tests := []struct {
		name       string
		sendErr    error
		wantStatus domain.MessageStatus
	}{
		{"sent", nil, domain.StatusSent},
		{"permanent failure", domain.Permanent("send", errors.New("bad number")), domain.StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			waker := &countingWaker{}
			var pub *webhook.Publisher
			e := newEnv(t, envConfig{tx: true, opts: func(o *Options) {
				pub = webhook.NewPublisher(o.Clock, nil, waker)
				o.Events = pub
			}})
			w := e.hook(domain.EventMessageStatus)
			conn := newStub(e.clk, func(context.Context, int, stubCall) (connector.SendResult, error) {
				return connector.SendResult{}, tt.sendErr
			})
			e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
			e.start()
			m := e.enqueue(e.msg("+48600000001"))[0]
			e.waitStatus(chA, tt.wantStatus, m.ID)

			ds, err := e.st.Webhooks().ListDeliveries(e.ctx, w.ID, 10)
			if err != nil || len(ds) != 1 {
				t.Fatalf("deliveries = %v, %v", ds, err)
			}
			var env webhook.Envelope
			if err := json.Unmarshal(ds[0].Payload, &env); err != nil {
				t.Fatal(err)
			}
			if ds[0].Event != domain.EventMessageStatus || env.Message.ID != m.ID.String() ||
				env.Message.Status != tt.wantStatus.String() || env.Message.Direction != "out" || env.Message.Body != nil {
				t.Errorf("payload = %s", ds[0].Payload)
			}
			if waker.n.Load() == 0 {
				t.Error("sender was not woken after the commit")
			}
		})
	}
}

func TestNoEventForRetriedAttempt(t *testing.T) {
	e := newEnv(t, envConfig{tx: true, opts: func(o *Options) { o.Events = webhook.NewPublisher(o.Clock, nil, nil) }})
	w := e.hook(domain.EventMessageStatus, domain.EventIncomingSMS)
	conn := newStub(e.clk, func(context.Context, int, stubCall) (connector.SendResult, error) {
		return connector.SendResult{}, domain.Transient("send", errors.New("busy"))
	})
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	m := e.enqueue(e.msg("+48600000001"))[0]
	e.waitStatus(chA, domain.StatusQueued, m.ID)
	e.tr.wait(t, "settled", func(v view) bool { return conn.N() == 1 && v.asleep(chA) })
	if ds, _ := e.st.Webhooks().ListDeliveries(e.ctx, w.ID, 10); len(ds) != 0 {
		t.Fatalf("a requeued message produced %d events", len(ds))
	}
}

// If the event cannot be stored, the result is not recorded either: the lease
// expires and the message is recovered (ADR 0002), the event is not lost.
func TestResultAndEventCommitTogether(t *testing.T) {
	e := newEnv(t, envConfig{tx: true, opts: func(o *Options) {
		o.Events = failingEvents{webhook.NewPublisher(o.Clock, nil, nil)}
	}})
	conn := newStub(e.clk, nil)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	m := e.enqueue(e.msg("+48600000001"))[0]
	e.tr.wait(t, "send attempted", func(v view) bool { return conn.N() == 1 && v.asleep(chA) })
	if got := e.get(m.ID); got.Status != domain.StatusSending {
		t.Fatalf("status = %s, want sending (result must roll back with the event)", got.Status)
	}
	if e.d.Stats().Totals.StoreErrors == 0 {
		t.Error("store error not counted")
	}
}

func TestEventsRequireTx(t *testing.T) {
	_, err := New(Options{Messages: newEnv(t, envConfig{}).repo, Events: webhook.NewPublisher(nil, nil, nil)})
	if err == nil {
		t.Fatal("Events without Tx accepted")
	}
}

func (e *env) deliveryFor(w domain.Webhook, msg domain.ID) *webhook.Envelope {
	e.t.Helper()
	ds, err := e.st.Webhooks().ListDeliveries(e.ctx, w.ID, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, d := range ds {
		var env webhook.Envelope
		if err := json.Unmarshal(d.Payload, &env); err == nil && env.Message.ID == msg.String() {
			return &env
		}
	}
	return nil
}

// Lease recovery and expiry change the status and enqueue the event in one
// transaction: a failing publisher leaves the message untouched, and the next
// pass produces both.
func TestRecoveryAndExpiryEventsAreAtomic(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *env) domain.Message
		run   func(d *Dispatcher, ctx context.Context)
		// the status the message has while the publisher fails
		before domain.MessageStatus
		reason string
	}{
		{
			name: "lease recovery",
			setup: func(e *env) domain.Message {
				m := e.enqueue(e.msg("+48600000001"))[0]
				_, ok, err := e.st.Messages().AcquireNext(e.ctx, store.AcquireRequest{
					ChannelID: chA, Now: e.clk.Now(), LeaseUntil: e.clk.Now().Add(time.Minute),
				})
				if err != nil || !ok {
					e.t.Fatalf("acquire = %v %v", ok, err)
				}
				e.clk.Advance(time.Hour)
				return m
			},
			run:    func(d *Dispatcher, ctx context.Context) { _ = d.recover(ctx) },
			before: domain.StatusSending, reason: "lease expired",
		},
		{
			name: "expiry",
			setup: func(e *env) domain.Message {
				m := e.enqueue(e.msg("+48600000001", func(m *domain.Message) { m.ExpiresAt = e.clk.Now().Add(time.Minute) }))[0]
				e.clk.Advance(time.Hour)
				return m
			},
			run:    func(d *Dispatcher, ctx context.Context) { d.expire(ctx) },
			before: domain.StatusQueued, reason: domain.LastErrorExpired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			waker := &countingWaker{}
			ev := &flakyEvents{StatusPublisher: nil}
			e := newEnv(t, envConfig{tx: true, opts: func(o *Options) {
				o.Retry = domain.DefaultRetryPolicy()
				o.Retry.MaxAttempts = 1 // a recovered lease fails at once
				ev.StatusPublisher = webhook.NewPublisher(o.Clock, nil, waker)
				o.Events = ev
			}})
			w := e.hook(domain.EventMessageStatus)
			m := tt.setup(e)

			ev.fail.Store(true)
			tt.run(e.d, e.ctx)
			if got := e.get(m.ID); got.Status != tt.before {
				t.Fatalf("status = %s while the event failed, want %s (rolled back)", got.Status, tt.before)
			}
			if e.deliveryFor(w, m.ID) != nil {
				t.Fatal("event stored although the transaction failed")
			}

			ev.fail.Store(false)
			tt.run(e.d, e.ctx)
			got := e.get(m.ID)
			if got.Status != domain.StatusFailed || !strings.Contains(got.LastError, tt.reason) {
				t.Fatalf("message = %+v", got)
			}
			env := e.deliveryFor(w, m.ID)
			if env == nil || env.Message.Status != "failed" || env.Message.Direction != "out" {
				t.Fatalf("no failed event for the message: %+v", env)
			}
			if waker.n.Load() == 0 {
				t.Error("sender not woken after the commit")
			}
			// Once: another pass changes nothing.
			tt.run(e.d, e.ctx)
			if ds, _ := e.st.Webhooks().ListDeliveries(e.ctx, w.ID, 10); len(ds) != 1 {
				t.Fatalf("%d deliveries, want 1", len(ds))
			}
		})
	}
}
