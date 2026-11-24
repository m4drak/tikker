// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package mailnotify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var t0 = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

// sink is a tiny SMTP server that records messages. dataReplies are the
// answers to successive DATA terminators (then 250).
type sink struct {
	t           *testing.T
	ln          net.Listener
	starttls    *tls.Config // offered when set
	user, pass  string      // AUTH PLAIN expected when set
	dataReplies []string

	mu       sync.Mutex
	messages []string
	auths    []string
	wg       sync.WaitGroup
}

func newSink(t *testing.T, implicit *tls.Config) *sink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if implicit != nil {
		ln = tls.NewListener(ln, implicit)
	}
	s := &sink{t: t, ln: ln}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(c)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *sink) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *sink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...)
}

func (s *sink) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	say := func(l string) { _, _ = io.WriteString(c, l+"\r\n") }
	say("220 sink ESMTP")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			lines := []string{"250-sink"}
			if s.starttls != nil {
				if _, ok := c.(*tls.Conn); !ok {
					lines = append(lines, "250-STARTTLS")
				}
			}
			if s.user != "" {
				lines = append(lines, "250-AUTH PLAIN")
			}
			for _, l := range append(lines, "250 8BITMIME") {
				say(l)
			}
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, s.starttls)
			if tc.Handshake() != nil {
				return
			}
			c, br = tc, bufio.NewReader(tc)
		case "AUTH":
			_, resp, _ := strings.Cut(arg, " ")
			raw, _ := base64.StdEncoding.DecodeString(resp)
			s.mu.Lock()
			s.auths = append(s.auths, string(raw))
			s.mu.Unlock()
			if string(raw) == "\x00"+s.user+"\x00"+s.pass {
				say("235 ok")
			} else {
				say("535 bad credentials")
			}
		case "MAIL", "RCPT", "RSET", "NOOP":
			say("250 ok")
		case "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			reply := "250 queued"
			if len(s.dataReplies) > 0 {
				reply, s.dataReplies = s.dataReplies[0], s.dataReplies[1:]
			}
			if strings.HasPrefix(reply, "250") {
				s.messages = append(s.messages, b.String())
			}
			s.mu.Unlock()
			say(reply)
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 no")
		}
	}
}

type env struct {
	t    *testing.T
	ctx  context.Context
	st   *sqlite.Store
	clk  *clock.Fake
	pub  *Publisher
	ids  *recordingIDs
	note domain.ID // the last notification
}

// recordingIDs remembers the last ID it handed out.
type recordingIDs struct {
	mu   sync.Mutex
	gen  domain.IDGenerator
	last domain.ID
}

func (r *recordingIDs) NewID() domain.ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = r.gen.NewID()
	return r.last
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	st, err := sqlite.Open(context.Background(), sqlite.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ids := &recordingIDs{gen: domain.NewULIDGenerator(clk)}
	return &env{t: t, ctx: context.Background(), st: st, clk: clk, ids: ids, pub: NewPublisher(clk, ids, nil)}
}

