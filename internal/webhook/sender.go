// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// SecretPurpose is the purpose string under which webhook secrets are sealed
// (secrets.Keyring.Seal/Open); the webhook ID is the record ID.
const SecretPurpose = "webhook"

// Defaults of [SenderOptions].
const (
	DefaultTimeout      = 10 * time.Second
	DefaultWorkers      = 2
	DefaultMaxIdle      = 30 * time.Second
	DefaultLeaseMargin  = 30 * time.Second
	DefaultPurgeAfter   = 7 * 24 * time.Hour
	DefaultPurgeEvery   = time.Hour
	maxResponseDrain    = 64 << 10
	maxLastErrorLen     = 500
	defaultUserAgent    = "tikker-webhook"
	finalizeTimeout     = 10 * time.Second
	errRetriesExhausted = "retries exhausted: "
)

// DefaultRetryPolicy returns the retry schedule of deliveries: nominal delays
// of 10 s, 40 s, 2 m 40 s, 10 m 40 s, 42 m 40 s and 1 h after the failures of
// 7 attempts in total (about 2 hours of retrying).
func DefaultRetryPolicy() domain.RetryPolicy {
	return domain.RetryPolicy{MaxAttempts: 7, Initial: 10 * time.Second, Multiplier: 4, Max: time.Hour, Jitter: 0.2}
}

// SecretOpener decrypts a stored webhook secret; *secrets.Keyring implements
// it.
type SecretOpener interface {
	Open(purpose, recordID string, sealed []byte) ([]byte, error)
}

// SenderOptions configures a [Sender].
type SenderOptions struct {
	Store   store.WebhookRepo // required
	Secrets SecretOpener      // required
	Clock   clock.Clock       // default system clock
	Logger  *slog.Logger      // default slog.Default()
	// Client sends the requests (default: a client with Timeout that does
	// not follow redirects). A custom client should not follow redirects
	// either: a 3xx answer is a permanent failure.
	Client *http.Client
	// Timeout bounds one request (default DefaultTimeout).
	Timeout time.Duration
	// Retry is the retry schedule (default DefaultRetryPolicy). Rand is its
	// jitter source.
	Retry domain.RetryPolicy
	Rand  domain.RandFunc
	// Workers is the number of concurrent attempts (default DefaultWorkers).
	Workers int
	// MaxIdle caps how long an idle worker sleeps (safety net for missed
	// wake-ups; default DefaultMaxIdle).
	MaxIdle time.Duration
	// PurgeAfter is how long finished deliveries are kept (default
	// DefaultPurgeAfter); the purge runs every DefaultPurgeEvery.
	PurgeAfter time.Duration
	// UserAgent is the User-Agent header (default "tikker-webhook").
	UserAgent string
	// PayloadRetention bounds how long the payload (SMS text and number) of
	// an undelivered event is kept; older pending deliveries are failed and
	// emptied. Use the SMS retention (zero: no limit).
	PayloadRetention time.Duration

	// AllowPrivate reports hosts (as written in the URL) that may resolve to
	// private, loopback or link-local addresses: the administrator's
	// allowlist (TIKKER_ALLOWED_HTTP_HOSTS). Every other destination is
	// refused when it resolves to such an address (SSRF guard).
	AllowPrivate func(host string) bool
	// The following hooks exist for tests; the defaults are the system
	// resolver, a net.Dialer and the system roots.
	Resolve   func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	TLSConfig *tls.Config
}

