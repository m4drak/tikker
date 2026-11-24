// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package mailnotify forwards inbound SMS by e-mail (PLAN §5 #5, ADR 0004).
//
// A [Publisher] adds one row to the durable mail outbox in the transaction
// that stores the inbound message, so a notification is never lost between
// the commit and the e-mail. A [Sender] leases due rows, reads the message,
// sends one e-mail through the configured SMTP relay (net/smtp, STARTTLS or
// implicit TLS; plain text only to a loopback relay) and records the
// outcome. Delivery is at-least-once like webhooks (ADR 0002): 4xx answers
// and network errors are retried with exponential backoff capped at one hour
// for as long as the message text exists and the notification is younger
// than MaxAge; 5xx answers end the notification. An optional hourly cap
// (MaxPerHour) defers notifications instead of dropping them.
//
// The e-mail carries the full sender number and the text (the owner's own
// mailbox); the subject and the logs carry the masked number only.
package mailnotify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/logging"
	"github.com/m4drak/tikker/internal/store"
)

// TLS modes of the relay connection.
const (
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
	TLSNone     = "none"
)

// Defaults of [Options].
const (
	DefaultTimeout    = 30 * time.Second
	DefaultMaxIdle    = 30 * time.Second
	DefaultPurgeAfter = 7 * 24 * time.Hour
	DefaultPurgeEvery = time.Hour
	// DefaultMaxAge is how long transient failures are retried.
	DefaultMaxAge = 72 * time.Hour
	// rateWindow is the window of MaxPerHour.
	rateWindow         = time.Hour
	defaultLeaseMargin = 30 * time.Second
	finalizeTimeout    = 10 * time.Second
	maxLastErrorLen    = 500
)

// DefaultRetryPolicy is the backoff of notifications: 30 s, 2 m, 8 m, 32 m,
// then hourly. The number of attempts is not limited by it (MaxAttempts is
// ignored): transient failures are retried until Options.MaxAge.
func DefaultRetryPolicy() domain.RetryPolicy {
	return domain.RetryPolicy{MaxAttempts: 8, Initial: 30 * time.Second, Multiplier: 4, Max: time.Hour, Jitter: 0.2}
}

// Relay is the SMTP relay and the e-mail addresses.
type Relay struct {
	Host     string
	Port     int
	TLSMode  string // TLSStartTLS (default), TLSImplicit or TLSNone
	Username string // empty: no AUTH
	Password string
	From     string   // RFC 5322 address
	To       []string // RFC 5322 addresses
	// TLSConfig is the base TLS configuration (tests set RootCAs);
	// ServerName is always the relay host.
	TLSConfig *tls.Config
	// HeloName is sent in EHLO (default "localhost").
	HeloName string
}