// inbound stores an inbound message with its notification, as the inbox
// does.
func (e *env) inbound(peer, body string) domain.Message {
	e.t.Helper()
	m := domain.Message{
		ID: e.ids.NewID(), Direction: domain.DirectionIn, Peer: peer, Body: body,
		Encoding: domain.EncodingUCS2, Segments: 1, Status: domain.StatusReceived,
		CreatedAt: e.clk.Now(), ReceivedAt: e.clk.Now(), CreatedBy: domain.SystemActor,
	}
	err := e.st.WithTx(e.ctx, func(tx store.Repos) error {
		if _, err := tx.Messages().InsertInbound(e.ctx, m); err != nil {
			return err
		}
		return e.pub.PublishIncoming(e.ctx, tx, m)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.note = e.ids.last // the publisher's ID came last
	return m
}

func (e *env) sender(r Relay, mod ...func(*Options)) *Sender {
	e.t.Helper()
	o := Options{
		Store: e.st, Relay: r, Clock: e.clk, Logger: slog.New(slog.DiscardHandler), Timeout: 5 * time.Second,
		Retry:  domain.RetryPolicy{MaxAttempts: 3, Initial: time.Minute, Multiplier: 2, Max: time.Hour},
		Rand:   func() float64 { return 0.5 },
		MaxAge: 72 * time.Hour, Retention: 24 * time.Hour,
	}
	for _, f := range mod {
		f(&o)
	}
	s, err := NewSender(o)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) notification() domain.MailNotification {
	e.t.Helper()
	n, err := e.st.MailOutbox().Get(e.ctx, e.note)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func relay(port int) Relay {
	return Relay{Host: "127.0.0.1", Port: port, TLSMode: TLSNone, From: "tikker <sms@example.net>", To: []string{"owner@example.net", "b@example.net"}}
}

// TestRetryThenDeliver: the relay refuses the first attempt with 451; the
// notification is retried after the backoff and the e-mail has the full
// number in the body and a masked one in the subject.
func TestRetryThenDeliver(t *testing.T) {
	e := newEnv(t)
	sk := newSink(t, nil)
	sk.user, sk.pass = "tikker", "relay-pw"
	sk.dataReplies = []string{"451 4.3.0 try later"}
	r := relay(sk.port())
	r.Username, r.Password = "tikker", "relay-pw"
	s := e.sender(r)
	m := e.inbound("+48511222333", "Kod: 1234\nZażółć")

	if !s.step(e.ctx) {
		t.Fatal("nothing acquired")
	}
	n := e.notification()
	if n.Status != domain.DeliveryPending || n.Attempts != 1 || !strings.Contains(n.LastError, "451") ||
		!n.NextAttemptAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after the first attempt: %+v", n)
	}
	if s.step(e.ctx) {
		t.Fatal("retried before the backoff")
	}
	e.clk.Advance(time.Minute)
	if !s.step(e.ctx) {
		t.Fatal("not retried after the backoff")
	}
	if n := e.notification(); n.Status != domain.DeliveryDelivered || n.Attempts != 2 || n.SentAt.IsZero() {
		t.Fatalf("after the retry: %+v", n)
	}
	msgs := sk.got()
	if len(msgs) != 1 {
		t.Fatalf("%d e-mails", len(msgs))
	}
	pm, err := mail.ReadMessage(strings.NewReader(msgs[0]))
	if err != nil {
		t.Fatal(err)
	}
	subj, _ := new(mime.WordDecoder).DecodeHeader(pm.Header.Get("Subject"))
	if subj != "SMS from +48*****2333" {
		t.Errorf("subject = %q", subj)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(pm.Body))
	for _, want := range []string{"From: +48511222333", "Kod: 1234\r\nZażółć", "Message: " + m.ID.String(), "Received: 2026-03-04T10:00:00Z"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("body misses %q:\n%s", want, body)
		}
	}
	for k, want := range map[string]string{"From": `"tikker" <sms@example.net>`, "To": "<owner@example.net>, <b@example.net>", "Auto-Submitted": "auto-generated"} {
		if got := pm.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if len(sk.auths) != 2 {
		t.Errorf("auths = %d", len(sk.auths))
	}
}

func TestPermanentFailures(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(e *env, sk *sink, r *Relay)
		want    string
		pending bool
	}{
		{name: "5xx on DATA", setup: func(_ *env, sk *sink, _ *Relay) { sk.dataReplies = []string{"554 5.7.1 spam"} }, want: "554"},
		{name: "bad relay password", setup: func(_ *env, sk *sink, r *Relay) {
			sk.user, sk.pass = "u", "right"
			r.Username, r.Password = "u", "wrong"
		}, want: "535"},
		{name: "no STARTTLS offered", setup: func(_ *env, _ *sink, r *Relay) { r.TLSMode = TLSStartTLS }, want: "does not offer STARTTLS"},
		{name: "no AUTH offered", setup: func(_ *env, _ *sink, r *Relay) { r.Username, r.Password = "u", "p" }, want: "does not offer AUTH"},
		{name: "message text gone", setup: func(e *env, _ *sink, _ *Relay) {
			// Retention (24 h in e.sender) has passed and purged the text.
			e.clk.Advance(25 * time.Hour)
			if _, err := e.st.Messages().PurgeBodies(e.ctx, e.clk.Now().Add(-24*time.Hour)); err != nil {
				e.t.Fatal(err)
			}
		}, want: "retention"},
		{name: "message unknown", setup: func(e *env, _ *sink, _ *Relay) {
			if _, ok, _ := e.st.MailOutbox().Acquire(e.ctx, t0, t0.Add(time.Hour)); !ok { // take the real one out of the way
				e.t.Fatal("no notification")
			}
			e.note = e.ids.NewID()
			if err := e.st.MailOutbox().Add(e.ctx, domain.MailNotification{ID: e.note, MessageID: "01UNKNOWN", CreatedAt: t0}); err != nil {
				e.t.Fatal(err)
			}
		}, want: "no longer exists"},
		{name: "relay down is retried", setup: func(_ *env, _ *sink, r *Relay) { r.Port = 1 }, want: "connect", pending: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			sk := newSink(t, nil)
			r := relay(sk.port())
			e.inbound("+48511222333", "x")
			tt.setup(e, sk, &r)
			s := e.sender(r)
			s.step(e.ctx)
			n := e.notification()
			if !strings.Contains(n.LastError, tt.want) || (n.Status == domain.DeliveryPending) != tt.pending {
				t.Errorf("notification = %+v, want %q pending=%v", n, tt.want, tt.pending)
			}
		})
	}
}

