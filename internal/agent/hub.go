// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
)

// Defaults of [Options].
const (
	// DefaultSendTimeout bounds one Send: the time for the agent to poll,
	// send and report. It is also Capabilities.SendTimeout, from which the
	// dispatcher sizes its lease.
	DefaultSendTimeout = 30 * time.Second
	// DefaultStaleAfter: an agent whose last check-in is older is down.
	DefaultStaleAfter = 90 * time.Second
	// DefaultMaxSegments is the longest message one Send may carry.
	DefaultMaxSegments = 10
	// DefaultMaxConcurrentSends per channel.
	DefaultMaxConcurrentSends = 1
	// DefaultMaxInbox bounds the buffered inbound SMS per port.
	DefaultMaxInbox = 500
	// DefaultMaxInboxTotal bounds the buffered inbound SMS per device.
	DefaultMaxInboxTotal = 2000
	// DefaultResultTTL is how long final send results and pending inbox
	// deletions are remembered (deduplication of redelivered commands).
	DefaultResultTTL = 24 * time.Hour
	// DefaultMaxWait caps the long-poll of POST /agent/v1/commands; it
	// stays well below the HTTP server's write timeout.
	DefaultMaxWait = 25 * time.Second
	// DefaultMaxCommands is the most commands one poll returns.
	DefaultMaxCommands = 10

	// maxResults bounds the remembered results per device.
	maxResults = 10000
	// defaultMaxPendingDeletes bounds the inbox deletions waiting for the
	// agent.
	defaultMaxPendingDeletes     = 10000
	defaultMaxDeletesPerResponse = 50
)

// Sentinel causes wrapped into connector errors; test with errors.Is.
var (
	// ErrNoCheckin: the agent has not checked in since tikker started.
	ErrNoCheckin = errors.New("agent has not checked in yet")
	// ErrStale: the agent's last check-in is older than StaleAfter.
	ErrStale = errors.New("agent is stale")
	// ErrNotFetched: the agent did not fetch the command before the send
	// timed out; nothing was sent.
	ErrNotFetched = errors.New("agent did not fetch the command")
	// ErrNoResult: the agent fetched the command but did not report a
	// result in time; the SMS may have been sent.
	ErrNoResult = errors.New("agent did not report a result")
	// ErrRedelivered: the agent reported a result for a command it had
	// been handed more than once; the SMS may have been sent twice.
	ErrRedelivered = errors.New("command was redelivered")
	// ErrUnknownPort: the agent did not report the channel's port.
	ErrUnknownPort = errors.New("port not reported by the agent")
	// ErrInvalidMessage: the message cannot be sent at all.
	ErrInvalidMessage = errors.New("invalid message")
	// ErrTooManyDeletes: too many stored inbox entries wait for the agent
	// to delete them; the rest stays buffered until it catches up.
	ErrTooManyDeletes = errors.New("too many inbox deletions pending")
	// ErrClosed: the connector was closed.
	ErrClosed = errors.New("connector closed")
)

// Operation names used in domain.ConnectorError.Op.
const (
	opSend        = "send"
	opFetchInbox  = "fetch_inbox"
	opDeleteInbox = "delete_inbox"
	opHealth      = "health"
	opProbe       = "probe"
)

