// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package storetest

import (
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

func testMailOutbox(t *testing.T, f Factory) {
	e := newEnv(t, f)
	repo := e.s.MailOutbox()

	if _, ok, err := repo.Acquire(e.ctx, T0, T0.Add(time.Minute)); err != nil || ok {
		t.Fatalf("Acquire on empty = %v %v", ok, err)
	}
	if _, ok, err := repo.NextDue(e.ctx); err != nil || ok {
		t.Fatalf("NextDue on empty = %v %v", ok, err)
	}

	a := domain.MailNotification{ID: e.id(), MessageID: e.id(), CreatedAt: T0}
	b := domain.MailNotification{ID: e.id(), MessageID: e.id(), CreatedAt: T0, NextAttemptAt: T0.Add(time.Hour)}
	// Added inside the transaction of the event.
	e.ok(e.s.WithTx(e.ctx, func(tx store.Repos) error { return tx.MailOutbox().Add(e.ctx, a) }))
	e.ok(repo.Add(e.ctx, b))
	e.is(repo.Add(e.ctx, a), domain.ErrConflict)

	at, ok, err := repo.NextDue(e.ctx)
	e.ok(err)
	if !ok || !at.Equal(T0) {
		t.Fatalf("NextDue = %v %v", at, ok)
	}

	got, ok, err := repo.Acquire(e.ctx, T0, T0.Add(time.Minute))
	e.ok(err)
	if !ok || got.ID != a.ID || got.MessageID != a.MessageID || got.Attempts != 1 || !got.LeaseUntil.Equal(T0.Add(time.Minute)) {
		t.Fatalf("Acquire = %+v %v", got, ok)
	}
	// Leased: not due again until the lease ends; b is not due yet.
	if _, ok, _ := repo.Acquire(e.ctx, T0.Add(time.Second), T0.Add(time.Minute)); ok {
		t.Fatal("leased notification acquired twice")
	}
	if at, _, _ := repo.NextDue(e.ctx); !at.Equal(T0.Add(time.Minute)) {
		t.Errorf("NextDue while leased = %v", at)
	}

	// Reschedule; once acquired again, the old lease cannot complete.
	e.ok(repo.Reschedule(e.ctx, got.Lease(), T0, T0.Add(10*time.Second), "421 try later"))
	again, ok, err := repo.Acquire(e.ctx, T0.Add(10*time.Second), T0.Add(time.Minute))
	e.ok(err)
	if !ok || again.ID != a.ID || again.Attempts != 2 || again.LastError != "421 try later" {
		t.Fatalf("second Acquire = %+v", again)
	}
	e.is(repo.MarkFailed(e.ctx, got.Lease(), T0, "old"), domain.ErrLeaseLost)
	e.ok(repo.MarkSent(e.ctx, again.Lease(), T0.Add(11*time.Second)))
	sent, err := repo.Get(e.ctx, a.ID)
	e.ok(err)
	if sent.Status != domain.DeliveryDelivered || !sent.SentAt.Equal(T0.Add(11*time.Second)) || sent.LastError != "" || !sent.LeaseUntil.IsZero() {
		t.Fatalf("after MarkSent = %+v", sent)
	}

	// b fails permanently.
	gb, ok, err := repo.Acquire(e.ctx, T0.Add(time.Hour), T0.Add(time.Hour+time.Minute))
	e.ok(err)
	if !ok || gb.ID != b.ID {
		t.Fatalf("Acquire b = %+v %v", gb, ok)
	}
	e.ok(repo.MarkFailed(e.ctx, gb.Lease(), T0.Add(time.Hour), "550 rejected"))
	if fb, _ := repo.Get(e.ctx, b.ID); fb.Status != domain.DeliveryFailed || fb.LastError != "550 rejected" {
		t.Fatalf("after MarkFailed = %+v", fb)
	}
	if _, ok, _ := repo.NextDue(e.ctx); ok {
		t.Error("NextDue with nothing pending")
	}

	_, err = repo.Get(e.ctx, e.id())
	e.is(err, domain.ErrNotFound)

	n, err := repo.Purge(e.ctx, T0.Add(30*time.Minute))
	e.ok(err)
	if n != 1 {
		t.Errorf("Purge = %d, want 1 (a; b is newer)", n)
	}
	if n, _ := repo.Purge(e.ctx, T0.Add(2*time.Hour)); n != 1 {
		t.Errorf("second Purge = %d", n)
	}
}