// TestTransientFailuresUntilMaxAge: 4xx answers are retried well past the
// policy's MaxAttempts, at most hourly, until MaxAge.
func TestTransientFailuresUntilMaxAge(t *testing.T) {
	e := newEnv(t)
	sk := newSink(t, nil)
	for range 20 {
		sk.dataReplies = append(sk.dataReplies, "421 busy")
	}
	s := e.sender(relay(sk.port()), func(o *Options) { o.MaxAge = 6 * time.Hour })
	e.inbound("+48511222333", "x")
	attempts := 0
	for range 12 {
		if s.step(e.ctx) {
			attempts++
		}
		if n := e.notification(); n.Status != domain.DeliveryPending {
			break
		}
		if next := e.notification().NextAttemptAt; next.Sub(e.clk.Now()) > time.Hour+time.Minute {
			t.Fatalf("retry delay %s above the 1 h cap", next.Sub(e.clk.Now()))
		}
		e.clk.Advance(time.Hour)
	}
	n := e.notification()
	if n.Status != domain.DeliveryFailed || !strings.Contains(n.LastError, "6h0m0s") || attempts <= 3 {
		t.Errorf("notification = %+v after %d attempts", n, attempts)
	}
	st, err := s.Stats(e.ctx)
	if err != nil || st.Failed != 1 || st.Retried < 3 || st.Pending != 0 {
		t.Errorf("stats = %+v, %v", st, err)
	}
}

// TestEmptySMSForwarded: an SMS with no text (not purged) is forwarded with
// a note.
func TestEmptySMSForwarded(t *testing.T) {
	e := newEnv(t)
	sk := newSink(t, nil)
	s := e.sender(relay(sk.port()))
	e.inbound("+48511222333", "")
	s.step(e.ctx)
	if n := e.notification(); n.Status != domain.DeliveryDelivered {
		t.Fatalf("notification = %+v", n)
	}
	if got := sk.got(); len(got) != 1 || !strings.Contains(got[0], "(empty message)") {
		t.Errorf("mails = %q", got)
	}
}