// Options tune the hub and its connectors; the zero value is valid.
type Options struct {
	// Logger receives logs (default slog.Default()). Message bodies and
	// tokens are never logged; numbers are masked.
	Logger *slog.Logger
	// Clock drives check-in ages, timeouts and timestamps (default
	// clock.System()).
	Clock clock.Clock
	// Location is the time zone of inbox timestamps (default UTC).
	Location *time.Location
	// SendTimeout bounds one Send (default DefaultSendTimeout).
	SendTimeout time.Duration
	// StaleAfter is the check-in age after which the agent counts as down
	// (default DefaultStaleAfter).
	StaleAfter time.Duration
	// Encodings the router script can send (default GSM-7 and UCS-2).
	Encodings []domain.Encoding
	// MaxSegments per Send (default DefaultMaxSegments); 1 disables
	// concatenation.
	MaxSegments int
	// MaxConcurrentSends per channel (default DefaultMaxConcurrentSends).
	MaxConcurrentSends int
	// MaxInbox bounds buffered inbound SMS per port (default
	// DefaultMaxInbox); further entries are refused and stay on the router.
	MaxInbox int
	// MaxInboxTotal bounds buffered inbound SMS per device over all ports
	// (default DefaultMaxInboxTotal). Entries are also refused for ports
	// the agent did not report at its last check-in.
	MaxInboxTotal int
	// ResultTTL is how long final results and pending inbox deletions are
	// kept (default DefaultResultTTL).
	ResultTTL time.Duration
	// MaxWait caps the long-poll of POST /agent/v1/commands (default
	// DefaultMaxWait).
	MaxWait time.Duration
	// MaxCommands is the most commands one poll returns (default
	// DefaultMaxCommands).
	MaxCommands int
	// RedeliverAfter hides a command handed to the agent from further polls
	// for this long (default SendTimeout/2), so that overlapping polls do
	// not both get it. After it, the command is handed out again (lost
	// response, crashed script); an outcome reported for a command handed
	// out more than once is flagged as a possible duplicate.
	RedeliverAfter time.Duration

	// maxPendingDeletes bounds the inbox deletions waiting for the agent
	// (default defaultMaxPendingDeletes; settable by tests).
	maxPendingDeletes int
	// maxDeletesPerResponse bounds inbox_delete in one response (default 50).
	maxDeletesPerResponse int
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.SendTimeout <= 0 {
		o.SendTimeout = DefaultSendTimeout
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = DefaultStaleAfter
	}
	if len(o.Encodings) == 0 {
		o.Encodings = []domain.Encoding{domain.EncodingGSM7, domain.EncodingUCS2}
	}
	if o.MaxSegments <= 0 {
		o.MaxSegments = DefaultMaxSegments
	}
	if o.MaxConcurrentSends <= 0 {
		o.MaxConcurrentSends = DefaultMaxConcurrentSends
	}
	if o.MaxInbox <= 0 {
		o.MaxInbox = DefaultMaxInbox
	}
	if o.MaxInboxTotal <= 0 {
		o.MaxInboxTotal = DefaultMaxInboxTotal
	}
	if o.ResultTTL <= 0 {
		o.ResultTTL = DefaultResultTTL
	}
	if o.MaxWait <= 0 {
		o.MaxWait = DefaultMaxWait
	}
	if o.MaxCommands <= 0 {
		o.MaxCommands = DefaultMaxCommands
	}
	if o.maxPendingDeletes <= 0 {
		o.maxPendingDeletes = defaultMaxPendingDeletes
	}
	if o.maxDeletesPerResponse <= 0 {
		o.maxDeletesPerResponse = defaultMaxDeletesPerResponse
	}
	if o.RedeliverAfter <= 0 {
		o.RedeliverAfter = max(o.SendTimeout/2, time.Second)
	}
	return o
}

// capabilities of the agent connector. The script sends single GSM-7 SMS
// with /tool sms send and everything else as the SMS-SUBMIT PDUs tikker
// builds (BuildSubmitPDUs). [WERYFIKACJA: PDU mode through at-chat, T51]
func (o Options) capabilities() connector.Capabilities {
	return connector.Capabilities{
		Encodings:          slices.Clone(o.Encodings),
		MaxSegments:        o.MaxSegments,
		Concatenation:      o.MaxSegments > 1,
		MaxConcurrentSends: o.MaxConcurrentSends,
		SendTimeout:        o.SendTimeout,
		InboundPDU:         true,
	}
}

// Hub holds the in-memory mailboxes of all agent devices. Connectors built
// by [Hub.Factory] and the HTTP handler of [Hub.Handler] share them: the
// connector side is driven by the queue, the inbox poller and the health
// checker, the handler side by the router script. It is safe for concurrent
// use.
type Hub struct {
	opts Options
	log  *slog.Logger
	clk  clock.Clock

	mu       sync.Mutex
	sessions map[domain.ID]*session
}

// NewHub returns an empty hub.
func NewHub(opts Options) *Hub {
	opts = opts.withDefaults()
	return &Hub{
		opts: opts, clk: opts.Clock, sessions: map[domain.ID]*session{},
		log: opts.Logger.With(slog.String("connector", string(domain.ConnectorAgent))),
	}
}

// Factory returns the connector.Factory of the agent connector.
func (h *Hub) Factory() connector.Factory {
	return func(ctx context.Context, cfg connector.DeviceConfig) (connector.Connector, error) {
		c, err := h.NewConn(ctx, cfg)
		if err != nil {
			return nil, err // not a typed nil inside the interface
		}
		return c, nil
	}
}