// Options configures a [Sender].
type Options struct {
	Store  store.Store // required: mail outbox, messages, channels
	Relay  Relay       // required
	Clock  clock.Clock
	Logger *slog.Logger
	// Timeout bounds one delivery (default DefaultTimeout).
	Timeout time.Duration
	// Retry is the backoff (default DefaultRetryPolicy; MaxAttempts is
	// ignored); Rand its jitter.
	Retry domain.RetryPolicy
	Rand  domain.RandFunc
	// MaxAge ends a notification that still fails transiently this long
	// after the SMS arrived (default DefaultMaxAge).
	MaxAge time.Duration
	// Retention is the SMS body retention (TIKKER_RETENTION): an empty
	// text of a message older than that was purged; a younger one is an
	// empty SMS and is forwarded. Zero: bodies are never purged.
	Retention time.Duration
	// MaxPerHour caps the e-mails (attempts) per hour; notifications over
	// the cap wait for the next window. Zero: no cap.
	MaxPerHour int
	// MaxIdle caps the idle sleep (missed wake-ups); PurgeAfter is how long
	// finished notifications are kept.
	MaxIdle    time.Duration
	PurgeAfter time.Duration
	// Dial opens the TCP connection (tests); default net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// errPermanent marks failures that a retry cannot fix.
var errPermanent = errors.New("permanent failure")

// Publisher stores a notification for every new inbound message. It has
// the method set of inbox.MessagePublisher.
type Publisher struct {
	clk   clock.Clock
	ids   domain.IDGenerator
	waker interface{ Wake() }
}

// NewPublisher returns a publisher; waker (the *Sender) may be nil.
func NewPublisher(clk clock.Clock, ids domain.IDGenerator, waker interface{ Wake() }) *Publisher {
	if clk == nil {
		clk = clock.System()
	}
	if ids == nil {
		ids = domain.NewULIDGenerator(clk)
	}
	return &Publisher{clk: clk, ids: ids, waker: waker}
}

// PublishIncoming adds the notification of m inside tx.
func (p *Publisher) PublishIncoming(ctx context.Context, tx store.Repos, m domain.Message) error {
	now := p.clk.Now()
	err := tx.MailOutbox().Add(ctx, domain.MailNotification{ID: p.ids.NewID(), MessageID: m.ID, CreatedAt: now, NextAttemptAt: now})
	if err != nil {
		return fmt.Errorf("mailnotify: store notification: %w", err)
	}
	return nil
}

// Wake tells the sender that notifications were committed.
func (p *Publisher) Wake() {
	if p.waker != nil {
		p.waker.Wake()
	}
}

// Sender delivers notifications with one worker (the volume is a few SMS).
type Sender struct {
	opts  Options
	st    store.Store
	clk   clock.Clock
	log   *slog.Logger
	lease time.Duration
	from  *mail.Address
	to    []*mail.Address

	// Counters for /metrics.
	sent, failed, retried, deferred atomic.Int64
	// Hourly cap state; touched by the single worker only.
	winStart     time.Time
	winCount     int
	winWarned    bool
	limitedUntil time.Time

	wake     chan struct{}
	mu       sync.Mutex
	started  bool
	stopping bool
	quit     chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewSender validates opts and returns a stopped sender.
func NewSender(opts Options) (*Sender, error) {
	r := &opts.Relay
	if opts.Store == nil {
		return nil, errors.New("mailnotify: Store is required")
	}
	if r.Host == "" || r.Port < 1 || r.Port > 65535 {
		return nil, errors.New("mailnotify: relay host and port are required")
	}
	if r.TLSMode == "" {
		r.TLSMode = TLSStartTLS
	}
	switch r.TLSMode {
	case TLSStartTLS, TLSImplicit, TLSNone:
	default:
		return nil, fmt.Errorf("mailnotify: unknown TLS mode %q", r.TLSMode)
	}
	if r.HeloName == "" {
		r.HeloName = "localhost"
	}
	from, err := mail.ParseAddress(r.From)
	if err != nil {
		return nil, fmt.Errorf("mailnotify: from address: %w", err)
	}
	if len(r.To) == 0 {
		return nil, errors.New("mailnotify: at least one recipient is required")
	}
	to := make([]*mail.Address, len(r.To))
	for i, a := range r.To {
		if to[i], err = mail.ParseAddress(a); err != nil {
			return nil, fmt.Errorf("mailnotify: recipient %d: %w", i+1, err)
		}
	}
	if opts.Clock == nil {
		opts.Clock = clock.System()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Retry == (domain.RetryPolicy{}) {
		opts.Retry = DefaultRetryPolicy()
	}
	if err := opts.Retry.Validate(); err != nil {
		return nil, fmt.Errorf("mailnotify: %w", err)
	}
	if opts.Rand == nil {
		opts.Rand = domain.DefaultRand
	}
	setDuration(&opts.Timeout, DefaultTimeout)
	setDuration(&opts.MaxIdle, DefaultMaxIdle)
	setDuration(&opts.PurgeAfter, DefaultPurgeAfter)
	setDuration(&opts.MaxAge, DefaultMaxAge)
	if opts.MaxPerHour < 0 {
		return nil, errors.New("mailnotify: MaxPerHour must not be negative")
	}
	if opts.Dial == nil {
		d := &net.Dialer{Timeout: opts.Timeout}
		opts.Dial = d.DialContext
	}
	return &Sender{
		opts: opts, st: opts.Store, clk: opts.Clock,
		log:   opts.Logger.With(slog.String("component", "mailnotify")),
		lease: opts.Timeout + defaultLeaseMargin,
		from:  from, to: to,
		wake: make(chan struct{}, 1), quit: make(chan struct{}),
	}, nil
}

func setDuration(d *time.Duration, def time.Duration) {
	if *d <= 0 {
		*d = def
	}
}

// Wake makes the worker look for due notifications. It never blocks.
func (s *Sender) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start launches the worker; it runs until ctx ends or Shutdown.
func (s *Sender) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("mailnotify: sender already started")
	}
	s.started = true
	ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.run(ctx)
	}()
	go func() {
		defer s.wg.Done()
		s.purgeLoop(ctx)
	}()
	s.log.Info("mail notifications started", slog.String("relay", net.JoinHostPort(s.opts.Relay.Host, strconv.Itoa(s.opts.Relay.Port))),
		slog.String("tls", s.opts.Relay.TLSMode), slog.Int("recipients", len(s.to)))
	return nil
}

