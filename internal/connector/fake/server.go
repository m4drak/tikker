// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package fake emulates the part of the RouterOS 7 REST API that tikker uses
// (/rest/tool/sms/*, /rest/interface/lte, /rest/system/*) so that connector,
// queue, poller and end-to-end tests run without hardware (PLAN T13).
//
// Behaviour follows docs/research/routeros-sms.md. Wherever the real router
// has not been measured yet (T00) the code carries an "ASSUMPTION(T00):"
// comment so it can be corrected in one place after the measurements.
//
// A Server is an http.Handler. Tests normally call Start (httptest) and then
// steer it through the control methods (FailNext, InjectSMS, SetLTE, Sent, ...);
// cmd/fake-routeros exposes the same controls over HTTP (ControlHandler).
package fake

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// TimestampISO formats like "2026-11-19 12:00:00", the default for
// Options.FormatTimestamp.
//
// ASSUMPTION(T00): the real inbox timestamp format is unknown (W8). The ISO
// form is the default; TimestampLegacy is offered so a parser can be
// exercised against both.
func TimestampISO(t time.Time) string { return t.Format("2006-01-02 15:04:05") }

// TimestampLegacy formats like the RouterOS console, e.g. "nov/19/2026 12:00:00".
func TimestampLegacy(t time.Time) string {
	return strings.ToLower(t.Format("Jan")) + t.Format("/02/2006 15:04:05")
}

// Options configure a Server. The zero value works.
type Options struct {
	// User and Password are the accepted basic-auth credentials
	// (default "tikker" / "tikker-test").
	User, Password string
	// Ports are the LTE interfaces (default "lte1").
	Ports []string
	// Lenient accepts texts that real RouterOS rejects or mangles: more than
	// 160 septets and non-GSM-7 characters.
	Lenient bool
	// Version is /system/resource "version" (default "7.16.2 (stable)").
	Version string
	// Model is /system/routerboard "model" (default "FakeBoard LTE").
	Model string
	// SendDelay is how long every send holds the modem port.
	SendDelay time.Duration
	// SessionTimeout is the REST per-request limit (default 60 s, research
	// W7). A send or hang running longer ends with 400 "Session closed".
	SessionTimeout time.Duration
	// InboxLimit is the inbox capacity; 0 means unlimited.
	InboxLimit int
	// AutoErase is the initial /tool sms auto-erase value: when the inbox is
	// full the oldest message is dropped instead of the new one.
	AutoErase bool
	// Now supplies the clock for default timestamps (default time.Now).
	Now func() time.Time
	// FormatTimestamp renders the inbox "timestamp" field (default TimestampISO).
	FormatTimestamp func(time.Time) string
	// InboxPortField adds a "port" field to inbox entries.
	// ASSUMPTION(T00): real entries probably carry no port field.
	InboxPortField bool
	// SMSC is the service-centre number put into built PDUs
	// (default "+48000000001").
	SMSC string
}

// Policies model the RouterOS user group (research W2).
type Policies struct {
	// RESTAPI false answers 401 to every call.
	RESTAPI bool
	// API false answers 500 "not allowed (9)" to every call.
	API bool
}

// Settings mirror /tool sms (GET /rest/tool/sms).
type Settings struct {
	Port                   string
	ReceiveEnabled         bool
	AutoErase              bool
	RemoveSentSMSAfterSend bool
	SMSStorage             string
	Polling                bool
}

// Server is the fake router. Create it with New.
type Server struct {
	opts Options

	mu             sync.Mutex
	user, pass     string
	lenient        bool
	sendDelay      time.Duration
	sessionTimeout time.Duration
	inboxLimit     int
	settings       Settings
	policies       Policies
	lte            map[string]*LTEState
	inbox          []InboxEntry
	nextInboxID    int
	nextRef        uint16
	sent           []SentMessage
	faults         map[Endpoint][]*faultEntry
	requests       map[Endpoint]int
	authFailures   int

	portSem map[string]chan struct{}
	done    chan struct{}
	once    sync.Once
	ts      *httptest.Server
}