// Register adds the agent factory to reg.
func (h *Hub) Register(reg *connector.Registry) error {
	return reg.Register(domain.ConnectorAgent, h.Factory())
}

// NewConn builds a connector for the agent device of cfg. Every connector of
// one device shares the device's mailbox; the mailbox lives while at least
// one of them is open. The newest connector's token is the one the agent
// must present.
func (h *Hub) NewConn(_ context.Context, cfg connector.DeviceConfig) (*Conn, error) {
	if cfg.Kind != "" && cfg.Kind != domain.ConnectorAgent {
		return nil, fmt.Errorf("agent connector for device %s: wrong kind %q", cfg.DeviceID, cfg.Kind)
	}
	if cfg.DeviceID.IsZero() {
		return nil, errors.New("agent connector: empty device id")
	}
	if cfg.AgentToken == "" {
		return nil, fmt.Errorf("agent connector for device %s: no agent token configured", cfg.DeviceID)
	}
	s := h.acquire(cfg.DeviceID, cfg.AgentToken)
	return &Conn{
		hub: h, s: s, caps: h.opts.capabilities(), clk: h.clk,
		log:   h.log.With(slog.String("device_id", cfg.DeviceID.String())),
		ports: map[string]chan struct{}{},
	}, nil
}

func tokenHash(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

func (h *Hub) acquire(id domain.ID, token string) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[id]
	if s == nil {
		s = newSession(h, id)
		h.sessions[id] = s
	}
	s.refs++
	s.token = tokenHash(token)
	return s
}

func (h *Hub) release(s *session) {
	h.mu.Lock()
	s.refs--
	last := s.refs == 0
	if last && h.sessions[s.deviceID] == s {
		delete(h.sessions, s.deviceID)
	}
	h.mu.Unlock()
	if last {
		s.shutdown()
	}
}

// authenticate returns the session whose token is token, or nil. Every
// session is compared, in constant time per comparison, so the time taken
// does not depend on which token (if any) matched or how much of it was
// right. A token shared by two devices matches neither.
func (h *Hub) authenticate(token string) *session {
	if token == "" {
		return nil
	}
	sum := tokenHash(token)
	h.mu.Lock()
	defer h.mu.Unlock()
	var found *session
	matches := 0
	for _, s := range h.sessions {
		if subtle.ConstantTimeCompare(s.token[:], sum[:]) == 1 {
			found = s
			matches++
		}
	}
	if matches > 1 {
		h.log.Warn("agent token is configured on several devices; rejecting it")
		return nil
	}
	return found
}

// ---------------------------------------------------------------------------
// Session: the mailbox of one device.

type resultStatus string

// Result statuses reported by the agent.
const (
	statusAccepted  resultStatus = "accepted"
	statusTransient resultStatus = "transient"
	statusAmbiguous resultStatus = "ambiguous"
	statusPermanent resultStatus = "permanent"
)

func (s resultStatus) valid() bool {
	switch s {
	case statusAccepted, statusTransient, statusAmbiguous, statusPermanent:
		return true
	}
	return false
}

// final results are remembered: a redelivered command with the same
// message ID gets them without reaching the router again.
func (s resultStatus) final() bool { return s == statusAccepted || s == statusPermanent }

type outcome struct {
	status    resultStatus
	reason    string // sanitized
	remoteRef string
	at        time.Time
	// deliveries is how often the command was handed to the agent before
	// this outcome was reported (0 for remembered results). More than one
	// means the agent may have executed it twice.
	deliveries int
}

type command struct {
	id         domain.ID
	port       string
	msg        connector.OutboundSMS
	pdus       []SubmitPDU
	enqueuedAt time.Time
	delivered  int       // times handed to the agent
	hiddenTill time.Time // not handed out again before (redelivery lease)
	waiters    int       // Sends waiting for the result
	done       chan struct{}
	out        outcome // set before done is closed
}

type inboxKey struct{ port, remoteID string }

// inboxEntry is one buffered inbound SMS. sms.RemoteID is the router's ID;
// the connector hands out handle instead (see handleOf).
type inboxEntry struct {
	sms    connector.InboundSMS
	fp     [sha256.Size]byte
	handle string
}

// deleteKey identifies one stored entry whose deletion on the router is
// pending. The fingerprint is part of the key: RouterOS reuses IDs, so two
// different messages may have had the same ID.
type deleteKey struct {
	inboxKey
	fp [sha256.Size]byte
}

