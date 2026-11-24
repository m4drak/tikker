// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/domain"
)

func TestBlockedAddr(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true}, {"127.1.2.3", true}, {"10.0.0.1", true}, {"172.16.5.5", true}, {"172.31.255.255", true},
		{"192.168.1.1", true}, {"169.254.169.254", true}, {"169.254.0.1", true}, {"100.64.0.1", true},
		{"100.127.255.255", true}, {"192.0.0.8", true}, {"198.18.0.1", true}, {"198.19.255.255", true},
		{"0.0.0.0", true}, {"0.1.2.3", true}, {"224.0.0.1", true}, {"255.255.255.255", true},
		{"::1", true}, {"::", true}, {"fe80::1", true}, {"fd00::1", true}, {"ff02::1", true},
		{"::ffff:127.0.0.1", true}, {"::ffff:169.254.169.254", true}, {"::ffff:10.0.0.1", true},
		{"8.8.8.8", false}, {"93.184.216.34", false}, {"172.32.0.1", false}, {"100.128.0.1", false},
		{"198.20.0.1", false}, {"2606:4700:4700::1111", false}, {"::ffff:8.8.8.8", false},
		{"64:ff9b::7f00:1", true}, {"64:ff9b::808:808", true}, {"64:ff9b:1::1", true},
		{"2002:7f00:1::1", true}, {"2002:808:808::1", true}, {"2001:0:4136:e378:8000:63bf:3fff:fdd2", true},
		{"::7f00:1", true}, {"::808:808", true},
		{"2001:db9::1", false}, {"2003::1", false}, {"64:ff9b:2::1", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			if got := BlockedAddr(netip.MustParseAddr(tt.ip)); got != tt.blocked {
				t.Errorf("BlockedAddr(%s) = %v, want %v", tt.ip, got, tt.blocked)
			}
		})
	}
}

// dialLog records the addresses the sender actually connects to and routes
// them to a test server.
type dialLog struct {
	mu     sync.Mutex
	addrs  []string
	target string // where connections really go
}

func (d *dialLog) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, d.target)
}

func (d *dialLog) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

func resolver(m map[string][]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, s := range m[host] {
			out = append(out, netip.MustParseAddr(s))
		}
		if out == nil {
			return nil, errors.New("no such host")
		}
		return out, nil
	}
}

func hostPort(t *testing.T, raw string) (host, port string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname(), u.Port()
}

func TestSSRFGuard(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	_, port := hostPort(t, srv.URL)
	none := func(string) bool { return false }

	tests := []struct {
		name      string
		url       string
		resolve   map[string][]string
		allow     func(string) bool
		wantSent  bool
		wantDials []string
	}{
		{"loopback literal", "http://127.0.0.1:" + port + "/h", nil, none, false, nil},
		{"loopback literal allowlisted", "http://127.0.0.1:" + port + "/h", nil, func(h string) bool { return h == "127.0.0.1" }, true, []string{"127.0.0.1:" + port}},
		{"metadata literal", "http://169.254.169.254/latest/meta-data", nil, none, false, nil},
		{"ipv6 loopback", "http://[::1]:" + port + "/h", nil, none, false, nil},
		{"name to loopback", "https://evil.test:" + port + "/h", map[string][]string{"evil.test": {"127.0.0.1"}}, none, false, nil},
		{"name to metadata", "https://evil.test/h", map[string][]string{"evil.test": {"169.254.169.254"}}, none, false, nil},
		{"name to cgnat", "https://evil.test/h", map[string][]string{"evil.test": {"100.64.1.1"}}, none, false, nil},
		{"name to ipv4-mapped", "https://evil.test/h", map[string][]string{"evil.test": {"::ffff:10.0.0.5"}}, none, false, nil},
		{"mixed answer uses the public address only", "http://mixed.test:" + port + "/h",
			map[string][]string{"mixed.test": {"10.0.0.1", "93.184.216.34"}}, none, true, []string{"93.184.216.34:" + port}},
		{"public name", "http://public.test:" + port + "/h",
			map[string][]string{"public.test": {"93.184.216.34"}}, none, true, []string{"93.184.216.34:" + port}},
		{"internal name allowlisted by name", "http://n8n.lan:" + port + "/h",
			map[string][]string{"n8n.lan": {"192.168.1.5"}}, func(h string) bool { return h == "n8n.lan" }, true, []string{"192.168.1.5:" + port}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			before := len(rec.requests())
			dl := &dialLog{target: srv.Listener.Addr().String()}
			w := e.hook("h", tt.url, domain.EventIncomingSMS)
			s := e.sender(SenderOptions{AllowPrivate: tt.allow, Resolve: resolver(tt.resolve), Dial: dl.dial})
			e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())
			stepAt(t, s, e, 0)

			d := e.deliveries(w)[0]
			sent := len(rec.requests()) - before
			if tt.wantSent {
				if sent != 1 || d.Status != domain.DeliveryDelivered {
					t.Fatalf("sent=%d delivery=%+v", sent, d)
				}
			} else if sent != 0 || len(dl.dialed()) != 0 || d.Status != domain.DeliveryFailed ||
				!strings.Contains(d.LastError, "not allowed") {
				t.Fatalf("destination not refused: sent=%d dials=%v delivery=%+v", sent, dl.dialed(), d)
			}
			if got := dl.dialed(); strings.Join(got, ",") != strings.Join(tt.wantDials, ",") {
				t.Errorf("dialed %v, want %v", got, tt.wantDials)
			}
		})
	}
}