// Sender runs the delivery workers.
type Sender struct {
	opts  SenderOptions
	repo  store.WebhookRepo
	clk   clock.Clock
	log   *slog.Logger
	lease time.Duration

	mu       sync.Mutex
	wakes    []chan struct{}
	started  bool
	stopping bool
	quit     chan struct{} // closed: take no new deliveries
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewSender validates opts and returns a stopped sender.
func NewSender(opts SenderOptions) (*Sender, error) {
	if opts.Store == nil || opts.Secrets == nil {
		return nil, errors.New("webhook: SenderOptions.Store and Secrets are required")
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
		return nil, fmt.Errorf("webhook: %w", err)
	}
	if opts.Rand == nil {
		opts.Rand = domain.DefaultRand
	}
	setDuration(&opts.Timeout, DefaultTimeout)
	setDuration(&opts.MaxIdle, DefaultMaxIdle)
	setDuration(&opts.PurgeAfter, DefaultPurgeAfter)
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.UserAgent == "" {
		opts.UserAgent = defaultUserAgent
	}
	s := &Sender{
		opts: opts, repo: opts.Store, clk: opts.Clock,
		log:   opts.Logger.With(slog.String("component", "webhook")),
		lease: opts.Timeout + DefaultLeaseMargin,
		quit:  make(chan struct{}),
	}
	if opts.Client == nil {
		// No proxy (the environment must not redirect the traffic) and a
		// dialer that refuses internal destinations.
		s.opts.Client = &http.Client{
			Transport: &http.Transport{
				Proxy:               nil,
				DialContext:         s.guardedDial,
				TLSClientConfig:     opts.TLSConfig.Clone(),
				TLSHandshakeTimeout: opts.Timeout,
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        10,
				IdleConnTimeout:     30 * time.Second,
			},
			Timeout:       opts.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	for range opts.Workers {
		s.wakes = append(s.wakes, make(chan struct{}, 1))
	}
	return s, nil
}

func setDuration(d *time.Duration, def time.Duration) {
	if *d <= 0 {
		*d = def
	}
}

// Wake makes idle workers look for due deliveries. It never blocks.
func (s *Sender) Wake() {
	for _, c := range s.wakes {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// Start launches the workers; they run until ctx ends or [Sender.Stop].
func (s *Sender) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("webhook: sender already started")
	}
	s.started = true
	ctx, s.cancel = context.WithCancel(ctx)
	for i := range s.opts.Workers {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.run(ctx, s.wakes[i])
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.purgeLoop(ctx)
	}()
	s.log.Info("webhook sender started", slog.Int("workers", s.opts.Workers))
	return nil
}

// Shutdown stops the sender: workers take no new deliveries and attempts in
// flight may finish until ctx ends. Then they are cancelled; a cancelled
// attempt is rescheduled and recorded as possibly received. It returns after
// every goroutine has exited, with ctx's error if attempts had to be
// cancelled.
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
		err = fmt.Errorf("webhook: shutdown: in-flight attempts cancelled: %w", ctx.Err())
		cancel()
		<-done
	}
	cancel()
	s.opts.Client.CloseIdleConnections()
	return err
}

// Stop is Shutdown without grace: attempts in flight are cancelled at once.
func (s *Sender) Stop() {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Shutdown(ctx)
}

func (s *Sender) quitting() bool {
	select {
	case <-s.quit:
		return true
	default:
		return false
	}
}

func (s *Sender) run(ctx context.Context, wake <-chan struct{}) {
	for ctx.Err() == nil && !s.quitting() {
		if s.step(ctx) {
			continue
		}
		wait := s.idleWait(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.quit:
			return
		case <-wake:
		case <-s.clk.After(wait):
		}
	}
}

// idleWait is the time until the next delivery is due, at most MaxIdle.
func (s *Sender) idleWait(ctx context.Context) time.Duration {
	at, ok, err := s.repo.NextDeliveryDue(ctx)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.log.Error("next delivery due failed", slog.Any("error", err))
		}
		return min(s.opts.MaxIdle, 5*time.Second)
	case !ok:
		return s.opts.MaxIdle
	}
	// A floor avoids a busy loop when a delivery is due but cannot be
	// acquired (taken by another worker meanwhile).
	return min(max(at.Sub(s.clk.Now()), 10*time.Millisecond), s.opts.MaxIdle)
}

// step acquires and attempts one delivery; it reports whether it found one.
func (s *Sender) step(ctx context.Context) bool {
	now := s.clk.Now()
	job, ok, err := s.repo.AcquireDelivery(ctx, now, now.Add(s.lease))
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("acquire delivery failed", slog.Any("error", err))
		}
		return false
	}
	if !ok {
		return false
	}
	s.attempt(ctx, job)
	return true
}

// Result classes of one HTTP attempt.
type result int

const (
	resDelivered result = iota
	resRetry
	resPermanent
)

