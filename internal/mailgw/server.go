// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package mailgw is the e-mail to SMS gateway of tikker (PLAN §5 #5,
// ADR 0004): a minimal SMTP submission server (RFC 5321, 4954, 3207) that
// turns an e-mail to <number>@<domain> into an SMS.
//
// It is deliberately small and strict:
//
//   - authentication is mandatory: MAIL FROM is refused before a successful
//     AUTH PLAIN or AUTH LOGIN whose password is an API token with scope
//     sms:send (the same check as the REST API), so it is never an open
//     relay; RCPT TO accepts the configured domain only;
//   - AUTH is offered only over TLS (STARTTLS or implicit TLS); a server
//     without TLS ([Options].Insecure) talks to loopback peers only;
//   - the text goes through the same send path as POST /api/v1/messages
//     ([Sender], the sending service), one message per recipient, stored
//     atomically; the Message-ID is the idempotency key, so a client that
//     retries does not send twice;
//   - sizes, line lengths, recipients, connections (in total and per client
//     address), failed logins, time and commands before AUTH, time per
//     command, session lifetime and messages per session are bounded; there
//     is no PIPELINING, BDAT or relaying;
//   - the token is checked again at every MAIL FROM and before a message is
//     stored, so revoking it, its expiry or disabling its owner also stops
//     sessions that are already open.
package mailgw

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m4drak/tikker/internal/auth"
	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/sending"
	"github.com/m4drak/tikker/internal/sms/phone"
	"github.com/m4drak/tikker/internal/store"
)

// Defaults of [Options].
const (
	DefaultMaxMessageBytes = 64 << 10
	DefaultMaxRecipients   = 10
	DefaultMaxConnections  = 20
	DefaultCommandTimeout  = time.Minute
	DefaultDataTimeout     = 5 * time.Minute
	// DefaultMaxConnectionsPerIP bounds the sessions of one client address
	// (IPv6: one /64), so a few hosts cannot hold every slot.
	DefaultMaxConnectionsPerIP = 3
	// DefaultMaxSessionDuration and DefaultMaxMessagesPerSession bound one
	// session: a long-lived session cannot outlive its token by much, and
	// a client has to reconnect (and authenticate) from time to time.
	DefaultMaxSessionDuration    = 10 * time.Minute
	DefaultMaxMessagesPerSession = 100
	// DefaultPreAuthTimeout and DefaultMaxPreAuthCommands bound a session
	// that has not authenticated yet.
	DefaultPreAuthTimeout     = 30 * time.Second
	DefaultMaxPreAuthCommands = 10
	// DefaultHandshakeTimeout bounds a TLS handshake.
	DefaultHandshakeTimeout = 10 * time.Second
	// maxLine is the longest line accepted, in commands and in DATA
	// (RFC 5321 allows 1000; base64 and quoted-printable use 76).
	maxLine = 4096
	// maxCommandLine is the longest command line.
	maxCommandLine = 2048
	// maxAuthFailures per connection; then the connection is closed.
	maxAuthFailures = 3
	// maxBadCommands per connection; then the connection is closed.
	maxBadCommands = 10
	// enqueueTimeout bounds storing one e-mail's messages.
	enqueueTimeout = 30 * time.Second
	// authTimeout bounds one token check.
	authTimeout = 10 * time.Second
)

// ErrServerClosed is returned by [Server.Serve] after [Server.Shutdown].
var ErrServerClosed = errors.New("mailgw: server closed")

// Authenticator checks an API token; *auth.Service implements it. Every
// invalid token yields auth.ErrInvalidToken.
type Authenticator interface {
	AuthenticateToken(ctx context.Context, raw string) (auth.Principal, error)
}

// Sender stores messages like the REST API; *sending.Service implements it.
type Sender interface {
	SendBatch(ctx context.Context, actor domain.Actor, reqs []sending.Request, so sending.SendOptions) (store.EnqueueResult, []sending.FieldError, error)
}

// RateLimiter is the per-token request budget shared with the API;
// *api.TokenLimiter implements it.
type RateLimiter interface {
	Allow(id domain.ID) (ok bool, retryAfter time.Duration)
}

