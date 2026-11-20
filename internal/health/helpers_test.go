// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package health

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/queue"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// waitLimit is a real-time guard that turns a deadlock into a test failure.
const waitLimit = 20 * time.Second

// *queue.Dispatcher is what Options.Dispatcher is meant for.
var _ StateSetter = (*queue.Dispatcher)(nil)

func ip(v int) *int { return &v }

func sig(rssi, rsrp, rsrq, sinr int) *domain.Signal {
	return &domain.Signal{RSSI: ip(rssi), RSRP: ip(rsrp), RSRQ: ip(rsrq), SINR: ip(sinr)}
}

// goodHealth is a registered modem.
func goodHealth() connector.ChannelHealth {
	return connector.ChannelHealth{
		Running: true, Registered: true, Registration: "home", Operator: "Orange", Technology: "LTE",
		Signal: sig(-67, -95, -10, 12),
	}
}

func badHealth(registration string) connector.ChannelHealth {
	return connector.ChannelHealth{Running: true, Registration: registration}
}

var (
	errTransient = domain.Transient("health", errors.New("connection refused"))
	errAuth      = domain.AuthError("health", errors.New("401"))
)

// ---------------------------------------------------------------------------
// Stub connector

type stubConn struct {
	mu          sync.Mutex
	hfn         func(ctx context.Context, n int) (connector.ChannelHealth, error)
	pfn         func(ctx context.Context) (connector.DeviceProbe, error)
	healthCalls int
	probeCalls  int
	cur, maxCur int
}

func newStub(hfn func(ctx context.Context, n int) (connector.ChannelHealth, error)) *stubConn {
	return &stubConn{hfn: hfn}
}

// script answers the n-th Health call with the n-th character of seq:
// g good, b not registered, e transient error, a auth error. After the end
// of seq the last answer repeats.
func script(seq string) func(context.Context, int) (connector.ChannelHealth, error) {
	return func(_ context.Context, n int) (connector.ChannelHealth, error) {
		i := min(n, len(seq)) - 1
		switch seq[i] {
		case 'g':
			return goodHealth(), nil
		case 'b':
			return badHealth("searching"), nil
		case 'a':
			return connector.ChannelHealth{}, errAuth
		default:
			return connector.ChannelHealth{}, errTransient
		}
	}
}

func (s *stubConn) Capabilities() connector.Capabilities { return connector.Capabilities{} }

func (s *stubConn) Send(context.Context, connector.ChannelRef, connector.OutboundSMS) (connector.SendResult, error) {
	return connector.SendResult{}, nil
}

func (s *stubConn) FetchInbox(context.Context, connector.ChannelRef) ([]connector.InboundSMS, error) {
	return nil, nil
}

func (s *stubConn) DeleteInbox(context.Context, connector.ChannelRef, []string) error { return nil }

func (s *stubConn) Health(ctx context.Context, _ connector.ChannelRef) (connector.ChannelHealth, error) {
	s.mu.Lock()
	s.healthCalls++
	n := s.healthCalls
	s.cur++
	s.maxCur = max(s.maxCur, s.cur)
	fn := s.hfn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cur--
		s.mu.Unlock()
	}()
	if fn == nil {
		return goodHealth(), nil
	}
	return fn(ctx, n)
}

func (s *stubConn) Probe(ctx context.Context) (connector.DeviceProbe, error) {
	s.mu.Lock()
	s.probeCalls++
	fn := s.pfn
	s.mu.Unlock()
	if fn == nil {
		return connector.DeviceProbe{}, nil
	}
	return fn(ctx)
}

func (s *stubConn) Close() error { return nil }

func (s *stubConn) setProbe(fn func(ctx context.Context) (connector.DeviceProbe, error)) {
	s.mu.Lock()
	s.pfn = fn
	s.mu.Unlock()
}

func (s *stubConn) calls() (health, probe int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthCalls, s.probeCalls
}

func (s *stubConn) maxConcurrent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxCur
}

// gateHealth blocks every Health call until it is released or ctx ends.
type gateHealth struct {
	entered chan struct{}
	release chan connector.ChannelHealth
}

func newGate() *gateHealth {
	return &gateHealth{entered: make(chan struct{}, 64), release: make(chan connector.ChannelHealth, 64)}
}

