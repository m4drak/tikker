// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	n := len(r.reqs)
	r.reqs = append(r.reqs, recorded{
		Body: body, Event: req.Header.Get(HeaderEvent), Delivery: req.Header.Get(HeaderDelivery),
		Sign: req.Header.Get(HeaderSignature), ContentType: req.Header.Get("Content-Type"), UA: req.UserAgent(),
	})
	code := 200
	switch {
	case r.handler != nil:
		code = r.handler(n)
	case len(r.codes) > 0:
		code = r.codes[min(n, len(r.codes)-1)]
	}
	r.mu.Unlock()
	if code/100 == 3 {
		w.Header().Set("Location", "/elsewhere")
	}
	w.WriteHeader(code)
}

func (r *recorder) requests() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

// stepAt advances the fake clock by d and attempts one delivery.
func stepAt(t *testing.T, s *Sender, e *env, d time.Duration) bool {
	t.Helper()
	e.clk.Advance(d)
	return s.step(e.ctx)
}

func TestDeliverySuccessAndSignature(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	w := e.hook("crm", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{UserAgent: "tikker-test"})
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())

	if !stepAt(t, s, e, 0) {
		t.Fatal("nothing delivered")
	}
	if stepAt(t, s, e, 0) {
		t.Fatal("a delivered event was sent again")
	}
	reqs := rec.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	ds := e.deliveries(w)
	if len(ds) != 1 || ds[0].Status != domain.DeliveryDelivered || ds[0].Attempts != 1 || ds[0].LastHTTPCode != 200 {
		t.Fatalf("delivery = %+v", ds)
	}
	if r.Delivery != ds[0].ID.String() || r.Event != "incoming_sms" || r.ContentType != "application/json" || r.UA != "tikker-test" {
		t.Errorf("headers = %+v", r)
	}
	if !Verify([]byte(testSecret), r.Body, r.Sign) {
		t.Errorf("signature %q does not verify", r.Sign)
	}
	if Verify([]byte("wrong-secret-xxxxxxx"), r.Body, r.Sign) || Verify([]byte(testSecret), append(r.Body, ' '), r.Sign) {
		t.Error("tampered body or wrong secret verified")
	}
	var env Envelope
	if err := json.Unmarshal(r.Body, &env); err != nil {
		t.Fatal(err)
	}
	if env.ID != ds[0].ID.String() || env.Event != domain.EventIncomingSMS || env.Message.Body == nil || *env.Message.Body != "hello" ||
		env.Message.Peer != "+48123456789" || env.Message.Status != "received" {
		t.Errorf("payload = %s", r.Body)
	}
}

func TestRetryThenSuccessKeepsDeliveryID(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{codes: []int{503, 500, 200}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	w := e.hook("crm", srv.URL, domain.EventMessageStatus)
	s := e.sender(SenderOptions{}) // nominal delays 10 s, 40 s (jitter 0.5 => factor 1)
	m := e.message()
	m.Direction, m.Status = domain.DirectionOut, domain.StatusSent
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventMessageStatus, m)

	if !stepAt(t, s, e, 0) {
		t.Fatal("first attempt not made")
	}
	d := e.deliveries(w)[0]
	if d.Status != domain.DeliveryPending || d.Attempts != 1 || d.LastHTTPCode != 503 || d.LastError == "" {
		t.Fatalf("after 503: %+v", d)
	}
	if want := e.clk.Now().Add(10 * time.Second); !d.NextAttemptAt.Equal(want) {
		t.Fatalf("next attempt %v, want %v", d.NextAttemptAt, want)
	}
	if stepAt(t, s, e, 9*time.Second) {
		t.Fatal("retried before the backoff elapsed")
	}
	if !stepAt(t, s, e, time.Second) { // 500
		t.Fatal("second attempt not made")
	}
	if d = e.deliveries(w)[0]; d.Attempts != 2 || !d.NextAttemptAt.Equal(e.clk.Now().Add(40*time.Second)) {
		t.Fatalf("after 500: %+v", d)
	}
	if !stepAt(t, s, e, 40*time.Second) { // 200
		t.Fatal("third attempt not made")
	}
	d = e.deliveries(w)[0]
	if d.Status != domain.DeliveryDelivered || d.Attempts != 3 || d.LastError != "" {
		t.Fatalf("final: %+v", d)
	}
	reqs := rec.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests", len(reqs))
	}
	for _, r := range reqs { // idempotency: same id, same bytes, same signature
		if r.Delivery != d.ID.String() || string(r.Body) != string(reqs[0].Body) || r.Sign != reqs[0].Sign {
			t.Errorf("retry differs from the first request: %+v", r)
		}
		if r.Event != "message_status" {
			t.Errorf("event = %q", r.Event)
		}
	}
}