// classify maps the HTTP answer (or the transport error) to a result.
func classify(code int, err error) result {
	switch {
	case err != nil:
		return resRetry
	case code >= 200 && code < 300:
		return resDelivered
	case code >= 500, code == http.StatusRequestTimeout, code == http.StatusTooManyRequests:
		return resRetry
	default: // other 4xx and 3xx (redirects are not followed)
		return resPermanent
	}
}

// Sign returns the value of the X-Tikker-Signature header for body.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether sig is the valid signature of body under secret. It
// is the check a receiver should perform (constant-time comparison).
func Verify(secret, body []byte, sig string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(sig))
}

// attempt performs one attempt of a leased delivery and records the outcome.
func (s *Sender) attempt(ctx context.Context, job store.DeliveryJob) {
	d, w := job.Delivery, job.Webhook
	log := s.log.With(slog.String("webhook_id", w.ID.String()), slog.String("delivery_id", d.ID.String()),
		slog.String("event", d.Event.String()), slog.Int("attempt", d.Attempts))

	// An attempt that began, but whose result was never recorded (crash), is
	// counted; do not let a poison delivery loop forever.
	if d.Attempts > s.opts.Retry.MaxAttempts {
		s.finish(log, d, resPermanent, "attempt limit reached without a recorded result", 0)
		return
	}

	// The request must finish inside the lease, or another worker may take
	// the delivery over while this one is still sending.
	remaining := d.LeaseUntil.Sub(s.clk.Now())
	if remaining <= time.Second {
		log.Warn("lease expired before the request could start; leaving the delivery to the next attempt")
		return
	}

	secret, err := s.opts.Secrets.Open(SecretPurpose, w.ID.String(), w.SecretEnc)
	var code int
	if err != nil {
		// Not the receiver's fault and fixable (restore the key): retry.
		err = fmt.Errorf("cannot decrypt webhook secret: %w", err)
	} else {
		code, err = s.post(ctx, w, d, secret, min(s.opts.Timeout, remaining-time.Second/2))
	}
	if ctx.Err() != nil && err != nil {
		// Forced shutdown: the receiver may have got the request. Retry at
		// once after the restart (at-least-once).
		s.record(log, func(c context.Context) error {
			return s.repo.RescheduleDelivery(c, d.Lease(), s.clk.Now(), s.clk.Now(),
				"interrupted by shutdown; the receiver may have received it", 0)
		})
		return
	}

	switch r := classify(code, err); {
	case errors.Is(err, errBlockedDestination):
		s.finish(log, d, resPermanent, err.Error(), 0)
	case r == resDelivered:
		s.record(log, func(c context.Context) error { return s.repo.MarkDelivered(c, d.Lease(), s.clk.Now(), code) })
		log.Info("webhook delivered", slog.Int("status", code))
	default:
		s.finish(log, d, r, describe(code, err), code)
	}
}

// finish records a failed attempt: permanent errors and exhausted retries end
// the delivery, the rest reschedule it with backoff.
func (s *Sender) finish(log *slog.Logger, d domain.WebhookDelivery, r result, msg string, code int) {
	now := s.clk.Now()
	msg = truncate(msg, maxLastErrorLen)
	switch {
	case r == resRetry && d.Attempts < s.opts.Retry.MaxAttempts:
		next := now.Add(s.opts.Retry.Delay(d.Attempts, s.opts.Rand))
		s.record(log, func(c context.Context) error { return s.repo.RescheduleDelivery(c, d.Lease(), now, next, msg, code) })
		log.Warn("webhook attempt failed, will retry", slog.String("error", msg), slog.Time("next_attempt_at", next))
	default:
		if r == resRetry {
			msg = truncate(errRetriesExhausted+msg, maxLastErrorLen)
		}
		s.record(log, func(c context.Context) error { return s.repo.MarkDeliveryFailed(c, d.Lease(), now, msg, code) })
		log.Error("webhook delivery failed", slog.String("error", msg))
	}
}

// record runs a result write with its own timeout, so it survives shutdown.
func (s *Sender) record(log *slog.Logger, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer cancel()
	switch err := fn(ctx); {
	case err == nil:
	case errors.Is(err, domain.ErrLeaseLost):
		log.Warn("lease lost before the result was recorded", slog.Any("error", err))
	default:
		// The lease expires and the delivery is attempted again.
		log.Error("recording delivery result failed", slog.Any("error", err))
	}
}