func (g *gateHealth) fn(ctx context.Context, _ int) (connector.ChannelHealth, error) {
	g.entered <- struct{}{}
	select {
	case h := <-g.release:
		return h, nil
	case <-ctx.Done():
		return connector.ChannelHealth{}, ctx.Err()
	}
}

func (g *gateHealth) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a Health call")
	}
}

// ---------------------------------------------------------------------------
// Recording dispatcher

type pushCall struct {
	ID     domain.ID
	State  domain.ChannelState
	Reason string
}

type recDisp struct {
	mu     sync.Mutex
	calls  []pushCall
	reject bool
}

func (d *recDisp) SetChannelState(id domain.ID, st domain.ChannelState, reason string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, pushCall{id, st, reason})
	return !d.reject
}

func (d *recDisp) setReject(v bool) {
	d.mu.Lock()
	d.reject = v
	d.mu.Unlock()
}

func (d *recDisp) all() []pushCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]pushCall(nil), d.calls...)
}

func (d *recDisp) states() []domain.ChannelState {
	var out []domain.ChannelState
	for _, c := range d.all() {
		out = append(out, c.State)
	}
	return out
}

// ---------------------------------------------------------------------------
// Recording store

// countRepo counts UpdateHealth calls and can fail them.
type countRepo struct {
	store.ChannelRepo
	mu      sync.Mutex
	updates int
	fail    error
}

func (r *countRepo) UpdateHealth(ctx context.Context, id domain.ID, h store.ChannelHealthUpdate) error {
	r.mu.Lock()
	r.updates++
	fail := r.fail
	r.mu.Unlock()
	if fail != nil {
		return fail
	}
	return r.ChannelRepo.UpdateHealth(ctx, id, h)
}

func (r *countRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updates
}

func (r *countRepo) setFail(err error) {
	r.mu.Lock()
	r.fail = err
	r.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Log buffer and change recorder

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

type changeLog struct {
	mu  sync.Mutex
	chs []StateChange
}

func (c *changeLog) add(ch StateChange) {
	c.mu.Lock()
	c.chs = append(c.chs, ch)
	c.mu.Unlock()
}

func (c *changeLog) all() []StateChange {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]StateChange(nil), c.chs...)
}

func (c *changeLog) states() []domain.ChannelState {
	var out []domain.ChannelState
	for _, ch := range c.all() {
		out = append(out, ch.To)
	}
	return out
}

// ---------------------------------------------------------------------------
// Loop tracker

// tracker records which loops sleep and how many checks finished, so tests
// wait on it instead of using time.Sleep.
type tracker struct {
	mu       sync.Mutex
	cond     *sync.Cond
	sleeping map[string]time.Duration
	sleeps   map[string]int
	checks   map[domain.ID]int
	timedOut bool
}

func newTracker() *tracker {
	tr := &tracker{sleeping: map[string]time.Duration{}, sleeps: map[string]int{}, checks: map[domain.ID]int{}}
	tr.cond = sync.NewCond(&tr.mu)
	return tr
}

func (tr *tracker) hooks() hooks {
	return hooks{
		sleep: func(g string, d time.Duration) {
			tr.mu.Lock()
			tr.sleeping[g] = d
			tr.sleeps[g]++
			tr.cond.Broadcast()
			tr.mu.Unlock()
		},
		wake: func(g string) {
			tr.mu.Lock()
			delete(tr.sleeping, g)
			tr.cond.Broadcast()
			tr.mu.Unlock()
		},
		checked: func(id domain.ID) {
			tr.mu.Lock()
			tr.checks[id]++
			tr.cond.Broadcast()
			tr.mu.Unlock()
		},
	}
}

type view struct {
	sleeping map[string]time.Duration
	sleeps   map[string]int
	checks   map[domain.ID]int
}

func (v view) asleep(g string) bool { _, ok := v.sleeping[g]; return ok }

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
	for !cond(view{tr.sleeping, tr.sleeps, tr.checks}) {
		if tr.timedOut {
			tr.timedOut = false
			t.Fatalf("timed out waiting for: %s (sleeping %v, checks %v)", desc, tr.sleeping, tr.checks)
		}
		tr.cond.Wait()
	}
}