func TestPermanentAndExhausted(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		attempts int
		errPart  string
	}{
		{"400 is permanent", 400, 1, "unexpected HTTP status 400"},
		{"404 is permanent", 404, 1, "404"},
		{"redirect is permanent", 302, 1, "302"},
		{"401 is permanent", 401, 1, "401"},
		{"500 until exhausted", 500, 3, "retries exhausted"},
		{"429 is retried", 429, 3, "retries exhausted"},
		{"408 is retried", 408, 3, "retries exhausted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			rec := &recorder{codes: []int{tc.code}}
			srv := httptest.NewServer(rec)
			defer srv.Close()
			w := e.hook("crm", srv.URL, domain.EventIncomingSMS)
			retry := DefaultRetryPolicy()
			retry.MaxAttempts = 3
			s := e.sender(SenderOptions{Retry: retry})
			e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())

			for range 10 {
				if !stepAt(t, s, e, time.Hour) {
					break
				}
			}
			d := e.deliveries(w)[0]
			if d.Status != domain.DeliveryFailed || d.Attempts != tc.attempts || d.LastHTTPCode != tc.code {
				t.Fatalf("delivery = %+v", d)
			}
			if !strings.Contains(d.LastError, tc.errPart) {
				t.Errorf("LastError = %q, want %q", d.LastError, tc.errPart)
			}
			if n := len(rec.requests()); n != tc.attempts {
				t.Errorf("%d requests, want %d", n, tc.attempts)
			}
		})
	}
}

func TestTimeoutAndConnectionErrorAreRetried(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused

	for name, url := range map[string]string{"timeout": slow.URL, "refused": deadURL} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			w := e.hook("crm", url+"/hook/s3cr3t-token", domain.EventIncomingSMS)
			s := e.sender(SenderOptions{Timeout: 100 * time.Millisecond})
			e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())
			if !stepAt(t, s, e, 0) {
				t.Fatal("no attempt")
			}
			d := e.deliveries(w)[0]
			if d.Status != domain.DeliveryPending || d.Attempts != 1 || d.LastHTTPCode != 0 || d.LastError == "" {
				t.Fatalf("delivery = %+v", d)
			}
			if strings.Contains(d.LastError, "s3cr3t") {
				t.Errorf("error leaks the URL path: %q", d.LastError)
			}
		})
	}
}

func TestExpiredLeaseIsRetriedAndFenced(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	w := e.hook("crm", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{Timeout: time.Second})
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())

	// A crashed worker acquired the delivery and never reported.
	lost, ok, err := e.st.Webhooks().AcquireDelivery(e.ctx, e.clk.Now(), e.clk.Now().Add(s.lease))
	if err != nil || !ok {
		t.Fatalf("acquire = %v %v", ok, err)
	}
	if stepAt(t, s, e, time.Second) {
		t.Fatal("delivery taken while the lease was live")
	}
	if !stepAt(t, s, e, s.lease) {
		t.Fatal("delivery not retried after the lease expired")
	}
	if d := e.deliveries(w)[0]; d.Status != domain.DeliveryDelivered || d.Attempts != 2 {
		t.Fatalf("delivery = %+v", d)
	}
	// The zombie's late result is rejected and cannot overwrite the outcome.
	err = e.st.Webhooks().MarkDeliveryFailed(e.ctx, lost.Delivery.Lease(), e.clk.Now(), "late", 500)
	if !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("late result = %v, want ErrLeaseLost", err)
	}
}

func TestPoisonDeliveryAfterRecordedCrashes(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	w := e.hook("crm", srv.URL, domain.EventIncomingSMS)
	retry := DefaultRetryPolicy()
	retry.MaxAttempts = 2
	s := e.sender(SenderOptions{Retry: retry, Timeout: time.Second})
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())
	for range 2 { // two crashed attempts
		if _, ok, _ := e.st.Webhooks().AcquireDelivery(e.ctx, e.clk.Now(), e.clk.Now().Add(s.lease)); !ok {
			t.Fatal("not acquired")
		}
		e.clk.Advance(s.lease)
	}
	if !stepAt(t, s, e, 0) {
		t.Fatal("poison delivery not picked up")
	}
	d := e.deliveries(w)[0]
	if d.Status != domain.DeliveryFailed || len(rec.requests()) != 0 {
		t.Fatalf("delivery = %+v, requests = %d", d, len(rec.requests()))
	}
}

