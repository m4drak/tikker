// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package storetest

import (
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

func (e *env) webhook(name string, enabled bool, evs ...domain.WebhookEvent) domain.Webhook {
	e.t.Helper()
	w := domain.Webhook{
		ID: e.id(), Name: name, URL: "https://example.test/" + name, SecretEnc: []byte("enc-" + name),
		Events: evs, Enabled: enabled, CreatedAt: T0,
	}
	e.ok(e.s.Webhooks().Create(e.ctx, w))
	return w
}

func (e *env) delivery(w domain.Webhook, at time.Time) domain.WebhookDelivery {
	return domain.WebhookDelivery{
		ID: e.id(), WebhookID: w.ID, Event: domain.EventIncomingSMS, Payload: []byte(`{"n":1}`),
		CreatedAt: T0, NextAttemptAt: at,
	}
}

func testWebhooks(t *testing.T, f Factory) {
	e := newEnv(t, f)
	repo := e.s.Webhooks()

	a := e.webhook("alpha", true, domain.EventIncomingSMS)
	b := e.webhook("beta", true, domain.EventIncomingSMS, domain.EventMessageStatus)
	c := e.webhook("gamma", false, domain.EventIncomingSMS)

	got, err := repo.Get(e.ctx, a.ID)
	e.ok(err)
	if got.Name != "alpha" || got.URL != a.URL || string(got.SecretEnc) != "enc-alpha" || !got.Enabled ||
		len(got.Events) != 1 || got.Events[0] != domain.EventIncomingSMS || !got.CreatedAt.Equal(T0) {
		t.Fatalf("Get = %+v", got)
	}
	_, err = repo.Get(e.ctx, e.id())
	e.is(err, domain.ErrNotFound)

	e.is(repo.Create(e.ctx, domain.Webhook{ID: e.id(), Name: "ALPHA", URL: "https://x.test", Events: a.Events}), domain.ErrConflict)

	all, err := repo.List(e.ctx)
	e.ok(err)
	if len(all) != 3 || all[0].ID != a.ID || all[2].ID != c.ID {
		t.Fatalf("List = %+v", all)
	}

	sub, err := repo.Subscribed(e.ctx, domain.EventIncomingSMS)
	e.ok(err)
	if len(sub) != 2 || sub[0].ID != a.ID || sub[1].ID != b.ID {
		t.Fatalf("Subscribed(incoming) = %+v, want alpha and beta (gamma is disabled)", sub)
	}
	sub, err = repo.Subscribed(e.ctx, domain.EventMessageStatus)
	e.ok(err)
	if len(sub) != 1 || sub[0].ID != b.ID {
		t.Fatalf("Subscribed(status) = %+v", sub)
	}

	// Update keeps the secret when SecretEnc is nil and replaces it otherwise.
	a.URL, a.Events, a.Enabled, a.SecretEnc = "https://other.test/hook", []domain.WebhookEvent{domain.EventMessageStatus}, false, nil
	e.ok(repo.Update(e.ctx, a))
	got, _ = repo.Get(e.ctx, a.ID)
	if got.URL != a.URL || got.Enabled || string(got.SecretEnc) != "enc-alpha" || got.Events[0] != domain.EventMessageStatus {
		t.Fatalf("after Update = %+v", got)
	}
	a.SecretEnc = []byte("rotated")
	e.ok(repo.Update(e.ctx, a))
	got, _ = repo.Get(e.ctx, a.ID)
	if string(got.SecretEnc) != "rotated" {
		t.Fatalf("secret not replaced: %q", got.SecretEnc)
	}
	e.is(repo.Update(e.ctx, domain.Webhook{ID: e.id(), Name: "nobody", URL: "https://x.test"}), domain.ErrNotFound)
	dup := b
	dup.Name, dup.SecretEnc = "alpha", nil
	e.is(repo.Update(e.ctx, dup), domain.ErrConflict)

	// Delete cascades to deliveries.
	d := e.delivery(b, T0)
	e.ok(repo.AddDeliveries(e.ctx, []domain.WebhookDelivery{d}))
	e.ok(repo.Delete(e.ctx, b.ID))
	_, err = repo.GetDelivery(e.ctx, d.ID)
	e.is(err, domain.ErrNotFound)
	e.is(repo.Delete(e.ctx, b.ID), domain.ErrNotFound)
	e.is(repo.AddDeliveries(e.ctx, []domain.WebhookDelivery{d}), domain.ErrNotFound) // unknown webhook
}

func testWebhookDeliveries(t *testing.T, f Factory) {
	e := newEnv(t, f)
	repo := e.s.Webhooks()
	w := e.webhook("hook", true, domain.EventIncomingSMS)
	lease := T0.Add(time.Minute)

	// Nothing to do: no job, nothing written.
	_, ok, err := repo.AcquireDelivery(e.ctx, T0, lease)
	e.ok(err)
	if ok {
		t.Fatal("acquired from an empty queue")
	}
	if _, ok, err := repo.NextDeliveryDue(e.ctx); err != nil || ok {
		t.Fatalf("NextDeliveryDue on empty = %v, %v", ok, err)
	}

	d1 := e.delivery(w, T0)
	d2 := e.delivery(w, T0.Add(time.Hour)) // not due yet
	e.ok(repo.AddDeliveries(e.ctx, []domain.WebhookDelivery{d1, d2}))
	e.ok(repo.AddDeliveries(e.ctx, nil))

	at, ok, err := repo.NextDeliveryDue(e.ctx)
	e.ok(err)
	if !ok || !at.Equal(T0) {
		t.Fatalf("NextDeliveryDue = %v %v, want %v", at, ok, T0)
	}

	job, ok, err := repo.AcquireDelivery(e.ctx, T0, lease)
	e.ok(err)
	if !ok || job.Delivery.ID != d1.ID || job.Delivery.Attempts != 1 || !job.Delivery.LeaseUntil.Equal(lease) ||
		job.Webhook.ID != w.ID || job.Webhook.URL != w.URL || string(job.Delivery.Payload) != `{"n":1}` ||
		job.Delivery.Status != domain.DeliveryPending {
		t.Fatalf("job = %+v", job)
	}
	// d1 is leased, d2 is not due: nothing else to take.
	if _, ok, _ := repo.AcquireDelivery(e.ctx, T0, lease); ok {
		t.Fatal("leased delivery acquired twice")
	}
	// The next due moment is the lease expiry or d2, whichever is first.
	at, _, _ = repo.NextDeliveryDue(e.ctx)
	if !at.Equal(lease) {
		t.Fatalf("NextDeliveryDue = %v, want lease expiry %v", at, lease)
	}

	// Reschedule: back to pending with a later time.
	retry := T0.Add(30 * time.Second)
	e.ok(repo.RescheduleDelivery(e.ctx, job.Delivery.Lease(), T0, retry, "boom", 503))
	got, err := repo.GetDelivery(e.ctx, d1.ID)
	e.ok(err)
	if got.Status != domain.DeliveryPending || got.LastError != "boom" || got.LastHTTPCode != 503 ||
		!got.NextAttemptAt.Equal(retry) || !got.LeaseUntil.IsZero() || got.Attempts != 1 {
		t.Fatalf("after reschedule = %+v", got)
	}
	// The attempt is still the current one until the delivery is re-acquired;
	// once delivered, the same lease cannot complete it again.
	e.ok(repo.MarkDelivered(e.ctx, job.Delivery.Lease(), T0, 200))
	got, _ = repo.GetDelivery(e.ctx, d1.ID)
	if got.Status != domain.DeliveryDelivered || !got.DeliveredAt.Equal(T0) {
		t.Fatalf("delivered = %+v", got)
	}
	e.is(repo.MarkDelivered(e.ctx, job.Delivery.Lease(), T0, 200), domain.ErrLeaseLost)

	// Fencing: an attempt whose lease expired and was re-acquired loses.
	job2, ok, err := repo.AcquireDelivery(e.ctx, T0.Add(2*time.Hour), T0.Add(3*time.Hour)) // d2
	e.ok(err)
	if !ok || job2.Delivery.ID != d2.ID {
		t.Fatalf("job2 = %+v %v", job2, ok)
	}
	stale := job2.Delivery.Lease()
	again, ok, err := repo.AcquireDelivery(e.ctx, T0.Add(4*time.Hour), T0.Add(5*time.Hour)) // lease expired
	e.ok(err)
	if !ok || again.Delivery.ID != d2.ID || again.Delivery.Attempts != 2 {
		t.Fatalf("re-acquire = %+v %v", again, ok)
	}
	e.is(repo.MarkDelivered(e.ctx, stale, T0, 200), domain.ErrLeaseLost)
	e.is(repo.MarkDeliveryFailed(e.ctx, stale, T0, "x", 0), domain.ErrLeaseLost)
	e.is(repo.RescheduleDelivery(e.ctx, stale, T0, T0, "x", 0), domain.ErrLeaseLost)
	e.ok(repo.MarkDeliveryFailed(e.ctx, again.Delivery.Lease(), T0.Add(4*time.Hour), "gave up", 400))
	got, _ = repo.GetDelivery(e.ctx, d2.ID)
	if got.Status != domain.DeliveryFailed || got.LastHTTPCode != 400 || got.LastError != "gave up" {
		t.Fatalf("failed delivery = %+v", got)
	}

	// Disabling cancels what is queued: nothing is handed out, now or after
	// re-enabling, and the payload is dropped.
	d3 := e.delivery(w, T0)
	e.ok(repo.AddDeliveries(e.ctx, []domain.WebhookDelivery{d3}))
	w.Enabled, w.SecretEnc = false, nil
	e.ok(repo.Update(e.ctx, w))
	if _, ok, _ := repo.AcquireDelivery(e.ctx, T0.Add(10*time.Hour), T0.Add(11*time.Hour)); ok {
		t.Fatal("delivery of a disabled webhook acquired")
	}
	if _, ok, _ := repo.NextDeliveryDue(e.ctx); ok {
		t.Fatal("NextDeliveryDue counts a disabled webhook")
	}
	w.Enabled = true
	e.ok(repo.Update(e.ctx, w))
	got, _ = repo.GetDelivery(e.ctx, d3.ID)
	if got.Status != domain.DeliveryFailed || len(got.Payload) != 0 || got.LastError == "" {
		t.Fatalf("cancelled delivery = %+v", got)
	}
	if _, ok, _ := repo.AcquireDelivery(e.ctx, T0.Add(10*time.Hour), T0.Add(11*time.Hour)); ok {
		t.Fatal("cancelled delivery acquired after re-enabling")
	}
	// Terminal deliveries keep no payload.
	got, _ = repo.GetDelivery(e.ctx, d1.ID)
	if len(got.Payload) != 0 {
		t.Errorf("delivered payload kept: %q", got.Payload)
	}

	d4 := e.delivery(w, T0)
	e.ok(repo.AddDeliveries(e.ctx, []domain.WebhookDelivery{d4}))
	list, err := repo.ListDeliveries(e.ctx, w.ID, 10)
	e.ok(err)
	if len(list) != 4 || list[0].ID != d4.ID || list[3].ID != d1.ID {
		t.Fatalf("ListDeliveries = %+v", list)
	}
	if list, _ = repo.ListDeliveries(e.ctx, w.ID, 1); len(list) != 1 {
		t.Fatalf("limit ignored: %d", len(list))
	}

	// Purge removes only terminal deliveries older than the cutoff.
	// d1 delivered at T0, d3 cancelled at T0, d2 failed at T0+4h; d4 pending.
	for _, step := range []struct {
		cutoff time.Duration
		want   int
	}{{0, 0}, {time.Hour, 2}, {100 * time.Hour, 1}} {
		n, err := repo.PurgeDeliveries(e.ctx, T0.Add(step.cutoff))
		e.ok(err)
		if n != step.want {
			t.Fatalf("PurgeDeliveries(T0+%s) = %d, want %d", step.cutoff, n, step.want)
		}
	}
	if _, err := repo.GetDelivery(e.ctx, d4.ID); err != nil {
		t.Fatalf("pending delivery purged: %v", err)
	}
}

// A changed destination or a removed event must not send queued deliveries
// anywhere they were not meant for.
func testWebhookSubscriptionChanges(t *testing.T, f Factory) {
	e := newEnv(t, f)
	repo := e.s.Webhooks()
	w := e.webhook("hook", true, domain.EventIncomingSMS, domain.EventMessageStatus)
	in, st := e.delivery(w, T0), e.delivery(w, T0)
	st.Event = domain.EventMessageStatus
	e.ok(repo.AddDeliveries(e.ctx, []domain.WebhookDelivery{in, st}))

	state := func(id domain.ID) domain.WebhookDeliveryStatus {
		d, err := repo.GetDelivery(e.ctx, id)
		e.ok(err)
		return d.Status
	}
	// Renaming and rotating the secret keep the queue.
	w.Name, w.SecretEnc = "renamed", []byte("new")
	e.ok(repo.Update(e.ctx, w))
	if state(in.ID) != domain.DeliveryPending || state(st.ID) != domain.DeliveryPending {
		t.Fatal("rename cancelled deliveries")
	}
	// Removing an event cancels only that event's deliveries.
	w.SecretEnc, w.Events = nil, []domain.WebhookEvent{domain.EventIncomingSMS}
	e.ok(repo.Update(e.ctx, w))
	if state(in.ID) != domain.DeliveryPending || state(st.ID) != domain.DeliveryFailed {
		t.Fatalf("after event removal: in=%s status=%s", state(in.ID), state(st.ID))
	}
	// A new URL cancels everything queued; later deliveries use the new URL.
	w.URL = "https://elsewhere.test/hook"
	e.ok(repo.Update(e.ctx, w))
	if state(in.ID) != domain.DeliveryFailed {
		t.Fatal("URL change left a delivery for the old destination queued")
	}
	fresh := e.delivery(w, T0)
	e.ok(repo.AddDeliveries(e.ctx, []domain.WebhookDelivery{fresh}))
	job, ok, err := repo.AcquireDelivery(e.ctx, T0, T0.Add(time.Minute))
	e.ok(err)
	if !ok || job.Delivery.ID != fresh.ID || job.Webhook.URL != "https://elsewhere.test/hook" {
		t.Fatalf("job = %+v %v", job, ok)
	}

	// Retention: queued payloads do not outlive the cutoff.
	old := e.delivery(w, T0.Add(time.Hour))
	e.ok(repo.AddDeliveries(e.ctx, []domain.WebhookDelivery{old}))
	n, err := repo.ExpireDeliveries(e.ctx, T0.Add(time.Hour), T0.Add(2*time.Hour))
	e.ok(err)
	if n != 2 { // fresh (leased, created T0) and old? old created at T0
		t.Fatalf("ExpireDeliveries = %d, want 2", n)
	}
	d, _ := repo.GetDelivery(e.ctx, old.ID)
	if d.Status != domain.DeliveryFailed || len(d.Payload) != 0 {
		t.Fatalf("expired delivery = %+v", d)
	}
	e.is(repo.MarkDelivered(e.ctx, job.Delivery.Lease(), T0, 200), domain.ErrLeaseLost)
}

// testWebhookConcurrency: many workers drain the queue; every attempt is
// handed to exactly one of them.
func testWebhookConcurrency(t *testing.T, f Factory) {
	e := newEnv(t, f)
	repo := e.s.Webhooks()
	w := e.webhook("hook", true, domain.EventIncomingSMS)
	const total, workers = 40, 8
	for range total {
		e.ok(repo.AddDeliveries(e.ctx, []domain.WebhookDelivery{e.delivery(w, T0)}))
	}

	var mu sync.Mutex
	seen := map[domain.ID]int{}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, ok, err := repo.AcquireDelivery(e.ctx, T0, T0.Add(time.Hour))
				if err != nil {
					t.Errorf("AcquireDelivery: %v", err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				seen[job.Delivery.ID]++
				mu.Unlock()
				if err := repo.MarkDelivered(e.ctx, job.Delivery.Lease(), T0, 200); err != nil {
					t.Errorf("MarkDelivered: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("%d deliveries handed out, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("delivery %s handed out %d times", id, n)
		}
	}
}