// Options configures a [Server].
type Options struct {
	// Domain is the recipient domain (required): RCPT TO:<number>@Domain.
	Domain string
	// Hostname is announced in the greeting (default Domain).
	Hostname string
	// TLSConfig enables STARTTLS, or implicit TLS with ImplicitTLS. It is
	// required unless Insecure is set.
	TLSConfig   *tls.Config
	ImplicitTLS bool
	// Insecure runs without TLS: AUTH is accepted in clear text, and only
	// from loopback peers (configuration allows it only on a loopback
	// listener).
	Insecure bool

	MaxMessageBytes int
	MaxRecipients   int
	MaxConnections  int
	CommandTimeout  time.Duration
	DataTimeout     time.Duration
	// MaxConnectionsPerIP bounds concurrent sessions per client address
	// (IPv6: per /64).
	MaxConnectionsPerIP int
	// MaxSessionDuration and MaxMessagesPerSession end a session with 421
	// when reached.
	MaxSessionDuration    time.Duration
	MaxMessagesPerSession int
	// PreAuthTimeout and MaxPreAuthCommands bound the time and the commands
	// before a successful AUTH; then the session ends with 421.
	PreAuthTimeout     time.Duration
	MaxPreAuthCommands int
	// HandshakeTimeout bounds a TLS handshake (STARTTLS or implicit).
	HandshakeTimeout time.Duration
	// AuthLimit bounds failed AUTH per client address (default
	// DefaultAuthLimit).
	AuthLimit Limit
	// SubjectInBody puts the subject on the first line of the SMS.
	SubjectInBody bool

	// DefaultRegion and AllowShortNumbers are the number rules of the
	// sending service (TIKKER_DEFAULT_REGION, TIKKER_ALLOW_SHORT_NUMBERS).
	DefaultRegion     string
	AllowShortNumbers bool

	Auth    Authenticator // required
	Sender  Sender        // required
	Limiter RateLimiter   // optional per-token budget
	Clock   clock.Clock   // default system clock
	Logger  *slog.Logger  // default slog.Default()
	// BaseContext is the parent of the contexts used for the store; it is
	// not cancelled by Shutdown (default context.Background()).
	BaseContext context.Context
}

// Server is an SMTP submission server. Create it with [New], run it with
// [Server.Serve] and stop it with [Server.Shutdown].
type Server struct {
	opts    Options
	clk     clock.Clock
	log     *slog.Logger
	phone   phone.Options
	authLim *authLimiter
	sem     chan struct{}
	nextID  atomic.Uint64

	mu       sync.Mutex
	ln       net.Listener
	sessions map[*session]struct{}
	perIP    map[string]int // open sessions per client key
	closing  bool
	wg       sync.WaitGroup
}

// New validates opts and returns a server.
func New(opts Options) (*Server, error) {
	opts.Domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(opts.Domain), "."))
	switch {
	case opts.Domain == "":
		return nil, errors.New("mailgw: Domain is required")
	case opts.Auth == nil || opts.Sender == nil:
		return nil, errors.New("mailgw: Auth and Sender are required")
	case opts.TLSConfig == nil && !opts.Insecure:
		return nil, errors.New("mailgw: TLSConfig is required unless Insecure")
	case opts.TLSConfig != nil && opts.Insecure:
		return nil, errors.New("mailgw: TLSConfig and Insecure are exclusive")
	case opts.ImplicitTLS && opts.TLSConfig == nil:
		return nil, errors.New("mailgw: ImplicitTLS needs TLSConfig")
	}
	if opts.Hostname == "" {
		opts.Hostname = opts.Domain
	}
	setInt(&opts.MaxMessageBytes, DefaultMaxMessageBytes)
	setInt(&opts.MaxRecipients, DefaultMaxRecipients)
	setInt(&opts.MaxConnections, DefaultMaxConnections)
	setDuration(&opts.CommandTimeout, DefaultCommandTimeout)
	setDuration(&opts.DataTimeout, DefaultDataTimeout)
	setInt(&opts.MaxConnectionsPerIP, DefaultMaxConnectionsPerIP)
	setDuration(&opts.MaxSessionDuration, DefaultMaxSessionDuration)
	setInt(&opts.MaxMessagesPerSession, DefaultMaxMessagesPerSession)
	setDuration(&opts.PreAuthTimeout, DefaultPreAuthTimeout)
	setInt(&opts.MaxPreAuthCommands, DefaultMaxPreAuthCommands)
	setDuration(&opts.HandshakeTimeout, DefaultHandshakeTimeout)
	if opts.AuthLimit == (Limit{}) {
		opts.AuthLimit = DefaultAuthLimit
	}
	if opts.AuthLimit.Burst < 1 || opts.AuthLimit.Every <= 0 {
		return nil, fmt.Errorf("mailgw: invalid AuthLimit %+v", opts.AuthLimit)
	}
	if opts.DefaultRegion == "" {
		opts.DefaultRegion = "PL"
	}
	if opts.Clock == nil {
		opts.Clock = clock.System()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.BaseContext == nil {
		opts.BaseContext = context.Background()
	}
	if opts.TLSConfig != nil {
		opts.TLSConfig = opts.TLSConfig.Clone()
		if opts.TLSConfig.MinVersion < tls.VersionTLS12 {
			opts.TLSConfig.MinVersion = tls.VersionTLS12
		}
	}
	return &Server{
		opts: opts, clk: opts.Clock,
		log:      opts.Logger.With(slog.String("component", "mailgw")),
		phone:    phone.Options{DefaultRegion: opts.DefaultRegion, AllowShort: opts.AllowShortNumbers},
		authLim:  newAuthLimiter(opts.AuthLimit),
		sem:      make(chan struct{}, opts.MaxConnections),
		sessions: map[*session]struct{}{},
		perIP:    map[string]int{},
	}, nil
}