func TestSecretCannotBeDecryptedIsRetried(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(&recorder{})
	defer srv.Close()
	w := e.hook("crm", srv.URL, domain.EventIncomingSMS)
	w.SecretEnc = []byte("garbage")
	if err := e.st.Webhooks().Update(e.ctx, w); err != nil {
		t.Fatal(err)
	}
	s := e.sender(SenderOptions{})
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())
	stepAt(t, s, e, 0)
	if d := e.deliveries(w)[0]; d.Status != domain.DeliveryPending || !strings.Contains(d.LastError, "decrypt") {
		t.Fatalf("delivery = %+v", d)
	}
}

// Many workers, many events: each delivery reaches the receiver exactly once.
func TestConcurrentWorkersNoDuplicate(t *testing.T) {
	e := newEnv(t)
	var hits sync.Map
	var total atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderDelivery)
		if n, _ := hits.LoadOrStore(id, new(atomic.Int64)); n.(*atomic.Int64).Add(1) > 1 {
			t.Errorf("delivery %s received twice", id)
		}
		total.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	hook := e.hook("crm", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{Workers: 8})
	pub := NewPublisher(e.clk, e.ids, s)
	const events = 60
	for range events {
		e.publish(pub, domain.EventIncomingSMS, e.message())
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s.step(e.ctx) {
			}
		}()
	}
	wg.Wait()
	if total.Load() != events {
		t.Fatalf("%d requests, want %d", total.Load(), events)
	}
	for _, d := range e.deliveries(hook) {
		if d.Status != domain.DeliveryDelivered || d.Attempts != 1 {
			t.Fatalf("delivery = %+v", d)
		}
	}
}

// The running sender is woken by Publisher.Wake and delivers without polling.
func TestStartWakeStop(t *testing.T) {
	e := newEnv(t)
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get(HeaderDelivery)
	}))
	defer srv.Close()
	e.hook("crm", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{Workers: 2})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if err := s.Start(context.Background()); err == nil {
		t.Error("second Start accepted")
	}
	pub := NewPublisher(e.clk, e.ids, s)
	e.publish(pub, domain.EventIncomingSMS, e.message())
	pub.Wake()
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("event not delivered after Wake")
	}
}

func TestDisabledAndUnsubscribedWebhooksGetNothing(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(&recorder{})
	defer srv.Close()
	statusOnly := e.hook("status", srv.URL, domain.EventMessageStatus)
	off := e.hook("off", srv.URL, domain.EventIncomingSMS)
	off.Enabled = false
	if err := e.st.Webhooks().Update(e.ctx, off); err != nil {
		t.Fatal(err)
	}
	e.publish(NewPublisher(e.clk, e.ids, nil), domain.EventIncomingSMS, e.message())
	if n := len(e.deliveries(statusOnly)) + len(e.deliveries(off)); n != 0 {
		t.Fatalf("%d deliveries created", n)
	}
}

func TestPublishRollsBackWithTransaction(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(&recorder{})
	defer srv.Close()
	w := e.hook("crm", srv.URL, domain.EventIncomingSMS)
	pub := NewPublisher(e.clk, e.ids, nil)
	errBoom := context.Canceled
	err := e.st.WithTx(e.ctx, func(tx store.Repos) error {
		if err := pub.PublishIncoming(e.ctx, tx, e.message()); err != nil {
			return err
		}
		return errBoom
	})
	if err != errBoom {
		t.Fatalf("err = %v", err)
	}
	if n := len(e.deliveries(w)); n != 0 {
		t.Fatalf("%d deliveries survived the rollback", n)
	}
}

func TestStatusPayloadHasNoBody(t *testing.T) {
	m := domain.Message{
		ID: "01JZZZZZZZZZZZZZZZZZZZZZZZ", Direction: domain.DirectionOut, Peer: "+48123456789", Body: "secret text",
		Status: domain.StatusFailed, Attempts: 3, LastError: "boom", PossibleDuplicate: true, Segments: 2, CreatedAt: t0,
	}
	b, err := BuildPayload("01JDELIVERY00000000000000A", domain.EventMessageStatus, m, t0)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	msg := got["message"].(map[string]any)
	if _, has := msg["body"]; has || strings.Contains(string(b), "secret text") {
		t.Errorf("status payload carries the text: %s", b)
	}
	if got["event"] != "message_status" || msg["status"] != "failed" || msg["last_error"] != "boom" ||
		msg["possible_duplicate"] != true || msg["sent_at"] != nil || got["occurred_at"] != "2026-11-19T12:00:00Z" {
		t.Errorf("payload = %s", b)
	}
}