// deleteMark carries what the agent compares before deleting an entry: it
// removes the entry only if phone and timestamp still match.
type deleteMark struct {
	phone, timestamp string
	at               time.Time
}

// report is the last check-in.
type report struct {
	at           time.Time
	version      string
	board        string
	agentVersion string
	ports        []connector.PortInfo
}

type session struct {
	hub      *Hub
	deviceID domain.ID
	refs     int               // guarded by hub.mu
	token    [sha256.Size]byte // guarded by hub.mu

	mu       sync.Mutex
	closed   bool
	done     chan struct{} // closed by shutdown
	wake     chan struct{} // closed and replaced when a command is queued
	queue    []*command    // unresolved commands, oldest first
	byID     map[domain.ID]*command
	results  map[domain.ID]outcome // final results, for deduplication
	order    []domain.ID           // results in insertion order
	inbox    map[string][]inboxEntry
	deletes  map[deleteKey]deleteMark
	lastSeen time.Time
	last     *report
}

func newSession(h *Hub, id domain.ID) *session {
	return &session{
		hub: h, deviceID: id, done: make(chan struct{}), wake: make(chan struct{}),
		byID: map[domain.ID]*command{}, results: map[domain.ID]outcome{},
		inbox: map[string][]inboxEntry{}, deletes: map[deleteKey]deleteMark{},
	}
}

func (s *session) now() time.Time { return s.hub.clk.Now() }

// shutdown wakes every waiting Send and long-poll; the session is gone.
func (s *session) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
}

// touch records that the agent was seen.
func (s *session) touch() {
	s.mu.Lock()
	s.lastSeen = s.now()
	s.mu.Unlock()
}

// enqueue adds a command for msg, or joins the unresolved one with the same
// message ID. A remembered final result is returned instead (ok true).
func (s *session) enqueue(port string, msg connector.OutboundSMS, pdus []SubmitPDU) (cmd *command, res outcome, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, outcome{}, false, ErrClosed
	}
	s.pruneLocked()
	if r, found := s.results[msg.MessageID]; found {
		return nil, r, true, nil
	}
	if c := s.byID[msg.MessageID]; c != nil {
		c.waiters++
		return c, outcome{}, false, nil
	}
	c := &command{
		id: msg.MessageID, port: port, msg: msg, pdus: pdus,
		enqueuedAt: s.now(), waiters: 1, done: make(chan struct{}),
	}
	s.queue = append(s.queue, c)
	s.byID[c.id] = c
	close(s.wake)
	s.wake = make(chan struct{})
	return c, outcome{}, false, nil
}

// abandon is called by a Send that stops waiting for cmd. It returns the
// outcome if cmd was resolved meanwhile (resolved true). Otherwise it
// reports whether the agent had fetched the command; the last waiter removes
// an unresolved command, so it is not delivered any more (the agent
// deduplicates a later redelivery by message ID).
func (s *session) abandon(cmd *command) (out outcome, resolved, fetched bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-cmd.done:
		return cmd.out, true, true
	default:
	}
	cmd.waiters--
	if cmd.waiters <= 0 {
		s.removeLocked(cmd)
	}
	return outcome{}, false, cmd.delivered > 0
}

func (s *session) removeLocked(cmd *command) {
	if s.byID[cmd.id] == cmd {
		delete(s.byID, cmd.id)
	}
	if i := slices.Index(s.queue, cmd); i >= 0 {
		s.queue = slices.Delete(s.queue, i, i+1)
	}
}

// pending returns up to max unresolved commands for ports (all ports when
// empty) and marks them delivered. A delivered command is hidden from
// further polls for RedeliverAfter, so overlapping polls do not both get it;
// then it is handed out again until the agent reports a result (a lost
// response or a crashed script). The agent drops commands it has already
// executed by message ID; if it does not, the outcome of a command handed
// out more than once is flagged as a possible duplicate (see Conn.outcome).
func (s *session) pending(ports []string, limit int) ([]*command, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []*command
	for _, c := range s.queue {
		if len(out) >= limit {
			break
		}
		if len(ports) > 0 && !slices.Contains(ports, c.port) {
			continue
		}
		if now.Before(c.hiddenTill) {
			continue
		}
		c.delivered++
		c.hiddenTill = now.Add(s.hub.opts.RedeliverAfter)
		out = append(out, c)
	}
	return out, s.wake
}