func TestSenderIgnoresProxyEnvironment(t *testing.T) {
	proxyHits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyHits++ }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	_, port := hostPort(t, srv.URL)
	e := newEnv(t)
	dl := &dialLog{target: srv.Listener.Addr().String()}
	w := e.hook("h", "http://public.test:"+port+"/h", domain.EventIncomingSMS)
	s := e.sender(SenderOptions{Resolve: resolver(map[string][]string{"public.test": {"93.184.216.34"}}), Dial: dl.dial})
	if tr, ok := s.opts.Client.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("the default transport may use a proxy")
	}
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())
	stepAt(t, s, e, 0)
	if proxyHits != 0 || len(rec.requests()) != 1 || e.deliveries(w)[0].Status != domain.DeliveryDelivered {
		t.Fatalf("proxy hits=%d, receiver requests=%d", proxyHits, len(rec.requests()))
	}
}

// The connection goes to an approved IP, but the certificate is still checked
// against the host name of the URL.
func TestTLSVerifiesTheHostName(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewTLSServer(rec)
	defer srv.Close()
	_, port := hostPort(t, srv.URL)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	resolve := resolver(map[string][]string{"example.com": {"93.184.216.34"}, "wrong.test": {"93.184.216.35"}})

	tests := []struct {
		name, host string
		wantSent   bool
	}{
		{"name in the certificate", "example.com", true},
		{"name not in the certificate", "wrong.test", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			before := len(rec.requests())
			dl := &dialLog{target: srv.Listener.Addr().String()}
			w := e.hook("h", "https://"+tt.host+":"+port+"/h", domain.EventIncomingSMS)
			s := e.sender(SenderOptions{
				Resolve: resolve, Dial: dl.dial,
				TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			})
			e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())
			stepAt(t, s, e, 0)
			d := e.deliveries(w)[0]
			if sent := len(rec.requests()) - before; tt.wantSent != (sent == 1) {
				t.Fatalf("sent=%d delivery=%+v", sent, d)
			}
			if !tt.wantSent && (d.Status != domain.DeliveryPending || !strings.Contains(d.LastError, "certificate")) {
				t.Fatalf("delivery = %+v, want a retried certificate error", d)
			}
			if got := dl.dialed(); len(got) != 1 || !strings.HasPrefix(got[0], "93.184.216.3") {
				t.Errorf("dialed %v, want the approved IP", got)
			}
		})
	}
}