// ---------------------------------------------------------------------------
// Environment

type envConfig struct {
	path string // database file; empty: in memory
	opts func(*Options)
}

type env struct {
	t    *testing.T
	ctx  context.Context
	clk  *clock.Fake
	st   *sqlite.Store
	repo *countRepo
	hc   *Checker
	disp *recDisp
	tr   *tracker
	logs *logBuf
	chg  *changeLog
	ids  *domain.ULIDGenerator
	devs map[string]domain.Device
}

func newEnv(t *testing.T, cfg envConfig) *env {
	t.Helper()
	e := &env{
		t: t, ctx: context.Background(), clk: clock.NewFake(t0), disp: &recDisp{}, tr: newTracker(),
		logs: &logBuf{}, chg: &changeLog{}, devs: map[string]domain.Device{},
	}
	e.ids = domain.NewULIDGenerator(e.clk)
	st, err := sqlite.Open(e.ctx, sqlite.Options{Path: cfg.path, Clock: e.clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st
	e.repo = &countRepo{ChannelRepo: st.Channels()}
	opts := Options{
		Channels:      e.repo,
		Clock:         e.clk,
		Logger:        slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Dispatcher:    e.disp,
		OnStateChange: e.chg.add,
	}
	if cfg.opts != nil {
		cfg.opts(&opts)
	}
	hc, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	hc.hooks = e.tr.hooks()
	e.hc = hc
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
		defer cancel()
		_ = hc.Shutdown(ctx)
	})
	return e
}

func filePath(t *testing.T) string { return filepath.Join(t.TempDir(), "tikker.db") }

func (e *env) device(name string) domain.Device {
	e.t.Helper()
	if d, ok := e.devs[name]; ok {
		return d
	}
	d := domain.Device{
		ID: e.ids.NewID(), Name: name, Connector: domain.ConnectorRouterOSREST, BaseURL: "https://10.0.0.1",
		Username: "api", PasswordEnc: []byte("x"), TLSMode: domain.TLSVerify, Enabled: true, CreatedAt: t0,
	}
	if err := e.st.Devices().Create(e.ctx, d); err != nil {
		e.t.Fatal(err)
	}
	e.devs[name] = d
	return d
}

// channel creates an enabled channel in the store with the given state.
func (e *env) channel(dev, port string, state domain.ChannelState, mods ...func(*domain.Channel)) domain.Channel {
	e.t.Helper()
	d := e.device(dev)
	c := domain.Channel{
		ID: e.ids.NewID(), DeviceID: d.ID, Port: port, Name: dev + "/" + port, RatePerMin: 6, MaxSegments: 1,
		Enabled: true, State: state,
	}
	for _, f := range mods {
		f(&c)
	}
	if err := e.st.Channels().Create(e.ctx, c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func spec(c domain.Channel, conn connector.Connector) ChannelSpec {
	return ChannelSpec{Channel: c, Connector: conn}
}

// stored reads the channel back from the store.
func (e *env) stored(id domain.ID) domain.Channel {
	e.t.Helper()
	c, err := e.st.Channels().Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) check(id domain.ID) ChannelStatus {
	e.t.Helper()
	st, err := e.hc.CheckNow(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) flush() {
	e.t.Helper()
	if err := e.hc.Flush(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) status(id domain.ID) ChannelStatus {
	e.t.Helper()
	st, ok := e.hc.Status(id)
	if !ok {
		e.t.Fatalf("channel %s unknown", id)
	}
	return st
}

func (e *env) start() {
	e.t.Helper()
	if err := e.hc.Start(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) shutdown() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	if err := e.hc.Shutdown(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// primed returns an ok channel whose first check already ran, so that the
// following checks are subject to hysteresis.
func (e *env) primed(conn *stubConn) domain.Channel {
	e.t.Helper()
	c := e.channel("dev", "lte1", domain.ChannelOK)
	e.hc.SetChannels([]ChannelSpec{spec(c, conn)})
	if st := e.check(c.ID); st.State != domain.ChannelOK {
		e.t.Fatalf("priming check: state %s (%s)", st.State, st.Reason)
	}
	return c
}