// resolve records the agent's result for id. known reports whether a Send
// was waiting for it. Final results are remembered even for unknown IDs: a
// command redelivered after a timeout or a restart of tikker then completes
// without reaching the router again.
func (s *session) resolve(id domain.ID, out outcome) (known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if out.status.final() {
		if _, dup := s.results[id]; !dup {
			s.order = append(s.order, id)
		}
		s.results[id] = out
		s.pruneLocked()
	}
	c := s.byID[id]
	if c == nil {
		return false
	}
	s.removeLocked(c)
	c.out = out
	c.out.deliveries = c.delivered
	close(c.done)
	return true
}

// pruneLocked forgets results and inbox deletions older than ResultTTL and
// keeps both below their caps.
func (s *session) pruneLocked() {
	cutoff := s.now().Add(-s.hub.opts.ResultTTL)
	drop := 0
	for drop < len(s.order) {
		r, ok := s.results[s.order[drop]]
		if ok && r.at.After(cutoff) && len(s.order)-drop <= maxResults {
			break
		}
		delete(s.results, s.order[drop])
		drop++
	}
	s.order = s.order[drop:]
	for k, m := range s.deletes {
		if !m.at.After(cutoff) {
			delete(s.deletes, k)
		}
	}
}

// ---------------------------------------------------------------------------
// Inbox

// push adds inbound SMS reported by the agent. An entry is identified by
// port, router ID and content fingerprint. An entry tikker has already
// stored (its deletion is pending) or still buffers is skipped. An entry
// with a known router ID but different content is a new message (RouterOS
// reuses IDs after deletion): it is buffered next to the old one, which
// stays until it is stored, and a pending deletion of the old content under
// that ID is dropped (that entry is gone from the router). Entries for a
// port the last check-in did not report (or before any check-in) are
// refused, as are entries beyond MaxInbox per port or MaxInboxTotal per
// device; refused entries stay on the router and are pushed again later.
// It returns how many entries were taken and refused, and how many of the
// refused ones were for unknown ports.
func (s *session) push(entries []pushed) (accepted, refused, unknownPort int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit, total := s.hub.opts.MaxInbox, 0
	for _, l := range s.inbox {
		total += len(l)
	}
	known := map[string]bool{}
	if s.last != nil {
		for _, p := range s.last.ports {
			known[p.Port] = true
		}
	}
	for _, p := range entries {
		if !known[p.port] {
			refused++
			unknownPort++
			continue
		}
		e := p.sms
		key := inboxKey{port: p.port, remoteID: e.RemoteID}
		fp := fingerprint(e)
		if _, ok := s.deletes[deleteKey{key, fp}]; ok {
			accepted++ // already stored; the agent learns to delete it
			continue
		}
		for k := range s.deletes {
			if k.inboxKey == key {
				delete(s.deletes, k)
			}
		}
		list := s.inbox[p.port]
		if slices.ContainsFunc(list, func(x inboxEntry) bool { return x.sms.RemoteID == e.RemoteID && x.fp == fp }) {
			accepted++
			continue
		}
		if len(list) >= limit || total >= s.hub.opts.MaxInboxTotal {
			refused++
			continue
		}
		s.inbox[p.port] = append(list, inboxEntry{sms: e, fp: fp, handle: handleOf(e.RemoteID, fp)})
		total++
		accepted++
	}
	return accepted, refused, unknownPort
}

// handleOf is the RemoteID the connector hands to the inbox poller: the
// router's ID plus a short content fingerprint, so that DeleteInbox removes
// exactly the entry FetchInbox returned even if the router has meanwhile
// reused the ID for another message.
func handleOf(remoteID string, fp [sha256.Size]byte) string {
	return remoteID + "@" + hex.EncodeToString(fp[:6])
}

// pushed is one inbound SMS reported by the agent for a port.
type pushed struct {
	port string
	sms  connector.InboundSMS
}