// New builds a Server in its initial state.
func New(opts Options) *Server {
	if opts.User == "" {
		opts.User = "tikker"
	}
	if opts.Password == "" {
		opts.Password = "tikker-test"
	}
	if len(opts.Ports) == 0 {
		opts.Ports = []string{"lte1"}
	}
	if opts.Version == "" {
		opts.Version = "7.16.2 (stable)"
	}
	if opts.Model == "" {
		opts.Model = "FakeBoard LTE"
	}
	if opts.SessionTimeout <= 0 {
		opts.SessionTimeout = 60 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.FormatTimestamp == nil {
		opts.FormatTimestamp = TimestampISO
	}
	if opts.SMSC == "" {
		opts.SMSC = "+48000000001"
	}
	s := &Server{
		opts:    opts,
		portSem: map[string]chan struct{}{},
		done:    make(chan struct{}),
	}
	for _, p := range opts.Ports {
		s.portSem[p] = make(chan struct{}, 1)
	}
	s.mu.Lock()
	s.resetLocked()
	s.mu.Unlock()
	return s
}

func (s *Server) resetLocked() {
	o := s.opts
	s.user, s.pass = o.User, o.Password
	s.lenient = o.Lenient
	s.sendDelay = o.SendDelay
	s.sessionTimeout = o.SessionTimeout
	s.inboxLimit = o.InboxLimit
	s.settings = Settings{
		Port: o.Ports[0], ReceiveEnabled: true, AutoErase: o.AutoErase,
		SMSStorage: "modem",
	}
	s.policies = Policies{RESTAPI: true, API: true}
	s.lte = map[string]*LTEState{}
	for _, p := range o.Ports {
		s.lte[p] = defaultLTE()
	}
	s.inbox = nil
	s.nextInboxID = 1
	s.nextRef = 1
	s.sent = nil
	s.faults = map[Endpoint][]*faultEntry{}
	s.requests = map[Endpoint]int{}
	s.authFailures = 0
}

// Reset returns the server to its initial state: empty inbox and sent log,
// no faults, zero counters, default LTE state, settings and credentials.
// Calls already in flight are not interrupted.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetLocked()
}

// Start serves the fake over plain HTTP on a free local port (httptest) and
// returns its base URL, e.g. "http://127.0.0.1:41234".
func (s *Server) Start() string {
	s.ts = httptest.NewServer(s)
	return s.ts.URL
}

// StartTLS is Start over HTTPS with a throw-away certificate. Use
// Client() to talk to it and Fingerprint() to pin it.
func (s *Server) StartTLS() string {
	s.ts = httptest.NewTLSServer(s)
	return s.ts.URL
}

// Client returns an HTTP client that trusts the StartTLS certificate.
func (s *Server) Client() *http.Client { return s.ts.Client() }

// Fingerprint returns the SHA-256 of the StartTLS leaf certificate in the
// colon-separated upper-case form of FormatFingerprint.
func (s *Server) Fingerprint() string {
	sum := sha256.Sum256(s.ts.Certificate().Raw)
	return FormatFingerprint(sum[:])
}

// Close stops the HTTP server (if started) and releases waiting requests.
func (s *Server) Close() {
	s.once.Do(func() { close(s.done) })
	if s.ts != nil {
		s.ts.Close()
	}
}

// sleep waits d; false means the server was closed meanwhile.
func (s *Server) sleep(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.done:
		return false
	}
}

// SetSendDelay sets how long each send holds its modem port.
func (s *Server) SetSendDelay(d time.Duration) {
	s.mu.Lock()
	s.sendDelay = d
	s.mu.Unlock()
}

// SetSessionTimeout sets the REST request limit (default 60 s).
func (s *Server) SetSessionTimeout(d time.Duration) {
	s.mu.Lock()
	s.sessionTimeout = d
	s.mu.Unlock()
}

// SetLenient switches between strict (RouterOS-like) and lenient send checks.
func (s *Server) SetLenient(v bool) {
	s.mu.Lock()
	s.lenient = v
	s.mu.Unlock()
}

// SetCredentials changes the accepted basic-auth credentials.
func (s *Server) SetCredentials(user, password string) {
	s.mu.Lock()
	s.user, s.pass = user, password
	s.mu.Unlock()
}

// SetPolicies changes the user policies (see Policies).
func (s *Server) SetPolicies(p Policies) {
	s.mu.Lock()
	s.policies = p
	s.mu.Unlock()
}

// SetSettings edits the /tool sms settings under the server lock.
func (s *Server) SetSettings(edit func(*Settings)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	edit(&s.settings)
}

// Settings returns a copy of the /tool sms settings.
func (s *Server) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// Requests returns how many authenticated calls reached ep (including those
// that were failed on purpose). EndpointAny returns the total.
func (s *Server) Requests(ep Endpoint) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ep == EndpointAny {
		n := 0
		for _, c := range s.requests {
			n += c
		}
		return n
	}
	return s.requests[ep]
}

// AllRequests returns a copy of the per-endpoint counters.
func (s *Server) AllRequests() map[Endpoint]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Endpoint]int, len(s.requests))
	for k, v := range s.requests {
		out[k] = v
	}
	return out
}

// AuthFailures returns how many calls were rejected for credentials or policy.
func (s *Server) AuthFailures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authFailures
}

// ports returns the configured LTE port names in order.
func (s *Server) ports() []string { return s.opts.Ports }