// TestHourlyCap: over MaxPerHour nothing is dropped; the rest goes out in
// the next window, and the cap is logged once per window.
func TestHourlyCap(t *testing.T) {
	e := newEnv(t)
	sk := newSink(t, nil)
	logs := &bytes.Buffer{}
	var mu sync.Mutex
	s := e.sender(relay(sk.port()), func(o *Options) {
		o.MaxPerHour = 2
		o.Logger = slog.New(slog.NewTextHandler(lockedWriter{&mu, logs}, nil))
	})
	for range 5 {
		e.inbound("+48511222333", "x")
	}
	for range 4 {
		s.step(e.ctx)
	}
	if n := len(sk.got()); n != 2 {
		t.Fatalf("%d e-mails in the first hour, want 2", n)
	}
	if d := s.idleWait(e.ctx); d != time.Hour {
		t.Errorf("idleWait = %s, want the rest of the window", d)
	}
	e.clk.Advance(time.Hour)
	for range 4 {
		s.step(e.ctx)
	}
	e.clk.Advance(time.Hour)
	for range 4 {
		s.step(e.ctx)
	}
	if n := len(sk.got()); n != 5 {
		t.Fatalf("%d e-mails after three windows, want 5", n)
	}
	mu.Lock()
	warns := strings.Count(logs.String(), "notification cap reached")
	mu.Unlock()
	if warns != 2 {
		t.Errorf("%d cap warnings, want one per limited window (2)", warns)
	}
	if st, _ := s.Stats(e.ctx); st.Sent != 5 || st.Deferred != 2 {
		t.Errorf("stats = %+v", st)
	}
	if _, err := NewSender(Options{Store: e.st, Relay: relay(25), MaxPerHour: -1}); err == nil {
		t.Error("negative cap accepted")
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestTLSModes(t *testing.T) {
	srvTLS, cliTLS := testTLS(t)
	tests := []struct {
		name     string
		implicit bool
		mode     string
	}{
		{"starttls", false, TLSStartTLS},
		{"implicit", true, TLSImplicit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			var sk *sink
			if tt.implicit {
				sk = newSink(t, srvTLS)
			} else {
				sk = newSink(t, nil)
				sk.starttls = srvTLS
			}
			sk.user, sk.pass = "u", "p"
			r := relay(sk.port())
			r.TLSMode, r.TLSConfig, r.Username, r.Password = tt.mode, cliTLS, "u", "p"
			e.inbound("+48511222333", "over tls")
			s := e.sender(r)
			s.step(e.ctx)
			if n := e.notification(); n.Status != domain.DeliveryDelivered {
				t.Fatalf("notification = %+v", n)
			}
		})
	}
}

// TestRunWakesAndShutsDown runs the worker: a published message is sent
// after Wake, and Shutdown stops the goroutines (goleak).
func TestRunWakesAndShutsDown(t *testing.T) {
	e := newEnv(t)
	sk := newSink(t, nil)
	// The worker runs on the real clock; the store's rows are dated t0.
	s := e.sender(relay(sk.port()), func(o *Options) { o.Clock = clock.System(); o.MaxAge = 100 * 365 * 24 * time.Hour })
	e.pub = NewPublisher(e.clk, e.ids, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Error("second Start succeeded")
	}
	e.inbound("+48511222333", "wake up")
	e.pub.Wake()
	deadline := time.Now().Add(10 * time.Second)
	for len(sk.got()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("not delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewSenderValidation(t *testing.T) {
	st := newEnv(t).st
	ok := relay(25)
	tests := []struct {
		name string
		mod  func(*Options)
	}{
		{"no store", func(o *Options) { o.Store = nil }},
		{"no host", func(o *Options) { o.Relay.Host = "" }},
		{"bad port", func(o *Options) { o.Relay.Port = 0 }},
		{"bad mode", func(o *Options) { o.Relay.TLSMode = "ssl" }},
		{"bad from", func(o *Options) { o.Relay.From = "nope" }},
		{"no to", func(o *Options) { o.Relay.To = nil }},
		{"bad to", func(o *Options) { o.Relay.To = []string{"x"} }},
		{"bad retry", func(o *Options) { o.Retry = domain.RetryPolicy{MaxAttempts: 1} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := Options{Store: st, Relay: ok}
			tt.mod(&o)
			if _, err := NewSender(o); err == nil {
				t.Error("NewSender() succeeded")
			}
		})
	}
	if s, err := NewSender(Options{Store: st, Relay: Relay{Host: "h", Port: 25, From: "a@b", To: []string{"c@d"}}}); err != nil || s.opts.Relay.TLSMode != TLSStartTLS {
		t.Errorf("defaults: %v", err)
	}
}

func TestComposeSanitizesSender(t *testing.T) {
	e := newEnv(t)
	s := e.sender(relay(25))
	m := domain.Message{ID: "01M", Peer: "Bank\r\nBcc: x@evil", Body: "hi", CreatedAt: t0}
	out := string(s.compose(domain.MailNotification{ID: "01N"}, m, "lte1 (01C)"))
	head, _, _ := strings.Cut(out, "\r\n\r\n")
	if strings.Contains(head, "Bcc:") || strings.Contains(head, "\r\nBcc") {
		t.Errorf("header injection:\n%s", head)
	}
	if !strings.Contains(head, "Message-ID: <01N@example.net>") {
		t.Errorf("message id:\n%s", head)
	}
	if got := truncate("ąąą", 3); got != "ą" {
		t.Errorf("truncate = %q", got)
	}
	if !permanent(errPermanent) || permanent(errors.New("x")) {
		t.Error("permanent")
	}
}

func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
}