// Shutdown stops taking notifications and lets an attempt in flight finish
// until ctx ends; then it is cancelled and rescheduled. It returns after the
// goroutines have exited, with ctx's error if an attempt was cancelled.
func (s *Sender) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopping {
		s.stopping = true
		close(s.quit)
	}
	started, cancel := s.started, s.cancel
	s.mu.Unlock()
	if !started {
		return nil
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = fmt.Errorf("mailnotify: shutdown: attempt cancelled: %w", ctx.Err())
		cancel()
		<-done
	}
	cancel()
	return err
}

func (s *Sender) quitting() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func (s *Sender) run(ctx context.Context) {
	for ctx.Err() == nil && !s.quitting() {
		if s.step(ctx) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.quit:
			return
		case <-s.wake:
		case <-s.clk.After(s.idleWait(ctx)):
		}
	}
}

// idleWait is the time until the next notification is due, at most MaxIdle,
// or until the hourly cap opens again.
func (s *Sender) idleWait(ctx context.Context) time.Duration {
	if d := s.limitedUntil.Sub(s.clk.Now()); d > 0 {
		return d
	}
	at, ok, err := s.st.MailOutbox().NextDue(ctx)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.log.Error("next notification due failed", slog.Any("error", err))
		}
		return min(s.opts.MaxIdle, 5*time.Second)
	case !ok:
		return s.opts.MaxIdle
	}
	return min(max(at.Sub(s.clk.Now()), 10*time.Millisecond), s.opts.MaxIdle)
}

// step acquires and attempts one notification; it reports whether it found
// one.
func (s *Sender) step(ctx context.Context) bool {
	now := s.clk.Now()
	if !s.allow(ctx, now) {
		return false
	}
	n, ok, err := s.st.MailOutbox().Acquire(ctx, now, now.Add(s.lease))
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("acquire notification failed", slog.Any("error", err))
		}
		return false
	}
	if !ok {
		return false
	}
	s.winCount++
	s.attempt(ctx, n)
	return true
}

// allow applies the hourly cap (fixed windows). Over the cap nothing is
// acquired: the notifications stay pending and go out in a later window. It
// warns once per window, when something is actually waiting.
func (s *Sender) allow(ctx context.Context, now time.Time) bool {
	if s.opts.MaxPerHour <= 0 {
		return true
	}
	if s.winStart.IsZero() || now.Sub(s.winStart) >= rateWindow {
		s.winStart, s.winCount, s.winWarned = now, 0, false
	}
	if s.winCount < s.opts.MaxPerHour {
		return true
	}
	s.limitedUntil = s.winStart.Add(rateWindow)
	if !s.winWarned {
		if at, ok, err := s.st.MailOutbox().NextDue(ctx); err == nil && ok && !at.After(now) {
			s.winWarned = true
			s.deferred.Add(1)
			s.log.Warn("e-mail notification cap reached; notifications deferred to the next window",
				slog.Int("max_per_hour", s.opts.MaxPerHour), slog.Time("resume_at", s.limitedUntil))
		}
	}
	return false
}

