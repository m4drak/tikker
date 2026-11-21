// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// tokenOwner is the creating token of test messages.
const tokenOwner domain.ID = "01JZZZZZZZZZZZZZZZZZZZZZZZ"

// waitLimit is a real-time guard that turns a deadlock into a test failure.
const waitLimit = 20 * time.Second

// ---------------------------------------------------------------------------
// Sleep tracker

// tracker records which goroutines (workers by channel ID, the janitor) are
// sleeping. Tests wait on it instead of using time.Sleep.
type tracker struct {
	mu       sync.Mutex
	cond     *sync.Cond
	sleeping map[domain.ID]time.Duration
	count    map[domain.ID]int // sleeps started
	timedOut bool
}

func newTracker() *tracker {
	tr := &tracker{sleeping: map[domain.ID]time.Duration{}, count: map[domain.ID]int{}}
	tr.cond = sync.NewCond(&tr.mu)
	return tr
}

func (tr *tracker) hooks() hooks {
	return hooks{
		sleep: func(id domain.ID, d time.Duration) {
			tr.mu.Lock()
			tr.sleeping[id] = d
			tr.count[id]++
			tr.cond.Broadcast()
			tr.mu.Unlock()
		},
		wake: func(id domain.ID) {
			tr.mu.Lock()
			delete(tr.sleeping, id)
			tr.cond.Broadcast()
			tr.mu.Unlock()
		},
	}
}

// view is what wait conditions see.
type view struct {
	sleeping map[domain.ID]time.Duration
	count    map[domain.ID]int
}

func (v view) asleep(id domain.ID) bool { _, ok := v.sleeping[id]; return ok }

func (v view) sleepsFor(id domain.ID, d time.Duration) bool {
	got, ok := v.sleeping[id]
	return ok && got == d
}

// wait blocks until cond holds. cond is re-evaluated whenever a tracked
// goroutine goes to sleep or wakes up.
func (tr *tracker) wait(t *testing.T, desc string, cond func(v view) bool) {
	t.Helper()
	guard := time.AfterFunc(waitLimit, func() {
		tr.mu.Lock()
		tr.timedOut = true
		tr.cond.Broadcast()
		tr.mu.Unlock()
	})
	defer guard.Stop()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for !cond(view{tr.sleeping, tr.count}) {
		if tr.timedOut {
			tr.timedOut = false
			t.Fatalf("timed out waiting for: %s (sleeping: %v)", desc, tr.sleeping)
		}
		tr.cond.Wait()
	}
}

func (tr *tracker) sleeps(id domain.ID) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.count[id]
}

// ---------------------------------------------------------------------------
// Stub connector

type stubCall struct {
	ChannelID domain.ID
	Port      string
	MessageID domain.ID
	To        string
	At        time.Time
}

// stubConn is a scripted connector.Connector.
type stubConn struct {
	clk  clock.Clock
	caps connector.Capabilities

	mu    sync.Mutex
	calls []stubCall
	fn    func(ctx context.Context, n int, c stubCall) (connector.SendResult, error)
}

func newStub(clk clock.Clock, fn func(ctx context.Context, n int, c stubCall) (connector.SendResult, error)) *stubConn {
	return &stubConn{
		clk: clk,
		caps: connector.Capabilities{
			Encodings: []domain.Encoding{domain.EncodingGSM7}, MaxSegments: 1,
			MaxConcurrentSends: 1, SendTimeout: 10 * time.Second,
		},
		fn: fn,
	}
}

func (s *stubConn) Capabilities() connector.Capabilities { return s.caps }

func (s *stubConn) Send(ctx context.Context, ch connector.ChannelRef, msg connector.OutboundSMS) (connector.SendResult, error) {
	c := stubCall{ChannelID: ch.ChannelID, Port: ch.Port, MessageID: msg.MessageID, To: msg.To, At: s.clk.Now()}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	n := len(s.calls)
	fn := s.fn
	s.mu.Unlock()
	if fn == nil {
		return connector.SendResult{}, nil
	}
	return fn(ctx, n, c)
}

func (s *stubConn) FetchInbox(context.Context, connector.ChannelRef) ([]connector.InboundSMS, error) {
	return nil, nil
}

func (s *stubConn) DeleteInbox(context.Context, connector.ChannelRef, []string) error { return nil }

func (s *stubConn) Health(context.Context, connector.ChannelRef) (connector.ChannelHealth, error) {
	return connector.ChannelHealth{}, nil
}

func (s *stubConn) Probe(context.Context) (connector.DeviceProbe, error) {
	return connector.DeviceProbe{}, nil
}

func (s *stubConn) Close() error { return nil }

func (s *stubConn) Calls() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

func (s *stubConn) N() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// gate makes sends block until released (or their context ends).
type gate struct {
	entered chan stubCall
	release chan error
}

func newGate() *gate {
	return &gate{entered: make(chan stubCall, 16), release: make(chan error, 16)}
}

func (g *gate) send(ctx context.Context, _ int, c stubCall) (connector.SendResult, error) {
	g.entered <- c
	select {
	case err := <-g.release:
		return connector.SendResult{}, err
	case <-ctx.Done():
		return connector.SendResult{}, ctx.Err()
	}
}