func fingerprint(e connector.InboundSMS) [sha256.Size]byte {
	h := sha256.New()
	for _, f := range []string{e.Phone, e.RawTimestamp, e.PDU, e.Body} {
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (s *session) fetch(port string) ([]connector.InboundSMS, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	list := s.inbox[port]
	out := make([]connector.InboundSMS, len(list))
	for i, e := range list {
		out[i] = e.sms
		out[i].RemoteID = e.handle
	}
	return out, nil
}

// markDeleted moves stored entries, identified by the handles FetchInbox
// returned, to the deletions the agent still has to perform on the router.
// Unknown handles are ignored. When the pending deletions are at their cap,
// the remaining entries stay buffered and ErrTooManyDeletes is returned:
// dropping them unmarked would let the next push add them again.
func (s *session) markDeleted(port string, handles []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	now := s.now()
	list := s.inbox[port]
	full := false
	for _, h := range handles {
		i := slices.IndexFunc(list, func(x inboxEntry) bool { return x.handle == h })
		if i < 0 {
			continue
		}
		e := list[i]
		if len(s.deletes) >= s.hub.opts.maxPendingDeletes {
			full = true
			break
		}
		s.deletes[deleteKey{inboxKey{port: port, remoteID: e.sms.RemoteID}, e.fp}] = deleteMark{
			phone: e.sms.Phone, timestamp: e.sms.RawTimestamp, at: now,
		}
		list = slices.Delete(list, i, i+1)
	}
	if len(list) == 0 {
		delete(s.inbox, port)
	} else {
		s.inbox[port] = list
	}
	if full {
		return fmt.Errorf("%w (%d)", ErrTooManyDeletes, len(s.deletes))
	}
	return nil
}

// pendingDeletes lists the entries the agent may delete on the router,
// sorted. Each carries the phone and timestamp the agent pushed: the agent
// deletes the router entry only if they still match.
func (s *session) pendingDeletes() []inboxRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	type item struct {
		ref inboxRef
		at  time.Time
	}
	all := make([]item, 0, len(s.deletes))
	for k, m := range s.deletes {
		all = append(all, item{inboxRef{Port: k.port, RemoteID: k.remoteID, Phone: m.phone, Timestamp: m.timestamp}, m.at})
	}
	slices.SortFunc(all, func(a, b item) int {
		return cmp.Or(a.at.Compare(b.at), strings.Compare(a.ref.Port, b.ref.Port),
			strings.Compare(a.ref.RemoteID, b.ref.RemoteID),
			strings.Compare(a.ref.Phone, b.ref.Phone), strings.Compare(a.ref.Timestamp, b.ref.Timestamp))
	})
	// Bounded per response: a RouterOS script reads the whole body into a
	// variable, so an unbounded list (acks failing for a while) could grow
	// past what /tool fetch returns and stall the agent. Oldest go first;
	// the rest follow in later responses.
	if n := s.hub.opts.maxDeletesPerResponse; len(all) > n {
		all = all[:n]
	}
	out := make([]inboxRef, len(all))
	for i, it := range all {
		out[i] = it.ref
	}
	return out
}

// acked forgets deletions the agent has performed (or found unnecessary).
// A reference with phone or timestamp forgets only the matching deletions;
// one without (older scripts) forgets every deletion under its router ID.
func (s *session) acked(refs []inboxRef) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range refs {
		legacy := r.Phone == "" && r.Timestamp == ""
		for k, m := range s.deletes {
			if k.port != r.Port || k.remoteID != r.RemoteID {
				continue
			}
			if legacy || (m.phone == r.Phone && m.timestamp == r.Timestamp) {
				delete(s.deletes, k)
				n++
			}
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Check-in

func (s *session) checkin(r report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.at = s.now()
	s.last = &r
	s.lastSeen = r.at
}

// state returns the last check-in and whether it is fresh.
func (s *session) state() (r *report, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return nil, ErrClosed
	case s.last == nil:
		return nil, ErrNoCheckin
	}
	cp := *s.last
	cp.ports = slices.Clone(s.last.ports)
	if age := s.now().Sub(cp.at); age > s.hub.opts.StaleAfter {
		return &cp, fmt.Errorf("%w: last check-in %s ago", ErrStale, age.Round(time.Second))
	}
	return &cp, nil
}

// statusSnapshot is GET /agent/v1/status.
type statusSnapshot struct {
	lastCheckin     time.Time
	lastSeen        time.Time
	pendingCommands int
	inboxEntries    int
	pendingDeletes  int
}

func (s *session) snapshot() statusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := statusSnapshot{lastSeen: s.lastSeen, pendingCommands: len(s.queue), pendingDeletes: len(s.deletes)}
	if s.last != nil {
		st.lastCheckin = s.last.at
	}
	for _, l := range s.inbox {
		st.inboxEntries += len(l)
	}
	return st
}