// Stats are the counters of the sender (for /metrics).
type Stats struct {
	Pending  int   // notifications waiting (or retrying)
	Sent     int64 // e-mails accepted by the relay
	Failed   int64 // notifications given up
	Retried  int64 // attempts rescheduled
	Deferred int64 // hourly windows in which the cap held notifications back
}

// Stats returns the counters and the number of pending notifications.
func (s *Sender) Stats(ctx context.Context) (Stats, error) {
	n, err := s.st.MailOutbox().CountPending(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("mailnotify: count pending: %w", err)
	}
	return Stats{Pending: n, Sent: s.sent.Load(), Failed: s.failed.Load(), Retried: s.retried.Load(), Deferred: s.deferred.Load()}, nil
}

// attempt sends one notification and records the outcome.
func (s *Sender) attempt(ctx context.Context, n domain.MailNotification) {
	log := s.log.With(slog.String("notification_id", n.ID.String()), slog.String("message_id", n.MessageID.String()),
		slog.Int("attempt", n.Attempts))
	if s.clk.Now().Sub(n.CreatedAt) >= s.opts.MaxAge {
		// Also ends a notification whose attempts keep crashing.
		s.finish(log, n, fmt.Errorf("%w: not delivered within %s", errPermanent, s.opts.MaxAge))
		return
	}
	m, err := s.st.Messages().Get(ctx, n.MessageID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		s.finish(log, n, fmt.Errorf("%w: the message no longer exists", errPermanent))
		return
	case err != nil:
		s.finish(log, n, fmt.Errorf("read message: %w", err))
		return
	case m.Body == "" && s.purged(m):
		s.finish(log, n, fmt.Errorf("%w: the message text is gone (retention)", errPermanent))
		return
	}
	channel := m.ChannelID.String()
	if c, err := s.st.Channels().Get(ctx, m.ChannelID); err == nil {
		channel = c.Name + " (" + c.ID.String() + ")"
	}
	data := s.compose(n, m, channel)
	err = s.deliver(ctx, data)
	if err != nil && ctx.Err() != nil {
		// Forced shutdown: the relay may have taken it; retry after restart.
		s.record(log, func(c context.Context) error {
			return s.st.MailOutbox().Reschedule(c, n.Lease(), s.clk.Now(), s.clk.Now(), "interrupted by shutdown")
		})
		return
	}
	if err == nil {
		s.record(log, func(c context.Context) error { return s.st.MailOutbox().MarkSent(c, n.Lease(), s.clk.Now()) })
		s.sent.Add(1)
		log.Info("sms forwarded by e-mail", logging.PhoneAttr("from", m.Peer))
		return
	}
	s.finish(log, n, err)
}

// purged reports whether the empty text of m was removed by retention:
// PurgeBodies empties messages created before now-Retention, so an empty
// text of a younger message is a genuinely empty SMS.
func (s *Sender) purged(m domain.Message) bool {
	return s.opts.Retention > 0 && m.CreatedAt.Before(s.clk.Now().Add(-s.opts.Retention))
}

// finish records a failed attempt: permanent errors and notifications
// older than MaxAge end, the rest are rescheduled with backoff (capped by
// the policy's Max, one hour by default).
func (s *Sender) finish(log *slog.Logger, n domain.MailNotification, err error) {
	now := s.clk.Now()
	msg := truncate(err.Error(), maxLastErrorLen)
	if !permanent(err) && now.Sub(n.CreatedAt) < s.opts.MaxAge {
		next := now.Add(s.opts.Retry.Delay(n.Attempts, s.opts.Rand))
		s.record(log, func(c context.Context) error { return s.st.MailOutbox().Reschedule(c, n.Lease(), now, next, msg) })
		s.retried.Add(1)
		log.Warn("e-mail notification failed, will retry", slog.String("error", msg), slog.Time("next_attempt_at", next))
		return
	}
	if !permanent(err) {
		msg = truncate("gave up after "+s.opts.MaxAge.String()+": "+msg, maxLastErrorLen)
	}
	s.record(log, func(c context.Context) error { return s.st.MailOutbox().MarkFailed(c, n.Lease(), now, msg) })
	s.failed.Add(1)
	log.Error("e-mail notification failed", slog.String("error", msg))
}

