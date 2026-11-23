// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package webhook

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/secrets"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

const testSecret = "whsec_0123456789abcdef"

type env struct {
	t    *testing.T
	ctx  context.Context
	clk  *clock.Fake
	st   *sqlite.Store
	ring *secrets.Keyring
	ids  domain.IDGenerator
	log  *slog.Logger
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	st, err := sqlite.Open(context.Background(), sqlite.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ring, err := secrets.NewKeyring(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		t: t, ctx: context.Background(), clk: clk, st: st, ring: ring, ids: domain.NewULIDGenerator(clk),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// hook stores an enabled webhook for url that listens to evs.
func (e *env) hook(name, url string, evs ...domain.WebhookEvent) domain.Webhook {
	e.t.Helper()
	e.clk.Advance(time.Millisecond)
	id := e.ids.NewID()
	enc, err := e.ring.Seal(SecretPurpose, id.String(), []byte(testSecret))
	if err != nil {
		e.t.Fatal(err)
	}
	w := domain.Webhook{ID: id, Name: name, URL: url, SecretEnc: enc, Events: evs, Enabled: true, CreatedAt: e.clk.Now()}
	if err := e.st.Webhooks().Create(e.ctx, w); err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) message() domain.Message {
	e.clk.Advance(time.Millisecond)
	return domain.Message{
		ID: e.ids.NewID(), Direction: domain.DirectionIn, ChannelID: e.ids.NewID(), Peer: "+48123456789",
		Body: "hello", Encoding: domain.EncodingGSM7, Segments: 1, Status: domain.StatusReceived,
		CreatedAt: e.clk.Now(), ReceivedAt: e.clk.Now(),
	}
}

// publish stores one event in its own transaction.
func (e *env) publish(p *Publisher, ev domain.WebhookEvent, m domain.Message) {
	e.t.Helper()
	err := e.st.WithTx(e.ctx, func(tx store.Repos) error {
		if ev == domain.EventIncomingSMS {
			return p.PublishIncoming(e.ctx, tx, m)
		}
		return p.PublishStatus(e.ctx, tx, m)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) sender(opts SenderOptions) *Sender {
	e.t.Helper()
	opts.Store, opts.Secrets, opts.Clock, opts.Logger = e.st.Webhooks(), e.ring, e.clk, e.log
	if opts.AllowPrivate == nil { // the receivers are httptest servers on loopback
		opts.AllowPrivate = func(h string) bool { return h == "127.0.0.1" }
	}
	if opts.Rand == nil {
		opts.Rand = func() float64 { return 0.5 } // no jitter
	}
	s, err := NewSender(opts)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) deliveries(w domain.Webhook) []domain.WebhookDelivery {
	e.t.Helper()
	ds, err := e.st.Webhooks().ListDeliveries(e.ctx, w.ID, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	return ds
}

// recorder is a receiver that answers with a scripted status per request.
type recorder struct {
	mu      sync.Mutex
	codes   []int // status per request; the last one repeats
	reqs    []recorded
	handler func(n int) int
}

type recorded struct {
	Body                  []byte
	Event, Delivery, Sign string
	ContentType, UA       string
}