func setInt(v *int, def int) {
	if *v <= 0 {
		*v = def
	}
}

func setDuration(d *time.Duration, def time.Duration) {
	if *d <= 0 {
		*d = def
	}
}

// Serve accepts connections on ln until [Server.Shutdown]; it then returns
// ErrServerClosed. ln is closed when Serve returns.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		_ = ln.Close()
		return ErrServerClosed
	}
	s.ln = ln
	// Serve counts as a member of wg, so that the sessions it adds never
	// race with the Wait of Shutdown on a zero counter.
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	defer func() { _ = ln.Close() }()
	s.log.Info("smtp gateway listening", slog.String("addr", ln.Addr().String()),
		slog.Bool("starttls", s.opts.TLSConfig != nil && !s.opts.ImplicitTLS),
		slog.Bool("implicit_tls", s.opts.ImplicitTLS), slog.String("domain", s.opts.Domain))

	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.isClosing() {
				return ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
				<-s.clk.After(backoff)
				continue
			}
			return fmt.Errorf("mailgw: accept: %w", err)
		}
		backoff = 0
		s.accept(conn)
	}
}

func (s *Server) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// accept starts a session, or turns the connection away when the server is
// full or closing.
func (s *Server) accept(conn net.Conn) {
	select {
	case s.sem <- struct{}{}:
	default:
		s.reject(conn, "421 4.3.2 Too many connections, try again later")
		return
	}
	sess := newSession(s, conn)
	s.mu.Lock()
	switch {
	case s.closing:
		s.mu.Unlock()
		<-s.sem
		s.reject(conn, "421 4.3.2 Service shutting down")
		return
	case s.perIP[sess.clientIP] >= s.opts.MaxConnectionsPerIP:
		s.mu.Unlock()
		<-s.sem
		s.log.Warn("smtp connection refused: too many from one address", slog.String("client", sess.clientIP))
		s.reject(conn, "421 4.7.0 Too many connections from your address, try again later")
		return
	}
	s.sessions[sess] = struct{}{}
	s.perIP[sess.clientIP]++
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.sessions, sess)
			if s.perIP[sess.clientIP]--; s.perIP[sess.clientIP] <= 0 {
				delete(s.perIP, sess.clientIP)
			}
			s.mu.Unlock()
			<-s.sem
			s.wg.Done()
		}()
		sess.serve()
	}()
}

// reject answers line and closes conn, without blocking the accept loop.
func (s *Server) reject(conn net.Conn, line string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = conn.Write([]byte(line + "\r\n"))
		_ = conn.Close()
	}()
}

// Shutdown stops accepting connections, tells idle sessions to go away
// (421) and waits for sessions that are storing a message to finish. When
// ctx ends first, the remaining connections are closed and ctx's error is
// returned. Shutdown may be called before or without Serve.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	ln := s.ln
	sessions := make([]*session, 0, len(s.sessions))
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, sess := range sessions {
		sess.interrupt()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for sess := range s.sessions {
			sess.forceClose()
		}
		s.mu.Unlock()
		<-done
		return fmt.Errorf("mailgw: shutdown: sessions closed: %w", ctx.Err())
	}
}