// permanent reports errors a retry cannot fix: our own checks and 5xx
// answers of the relay.
func permanent(err error) bool {
	var te *textproto.Error
	return errors.Is(err, errPermanent) || (errors.As(err, &te) && te.Code >= 500)
}

// record writes a result with its own timeout, so it survives shutdown.
func (s *Sender) record(log *slog.Logger, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer cancel()
	switch err := fn(ctx); {
	case err == nil:
	case errors.Is(err, domain.ErrLeaseLost):
		log.Warn("lease lost before the result was recorded", slog.Any("error", err))
	default:
		log.Error("recording notification result failed", slog.Any("error", err))
	}
}

// deliver runs one SMTP transaction with the relay.
func (s *Sender) deliver(ctx context.Context, data []byte) error {
	r := s.opts.Relay
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	conn, err := s.opts.Dial(ctx, "tcp", net.JoinHostPort(r.Host, strconv.Itoa(r.Port)))
	if err != nil {
		return fmt.Errorf("connect to the relay: %w", err)
	}
	// Socket deadlines are compared with the OS clock.
	_ = conn.SetDeadline(time.Now().Add(s.opts.Timeout))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if r.TLSConfig != nil {
		tlsCfg = r.TLSConfig.Clone()
	}
	tlsCfg.ServerName = r.Host
	if r.TLSMode == TLSImplicit {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("tls handshake with the relay: %w", err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, r.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("relay greeting: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello(r.HeloName); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	if r.TLSMode == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%w: the relay does not offer STARTTLS", errPermanent)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if r.Username != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return fmt.Errorf("%w: the relay does not offer AUTH", errPermanent)
		}
		if err := c.Auth(smtp.PlainAuth("", r.Username, r.Password, r.Host)); err != nil {
			return fmt.Errorf("AUTH: %w", err)
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, to := range s.to {
		if err := c.Rcpt(to.Address); err != nil {
			return fmt.Errorf("RCPT TO: %w", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	_ = c.Quit() // the relay has accepted the message
	return nil
}

// compose renders the e-mail: the subject has the masked number, the body
// the full number, time, channel and text.
func (s *Sender) compose(n domain.MailNotification, m domain.Message, channel string) []byte {
	var b bytes.Buffer
	to := make([]string, len(s.to))
	for i, a := range s.to {
		to[i] = a.String()
	}
	peer := oneLine(m.Peer)
	domainPart := "tikker.invalid"
	if _, d, ok := strings.Cut(s.from.Address, "@"); ok && d != "" {
		domainPart = d
	}
	received := m.ReceivedAt
	if received.IsZero() {
		received = m.CreatedAt
	}
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", s.from.String())
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", "SMS from "+logging.MaskPhone(peer)))
	header("Date", s.clk.Now().Format(time.RFC1123Z))
	header("Message-ID", "<"+n.ID.String()+"@"+domainPart+">")
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	header("Auto-Submitted", "auto-generated")
	header("X-Tikker-Message-Id", m.ID.String())
	b.WriteString("\r\n")

	var body strings.Builder
	body.WriteString("From: " + peer + "\n")
	body.WriteString("Received: " + received.UTC().Format(time.RFC3339) + "\n")
	if channel != "" {
		body.WriteString("Channel: " + oneLine(channel) + "\n")
	}
	body.WriteString("Message: " + m.ID.String() + "\n\n")
	if m.Body == "" {
		body.WriteString("(empty message)")
	}
	body.WriteString(m.Body)
	body.WriteString("\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(body.String(), "\n", "\r\n")))
	_ = qp.Close()
	return b.Bytes()
}

// oneLine drops control characters, so a sender name cannot add headers.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// purgeLoop deletes finished notifications older than PurgeAfter.
func (s *Sender) purgeLoop(ctx context.Context) {
	for {
		n, err := s.st.MailOutbox().Purge(ctx, s.clk.Now().Add(-s.opts.PurgeAfter))
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			s.log.Error("purge notifications failed", slog.Any("error", err))
		case n > 0:
			s.log.Info("purged notifications", slog.Int("count", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-s.quit:
			return
		case <-s.clk.After(DefaultPurgeEvery):
		}
	}
}