func TestDeliveryIDIsSigned(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	w := e.hook("h", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{})
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())
	stepAt(t, s, e, 0)
	r := rec.requests()[0]
	secret := []byte(testSecret)
	header := func(id, sig string) http.Header {
		h := http.Header{}
		h.Set(HeaderDelivery, id)
		h.Set(HeaderEvent, r.Event)
		h.Set(HeaderSignature, sig)
		return h
	}

	env, err := VerifyRequest(secret, r.Body, header(r.Delivery, r.Sign))
	if err != nil || env.ID != r.Delivery || env.ID != e.deliveries(w)[0].ID.String() {
		t.Fatalf("valid request rejected: %+v %v", env, err)
	}
	// A replay under another delivery ID (to defeat deduplication).
	if _, err := VerifyRequest(secret, r.Body, header("01JOTHER0000000000000000AA", r.Sign)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("changed header accepted: %v", err)
	}
	// Changing the ID inside the body breaks the signature.
	forged := []byte(strings.Replace(string(r.Body), r.Delivery, "01JOTHER0000000000000000AA", 1))
	if _, err := VerifyRequest(secret, forged, header("01JOTHER0000000000000000AA", r.Sign)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("changed body ID accepted: %v", err)
	}
	if _, err := VerifyRequest([]byte("another-secret-value"), r.Body, header(r.Delivery, r.Sign)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong secret accepted: %v", err)
	}
	if _, err := VerifyRequest(secret, r.Body, header(r.Delivery, "")); !errors.Is(err, ErrBadSignature) {
		t.Errorf("missing signature accepted: %v", err)
	}
	// The event header must match the signed event.
	eventTests := []struct {
		name  string
		event func(h http.Header)
		ok    bool
	}{
		{"matching", func(http.Header) {}, true},
		{"different", func(h http.Header) { h.Set(HeaderEvent, "message_status") }, false},
		{"missing", func(h http.Header) { h.Del(HeaderEvent) }, false},
		{"empty", func(h http.Header) { h.Set(HeaderEvent, "") }, false},
	}
	for _, tt := range eventTests {
		t.Run("event header "+tt.name, func(t *testing.T) {
			h := header(r.Delivery, r.Sign)
			tt.event(h)
			_, err := VerifyRequest(secret, r.Body, h)
			if tt.ok && err != nil {
				t.Errorf("rejected: %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrBadSignature) {
				t.Errorf("accepted or wrong error: %v", err)
			}
		})
	}
}

// Two deliveries of one event to two webhooks have different signed IDs.
func TestEachDeliveryHasItsOwnSignedID(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	e.hook("a", srv.URL, domain.EventIncomingSMS)
	e.hook("b", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{})
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())
	stepAt(t, s, e, 0)
	stepAt(t, s, e, 0)
	reqs := rec.requests()
	if len(reqs) != 2 || reqs[0].Delivery == reqs[1].Delivery {
		t.Fatalf("requests = %+v", reqs)
	}
	for _, r := range reqs {
		if _, err := VerifyRequest([]byte(testSecret), r.Body, http.Header{HeaderDelivery: {r.Delivery}, HeaderEvent: {r.Event}, HeaderSignature: {r.Sign}}); err != nil {
			t.Errorf("verify: %v", err)
		}
	}
}

// A worker that was paused between acquiring and sending must not POST after
// its lease ran out: another worker may own the delivery by then.
func TestNoRequestAfterLeaseExpiry(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	w := e.hook("h", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{Timeout: time.Second})
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())

	paused, ok, err := e.st.Webhooks().AcquireDelivery(e.ctx, e.clk.Now(), e.clk.Now().Add(s.lease))
	if err != nil || !ok {
		t.Fatalf("acquire = %v %v", ok, err)
	}
	e.clk.Advance(s.lease + time.Second) // the worker sleeps through the expiry
	// Meanwhile another worker reclaims and delivers.
	if !s.step(e.ctx) || len(rec.requests()) != 1 {
		t.Fatalf("reclaim failed, requests = %d", len(rec.requests()))
	}
	s.attempt(e.ctx, paused) // the paused worker wakes up
	if n := len(rec.requests()); n != 1 {
		t.Fatalf("%d requests: the paused worker sent after its lease expired", n)
	}
	if d := e.deliveries(w)[0]; d.Status != domain.DeliveryDelivered || d.Attempts != 2 {
		t.Fatalf("delivery = %+v", d)
	}
}

// The HTTP deadline never extends past the lease.
func TestRequestDeadlineIsCappedByLease(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice a closed connection
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	w := e.hook("h", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{Timeout: time.Minute})
	e.publish(NewPublisher(e.clk, e.ids, s), domain.EventIncomingSMS, e.message())
	job, ok, err := e.st.Webhooks().AcquireDelivery(e.ctx, e.clk.Now(), e.clk.Now().Add(2*time.Second)) // 2 s left of a minute
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	begin := time.Now()
	s.attempt(e.ctx, job)
	<-started
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("attempt took %s with 2 s of lease left", took)
	}
	if d := e.deliveries(w)[0]; d.Status != domain.DeliveryPending || d.LastError == "" {
		t.Fatalf("delivery = %+v", d)
	}
}