func (g *gate) waitEntered(t *testing.T) stubCall {
	t.Helper()
	select {
	case c := <-g.entered:
		return c
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a send")
		return stubCall{}
	}
}

// ---------------------------------------------------------------------------
// Environment

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type eventLog struct {
	mu  sync.Mutex
	evs []Event
}

func (e *eventLog) add(ev Event) {
	e.mu.Lock()
	e.evs = append(e.evs, ev)
	e.mu.Unlock()
}

func (e *eventLog) kinds() []EventKind {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]EventKind, len(e.evs))
	for i, ev := range e.evs {
		out[i] = ev.Kind
	}
	return out
}

func (e *eventLog) all() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.evs...)
}

type envConfig struct {
	path string      // database file; empty: in memory
	clk  *clock.Fake // default: new fake at t0
	opts func(*Options)
	repo func(store.MessageRepo) store.MessageRepo // wrap the repo
}

type env struct {
	t      *testing.T
	ctx    context.Context
	clk    *clock.Fake
	st     *sqlite.Store
	repo   store.MessageRepo
	d      *Dispatcher
	tr     *tracker
	logs   *logBuf
	events *eventLog
	ids    *domain.ULIDGenerator

	relMu    sync.Mutex
	released []connector.Connector
}

func newEnv(t *testing.T, cfg envConfig) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), clk: cfg.clk, tr: newTracker(), logs: &logBuf{}, events: &eventLog{}}
	if e.clk == nil {
		e.clk = clock.NewFake(t0)
	}
	e.ids = domain.NewULIDGenerator(e.clk)
	st, err := sqlite.Open(e.ctx, sqlite.Options{Path: cfg.path, Clock: e.clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st
	e.repo = st.Messages()
	if cfg.repo != nil {
		e.repo = cfg.repo(e.repo)
	}
	opts := Options{
		Messages: e.repo,
		Clock:    e.clk,
		Logger:   slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Rand:     func() float64 { return 0.5 }, // no jitter
		OnEvent:  e.events.add,
		OnConnectorReleased: func(c connector.Connector) {
			e.relMu.Lock()
			e.released = append(e.released, c)
			e.relMu.Unlock()
		},
	}
	if cfg.opts != nil {
		cfg.opts(&opts)
	}
	d, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	d.hooks = e.tr.hooks()
	e.d = d
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
		defer cancel()
		_ = d.Shutdown(ctx)
	})
	return e
}

func filePath(t *testing.T) string { return filepath.Join(t.TempDir(), "tikker.db") }

func (e *env) start() {
	e.t.Helper()
	if err := e.d.Start(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) shutdown() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	if err := e.d.Shutdown(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) releasedConns() []connector.Connector {
	e.relMu.Lock()
	defer e.relMu.Unlock()
	return append([]connector.Connector(nil), e.released...)
}

func spec(id domain.ID, conn connector.Connector, tags ...string) ChannelSpec {
	return ChannelSpec{
		Channel: domain.Channel{ID: id, Port: "lte1", Name: string(id), Tags: tags,
			Enabled: true, State: domain.ChannelOK, MaxSegments: 1},
		Connector: conn,
	}
}

// msg builds an outbound message due now.
func (e *env) msg(peer string, mods ...func(*domain.Message)) domain.Message {
	now := e.clk.Now()
	m := domain.Message{
		ID: e.ids.NewID(), Peer: peer, Body: "secret body " + peer, Encoding: domain.EncodingGSM7, Segments: 1,
		CreatedBy: domain.TokenActor(tokenOwner), CreatedAt: now, NextAttemptAt: now,
	}
	for _, f := range mods {
		f(&m)
	}
	return m
}

func pinned(ch domain.ID) func(*domain.Message) {
	return func(m *domain.Message) { m.Route.ChannelID = ch }
}

func tagged(tags ...string) func(*domain.Message) {
	return func(m *domain.Message) { m.Route.Tags = tags }
}

// enqueue stores messages and notifies the dispatcher, like the API does.
func (e *env) enqueue(ms ...domain.Message) []domain.Message {
	e.t.Helper()
	res, err := e.st.Messages().Enqueue(e.ctx, ms, store.Idempotency{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.d.Notify()
	return res.Messages
}

func (e *env) get(id domain.ID) domain.Message {
	e.t.Helper()
	m, err := e.st.Messages().Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// status reads a message status without failing the test from another
// goroutine.
func (e *env) status(id domain.ID) domain.MessageStatus {
	m, err := e.st.Messages().Get(e.ctx, id)
	if err != nil {
		return ""
	}
	return m.Status
}

// waitStatus waits until every message has the given status and worker ch
// sleeps again.
func (e *env) waitStatus(ch domain.ID, want domain.MessageStatus, ids ...domain.ID) {
	e.t.Helper()
	e.tr.wait(e.t, "messages "+string(want), func(v view) bool {
		for _, id := range ids {
			if e.status(id) != want {
				return false
			}
		}
		return ch == "" || v.asleep(ch)
	})
}