// post sends the request and returns the HTTP status (0 on transport errors).
func (s *Sender) post(ctx context.Context, w domain.Webhook, d domain.WebhookDelivery, secret []byte, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(d.Payload))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", s.opts.UserAgent)
	h.Set(HeaderEvent, d.Event.String())
	h.Set(HeaderDelivery, d.ID.String())
	h.Set(HeaderSignature, Sign(secret, d.Payload))
	resp, err := s.opts.Client.Do(req) //nolint:gosec // G107: the URL is configured by an administrator
	if err != nil {
		return 0, fmt.Errorf("request failed: %w", unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.CopyN(io.Discard, resp.Body, maxResponseDrain) // let the connection be reused
	return resp.StatusCode, nil
}

// unwrapURLError drops the URL (it may carry a secret path) from a client
// error.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

func describe(code int, err error) string {
	if err != nil {
		return err.Error()
	}
	return "unexpected HTTP status " + strconv.Itoa(code)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// purgeLoop deletes finished deliveries older than PurgeAfter.
func (s *Sender) purgeLoop(ctx context.Context) {
	for {
		if r := s.opts.PayloadRetention; r > 0 {
			now := s.clk.Now()
			if n, err := s.repo.ExpireDeliveries(ctx, now.Add(-r), now); err != nil && ctx.Err() == nil {
				s.log.Error("expire webhook payloads failed", slog.Any("error", err))
			} else if n > 0 {
				s.log.Warn("undelivered webhook events dropped after the retention period", slog.Int("count", n))
			}
		}
		n, err := s.repo.PurgeDeliveries(ctx, s.clk.Now().Add(-s.opts.PurgeAfter))
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			s.log.Error("purge webhook deliveries failed", slog.Any("error", err))
		case n > 0:
			s.log.Info("purged webhook deliveries", slog.Int("count", n))
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

// ---------------------------------------------------------------------------
// Receiver side

// ErrBadSignature is returned by [VerifyRequest].
var ErrBadSignature = errors.New("webhook: signature does not match")

// VerifyRequest is what a receiver does with a delivery: it checks the
// signature of body, parses the envelope and requires the X-Tikker-Delivery
// header to equal the signed delivery ID (so a replayed request cannot carry
// a different ID). Deduplicate on the returned Envelope.ID.
func VerifyRequest(secret, body []byte, h http.Header) (Envelope, error) {
	if !Verify(secret, body, h.Get(HeaderSignature)) {
		return Envelope{}, ErrBadSignature
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, fmt.Errorf("webhook: parse body: %w", err)
	}
	if env.ID == "" || h.Get(HeaderDelivery) != env.ID {
		return Envelope{}, fmt.Errorf("%w: delivery header differs from the signed ID", ErrBadSignature)
	}
	return env, nil
}

// ---------------------------------------------------------------------------
// Destination guard (SSRF)

var errBlockedDestination = errors.New("destination address is not allowed")

// blockedPrefixes are ranges that are not public unicast addresses and are
// not covered by the netip predicates below.
var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4",
	"100::/64", "fc00::/7", "fe80::/10",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// BlockedAddr reports whether a webhook must not connect to ip: loopback,
// private, link-local (including the 169.254.169.254 metadata address),
// unspecified, multicast, carrier-grade NAT, benchmarking and reserved ranges.
func BlockedAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Sender) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if s.opts.Resolve != nil {
		return s.opts.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// guardedDial resolves the host itself, drops every blocked address and dials
// an approved literal IP. TLS still verifies the certificate against the host
// name of the URL (the transport derives ServerName from it, not from the
// dialed address). Resolution and dialing happen together, so a DNS answer
// cannot change between the check and the connection.
func (s *Sender) guardedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("bad address: %w", err)
	}
	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = s.resolve(ctx, host); err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	trusted := s.opts.AllowPrivate != nil && s.opts.AllowPrivate(host)
	dial := s.opts.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: s.opts.Timeout}).DialContext
	}
	var errs []error
	approved := 0
	for _, ip := range ips {
		if !trusted && BlockedAddr(ip) {
			continue
		}
		approved++
		conn, err := dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	if approved == 0 {
		return nil, fmt.Errorf("%w: %s resolves to an internal address", errBlockedDestination, host)
	}
	return nil, errors.Join(errs...)
}