func TestQueuedDeliveriesDoNotFollowAChangedURL(t *testing.T) {
	e := newEnv(t)
	oldRec, newRec := &recorder{}, &recorder{}
	oldSrv, newSrv := httptest.NewServer(oldRec), httptest.NewServer(newRec)
	defer oldSrv.Close()
	defer newSrv.Close()
	w := e.hook("h", oldSrv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{})
	pub := NewPublisher(e.clk, e.ids, s)
	e.publish(pub, domain.EventIncomingSMS, e.message())

	w.URL, w.SecretEnc = newSrv.URL, nil
	if err := e.st.Webhooks().Update(e.ctx, w); err != nil {
		t.Fatal(err)
	}
	if stepAt(t, s, e, 0) || len(oldRec.requests())+len(newRec.requests()) != 0 {
		t.Fatal("a queued event was sent after the URL changed")
	}
	e.publish(pub, domain.EventIncomingSMS, e.message())
	stepAt(t, s, e, 0)
	if len(newRec.requests()) != 1 || len(oldRec.requests()) != 0 {
		t.Fatalf("old=%d new=%d", len(oldRec.requests()), len(newRec.requests()))
	}
}

func TestPayloadRetentionDropsUndeliveredEvents(t *testing.T) {
	e := newEnv(t)
	w := e.hook("h", "https://unused.example.com", domain.EventIncomingSMS)
	d := domain.WebhookDelivery{
		ID: e.ids.NewID(), WebhookID: w.ID, Event: domain.EventIncomingSMS, Payload: []byte(`{"secret":"sms text"}`),
		CreatedAt: e.clk.Now(), NextAttemptAt: e.clk.Now().Add(1000 * time.Hour), // never due in this test
	}
	if err := e.st.Webhooks().AddDeliveries(e.ctx, []domain.WebhookDelivery{d}); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(48 * time.Hour)
	s := e.sender(SenderOptions{PayloadRetention: 24 * time.Hour})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, _ := e.st.Webhooks().GetDelivery(e.ctx, d.ID)
		if got.Status == domain.DeliveryFailed {
			if len(got.Payload) != 0 || !strings.Contains(got.LastError, "retention") {
				t.Fatalf("delivery = %+v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("payload not dropped: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Shutdown lets an attempt in flight finish when the budget allows it ...
func TestShutdownWaitsForTheAttemptInFlight(t *testing.T) {
	e := newEnv(t)
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	w := e.hook("h", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{Workers: 1})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	pub := NewPublisher(e.clk, e.ids, s)
	e.publish(pub, domain.EventIncomingSMS, e.message())
	pub.Wake()
	<-entered

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		done <- s.Shutdown(ctx)
	}()
	time.Sleep(50 * time.Millisecond) // Shutdown is waiting, not cancelling
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if d := e.deliveries(w)[0]; d.Status != domain.DeliveryDelivered {
		t.Fatalf("delivery = %+v", d)
	}
}

// ... and cancels it when the shared deadline passes, recording that the
// receiver may have got the request, with a retry at once.
func TestShutdownDuringTheFinalAttempt(t *testing.T) {
	e := newEnv(t)
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer srv.Close()
	w := e.hook("h", srv.URL, domain.EventIncomingSMS)
	s := e.sender(SenderOptions{Workers: 1})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	pub := NewPublisher(e.clk, e.ids, s)
	e.publish(pub, domain.EventIncomingSMS, e.message())
	pub.Wake()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err := s.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want the deadline error", err)
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("Shutdown took %s", took)
	}
	d := e.deliveries(w)[0]
	if d.Status != domain.DeliveryPending || !strings.Contains(d.LastError, "may have received") || !d.NextAttemptAt.Equal(e.clk.Now()) {
		t.Fatalf("delivery = %+v", d)
	}
	if err := s.Shutdown(context.Background()); err != nil { // idempotent
		t.Fatalf("second Shutdown = %v", err)
	}
}
