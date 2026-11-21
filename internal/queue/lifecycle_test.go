// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/connector"
	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/connector/routeros"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// fakeRepo is a MessageRepo that is never called.
type fakeRepo struct{ store.MessageRepo }

var errDB = errors.New("database is on fire")

// faultRepo injects store errors into selected methods.
type faultRepo struct {
	store.MessageRepo
	mu   sync.Mutex
	fail map[string]int // method -> remaining failures
}

func (f *faultRepo) set(method string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail == nil {
		f.fail = map[string]int{}
	}
	f.fail[method] = n
}

func (f *faultRepo) err(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[method] > 0 {
		f.fail[method]--
		return errDB
	}
	return nil
}

func (f *faultRepo) AcquireNext(ctx context.Context, req store.AcquireRequest) (domain.Message, bool, error) {
	if err := f.err("AcquireNext"); err != nil {
		return domain.Message{}, false, err
	}
	return f.MessageRepo.AcquireNext(ctx, req)
}

func (f *faultRepo) NextDue(ctx context.Context, ch domain.ID, tags []string) (time.Time, bool, error) {
	if err := f.err("NextDue"); err != nil {
		return time.Time{}, false, err
	}
	return f.MessageRepo.NextDue(ctx, ch, tags)
}

func (f *faultRepo) MarkSent(ctx context.Context, l domain.Lease, at time.Time) error {
	if err := f.err("MarkSent"); err != nil {
		return err
	}
	return f.MessageRepo.MarkSent(ctx, l, at)
}

func (f *faultRepo) RecoverExpiredLeases(ctx context.Context, now time.Time, maxAttempts int) ([]store.RecoveredLease, error) {
	if err := f.err("RecoverExpiredLeases"); err != nil {
		return nil, err
	}
	return f.MessageRepo.RecoverExpiredLeases(ctx, now, maxAttempts)
}

func (f *faultRepo) ExpireQueued(ctx context.Context, now time.Time) (int, error) {
	if err := f.err("ExpireQueued"); err != nil {
		return 0, err
	}
	return f.MessageRepo.ExpireQueued(ctx, now)
}

// TestKillDuringSend is the ADR 0002 scenario: process A leases a message and
// dies in the middle of the send (its clock stops, nothing is recorded).
// After the lease has run out, process B on the same database file recovers
// the message as a possible duplicate and sends it. When A's late result
// finally arrives, it is fenced off with ErrLeaseLost.
func TestKillDuringSend(t *testing.T) {
	path := filePath(t)
	a := newEnv(t, envConfig{path: path})
	gA := newGate()
	connA := newStub(a.clk, gA.send) // SendTimeout 10 s -> lease 50 s
	a.d.SetChannels([]ChannelSpec{spec(chA, connA)})
	a.start()
	m := a.enqueue(a.msg("+48600000001"))[0]
	gA.waitEntered(t)
	if got := a.get(m.ID); got.Status != domain.StatusSending || !got.LeaseUntil.Equal(t0.Add(50*time.Second)) {
		t.Fatalf("leased message = %+v", got)
	}

	// "Restart": a second dispatcher and store on the same file, its clock
	// past A's lease. A's frozen clock never times out its send.
	b := newEnv(t, envConfig{path: path, clk: clock.NewFake(t0.Add(51 * time.Second))})
	connB := newStub(b.clk, nil)
	b.d.SetChannels([]ChannelSpec{spec(chA, connB)})
	b.start()
	b.waitStatus(chA, domain.StatusSent, m.ID)

	got := b.get(m.ID)
	if got.Attempts != 2 || !got.PossibleDuplicate || got.ChannelID != chA || !got.SentAt.Equal(t0.Add(51*time.Second)) {
		t.Fatalf("message after recovery = %+v", got)
	}
	if st := b.d.Stats(); st.RecoveredLeases != 1 || st.Totals.PossibleDuplicates != 1 || st.Totals.Sent != 1 {
		t.Fatalf("B stats = %+v", st)
	}
	if !strings.Contains(b.logs.String(), "possible duplicate: lease expired") {
		t.Fatalf("B did not log the possible duplicate:\n%s", b.logs.String())
	}

	// A wakes up and reports success: its stale lease is rejected.
	gA.release <- nil
	a.tr.wait(t, "A lease lost", func(v view) bool { return a.d.Stats().Totals.LeaseLost == 1 && v.asleep(chA) })
	if after := b.get(m.ID); after.Attempts != 2 || !after.SentAt.Equal(got.SentAt) || after.Status != domain.StatusSent {
		t.Fatalf("stale write changed the message: %+v", after)
	}
	if st := a.d.Stats().Totals; st.Sent != 0 {
		t.Fatalf("A counted the stale send: %+v", st)
	}
	if !strings.Contains(a.logs.String(), "lease lost") {
		t.Fatalf("A did not log the lost lease:\n%s", a.logs.String())
	}
	if len(connB.Calls()) != 1 || len(connA.Calls()) != 1 {
		t.Fatalf("calls A=%d B=%d", len(connA.Calls()), len(connB.Calls()))
	}
}

func TestShutdownWaitsForInFlightSend(t *testing.T) {
	e := newEnv(t, envConfig{})
	g := newGate()
	conn := newStub(e.clk, g.send)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	ms := e.enqueue(e.msg("+48600000001"), e.msg("+48600000002"))
	g.waitEntered(t)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
		defer cancel()
		done <- e.d.Shutdown(ctx)
	}()
	e.tr.wait(t, "shutdown requested", func(view) bool { return e.d.Stats().Closed })
	g.release <- nil
	if err := <-done; err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if got := e.get(ms[0].ID); got.Status != domain.StatusSent || got.PossibleDuplicate {
		t.Fatalf("in-flight message = %+v", got)
	}
	if got := e.get(ms[1].ID); got.Status != domain.StatusQueued || got.Attempts != 0 {
		t.Fatalf("no new lease may be taken during shutdown: %+v", got)
	}
	if conn.N() != 1 {
		t.Fatalf("calls = %d", conn.N())
	}
	if rel := e.releasedConns(); len(rel) != 1 || rel[0] != connector.Connector(conn) {
		t.Fatalf("released = %v", rel)
	}
}

func TestShutdownCancelsInFlightSend(t *testing.T) {
	tests := []struct {
		name       string
		maxAtt     int
		wantStatus domain.MessageStatus
	}{
		{"requeued", 5, domain.StatusQueued},
		{"failed at the retry limit", 1, domain.StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envConfig{opts: func(o *Options) {
				o.Retry = domain.DefaultRetryPolicy()
				o.Retry.MaxAttempts = tt.maxAtt
			}})
			g := newGate()
			conn := newStub(e.clk, g.send)
			e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
			e.start()
			m := e.enqueue(e.msg("+48600000001"))[0]
			g.waitEntered(t)

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // deadline already passed
			if err := e.d.Shutdown(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Shutdown = %v, want context.Canceled", err)
			}
			got := e.get(m.ID)
			if got.Status != tt.wantStatus || !got.PossibleDuplicate || got.Attempts != 1 || !got.LeaseUntil.IsZero() {
				t.Fatalf("message = %+v", got)
			}
			if tt.wantStatus == domain.StatusQueued && !got.NextAttemptAt.Equal(t0) {
				t.Fatalf("interrupted send must be retried at once, NextAttemptAt = %v", got.NextAttemptAt)
			}
			if st := e.d.Stats().Totals; st.PossibleDuplicates != 1 {
				t.Fatalf("stats = %+v", st)
			}
		})
	}
}

func TestSendTimeout(t *testing.T) {
	e := newEnv(t, envConfig{})
	g := newGate()
	conn := newStub(e.clk, g.send)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	m := e.enqueue(e.msg("+48600000001"))[0]
	g.waitEntered(t)
	e.clk.Advance(10 * time.Second) // Capabilities.SendTimeout
	e.waitStatus(chA, domain.StatusQueued, m.ID)
	got := e.get(m.ID)
	if !got.PossibleDuplicate || !strings.HasPrefix(got.LastError, "send timeout after 10s") || got.Attempts != 1 {
		t.Fatalf("message = %+v", got)
	}
}

func TestExpireQueued(t *testing.T) {
	t.Run("janitor", func(t *testing.T) {
		e := newEnv(t, envConfig{})
		s := spec(chA, newStub(e.clk, nil))
		s.Channel.State = domain.ChannelDown // nothing gets sent
		e.d.SetChannels([]ChannelSpec{s})
		e.start()
		m := e.enqueue(e.msg("+48600000001", func(m *domain.Message) { m.ExpiresAt = t0.Add(90 * time.Second) }))[0]
		e.tr.wait(t, "janitor asleep", func(v view) bool { return v.asleep(janitorID) && v.sleepsFor(chA, 0) })
		e.clk.Advance(2 * time.Minute)
		e.tr.wait(t, "expired", func(view) bool { return e.status(m.ID) == domain.StatusFailed })
		if got := e.get(m.ID); !strings.HasPrefix(got.LastError, domain.LastErrorExpired) || got.Attempts != 0 {
			t.Fatalf("message = %+v", got)
		}
		e.tr.wait(t, "stats", func(view) bool { return e.d.Stats().Expired == 1 })
	})
	t.Run("due but expired triggers a sweep", func(t *testing.T) {
		e := newEnv(t, envConfig{})
		conn := newStub(e.clk, nil)
		s := spec(chA, conn)
		s.Channel.State = domain.ChannelDown
		e.d.SetChannels([]ChannelSpec{s})
		e.start()
		m := e.enqueue(e.msg("+48600000001", func(m *domain.Message) { m.ExpiresAt = t0.Add(5 * time.Second) }))[0]
		e.tr.wait(t, "janitor asleep", func(v view) bool { return v.asleep(janitorID) })
		e.clk.Advance(10 * time.Second) // before any janitor timer
		e.d.SetChannelState(chA, domain.ChannelOK, "")
		e.tr.wait(t, "expired by sweep", func(view) bool { return e.status(m.ID) == domain.StatusFailed })
		e.tr.wait(t, "stats", func(view) bool {
			st := e.d.Stats()
			return st.Expired == 1 && st.LastExpireAt.Equal(t0.Add(10*time.Second))
		})
		if conn.N() != 0 {
			t.Fatal("expired message was sent")
		}
	})
}

// TestNoWritesWhenIdle checks that idle workers and the janitor never write
// to the database file (flash wear, SPEC §2.2).
func TestNoWritesWhenIdle(t *testing.T) {
	path := filePath(t)
	e := newEnv(t, envConfig{path: path})
	e.d.SetChannels([]ChannelSpec{spec(chA, newStub(e.clk, nil), "a"), spec(chB, newStub(e.clk, nil))})
	e.start()
	e.tr.wait(t, "idle", func(v view) bool { return v.asleep(chA) && v.asleep(chB) && v.asleep(janitorID) })

	observer, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	observer.SetMaxOpenConns(1)
	defer func() { _ = observer.Close() }()
	version := func() int {
		var v int
		if err := observer.QueryRow("PRAGMA data_version").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := version()
	for range 5 {
		nA, nB, nJ := e.tr.sleeps(chA), e.tr.sleeps(chB), e.tr.sleeps(janitorID)
		e.clk.Advance(time.Hour)
		e.tr.wait(t, "another idle round", func(v view) bool {
			return v.count[chA] > nA && v.count[chB] > nB && v.count[janitorID] > nJ &&
				v.asleep(chA) && v.asleep(chB) && v.asleep(janitorID)
		})
	}
	if after := version(); after != before {
		t.Fatalf("data_version %d -> %d: idle dispatcher wrote to the database", before, after)
	}
	if st := e.d.Stats(); !st.LastRecoverAt.After(t0) || !st.LastExpireAt.After(t0) {
		t.Fatalf("janitor did not run: %+v", st)
	}
}

func TestSetChannels(t *testing.T) {
	e := newEnv(t, envConfig{})
	g := newGate()
	conn1 := newStub(e.clk, g.send)
	conn2 := newStub(e.clk, nil)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn1)})
	e.start()

	// Replace the connector while a send is in flight: the new worker must
	// wait for the old one.
	ms := e.enqueue(e.msg("+48600000001"), e.msg("+48600000002"))
	g.waitEntered(t)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn2)})
	if conn2.N() != 0 || len(e.releasedConns()) != 0 {
		t.Fatal("new worker ran while the old one was sending")
	}
	g.release <- nil
	e.waitStatus(chA, domain.StatusSent, ms[0].ID, ms[1].ID)
	if c := conn2.Calls(); len(c) != 1 || c[0].MessageID != ms[1].ID {
		t.Fatalf("new connector calls = %+v", c)
	}
	e.tr.wait(t, "old connector released", func(view) bool { return len(e.releasedConns()) == 1 })
	if e.releasedConns()[0] != connector.Connector(conn1) {
		t.Fatal("wrong connector released")
	}

	// Update in place: disable, then change rate and re-enable.
	s := spec(chA, conn2)
	s.Channel.Enabled = false
	e.d.SetChannels([]ChannelSpec{s})
	m3 := e.enqueue(e.msg("+48600000003"))[0]
	e.tr.wait(t, "disabled", func(v view) bool { return v.sleepsFor(chA, 0) })
	if st := e.d.Stats().Channels; len(st) != 1 || st[0].Active || st[0].Enabled {
		t.Fatalf("stats = %+v", st)
	}
	if e.status(m3.ID) != domain.StatusQueued {
		t.Fatal("disabled channel sent")
	}
	s.Channel.Enabled = true
	e.d.SetChannels([]ChannelSpec{s})
	e.waitStatus(chA, domain.StatusSent, m3.ID)

	// Remove: the worker exits and the connector is released.
	e.d.SetChannels(nil)
	e.tr.wait(t, "removed", func(view) bool { return len(e.releasedConns()) == 2 })
	m4 := e.enqueue(e.msg("+48600000004"))[0]
	if len(e.d.Stats().Channels) != 0 || e.status(m4.ID) != domain.StatusQueued {
		t.Fatal("removed channel still served")
	}

	// A channel set before Start and replaced before it ran.
	e2 := newEnv(t, envConfig{})
	c1, c2 := newStub(e2.clk, nil), newStub(e2.clk, nil)
	e2.d.SetChannels([]ChannelSpec{spec(chA, c1), spec(chB, c1)})
	e2.d.SetChannels([]ChannelSpec{spec(chA, c2)})
	if rel := e2.releasedConns(); len(rel) != 1 || rel[0] != connector.Connector(c1) {
		t.Fatalf("released before start = %v", rel)
	}
	e2.start()
	m := e2.enqueue(e2.msg("+48600000001"))[0]
	e2.waitStatus(chA, domain.StatusSent, m.ID)
	if c2.N() != 1 || c1.N() != 0 {
		t.Fatal("wrong connector used")
	}
}

func TestStoreErrors(t *testing.T) {
	var fr *faultRepo
	e := newEnv(t, envConfig{repo: func(r store.MessageRepo) store.MessageRepo {
		fr = &faultRepo{MessageRepo: r}
		return fr
	}})
	conn := newStub(e.clk, nil)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})

	fr.set("RecoverExpiredLeases", 1)
	if err := e.d.Start(e.ctx); !errors.Is(err, errDB) {
		t.Fatalf("Start = %v, want the recovery error", err)
	}

	e = newEnv(t, envConfig{repo: func(r store.MessageRepo) store.MessageRepo {
		fr = &faultRepo{MessageRepo: r}
		return fr
	}, opts: func(o *Options) { o.RecoverInterval = time.Minute; o.ExpireInterval = time.Minute }})
	conn = newStub(e.clk, nil)
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()
	e.tr.wait(t, "idle", func(v view) bool { return v.sleepsFor(chA, DefaultMaxIdle) })

	// AcquireNext fails: back off, then succeed.
	fr.set("AcquireNext", 1)
	m1 := e.enqueue(e.msg("+48600000001"))[0]
	e.tr.wait(t, "error backoff", func(v view) bool { return v.sleepsFor(chA, DefaultErrorBackoff) })
	e.clk.Advance(DefaultErrorBackoff)
	e.waitStatus(chA, domain.StatusSent, m1.ID)

	// NextDue fails: back off.
	fr.set("NextDue", 1)
	before := e.tr.sleeps(chA)
	e.d.Notify()
	e.tr.wait(t, "next due backoff", func(v view) bool { return v.count[chA] > before && v.sleepsFor(chA, DefaultErrorBackoff) })

	// MarkSent fails: the message stays leased until the janitor recovers
	// it as a possible duplicate and it is sent again.
	fr.set("MarkSent", 1)
	m2 := e.enqueue(e.msg("+48600000002"))[0]
	e.tr.wait(t, "result lost", func(v view) bool { return conn.N() == 2 && v.asleep(chA) && e.d.Stats().Totals.StoreErrors == 3 })
	if got := e.get(m2.ID); got.Status != domain.StatusSending {
		t.Fatalf("message = %+v", got)
	}
	fr.set("ExpireQueued", 1)
	e.clk.Advance(time.Hour) // past the lease; janitor recovers and expires (fails)
	e.waitStatus(chA, domain.StatusSent, m2.ID)
	if got := e.get(m2.ID); !got.PossibleDuplicate || got.Attempts != 2 {
		t.Fatalf("recovered message = %+v", got)
	}
	e.tr.wait(t, "expire error counted", func(view) bool { return e.d.Stats().Totals.StoreErrors == 4 })
	if !strings.Contains(e.logs.String(), "expire queued messages failed") {
		t.Fatal("expire error not logged")
	}
}

// TestRouterOSConnector runs the dispatcher against the real routeros-rest
// connector and the fake RouterOS server.
func TestRouterOSConnector(t *testing.T) {
	srv := fake.New(fake.Options{User: "tikker", Password: "pw", Ports: []string{"lte1"}})
	url := srv.Start()
	t.Cleanup(srv.Close)

	e := newEnv(t, envConfig{})
	conn, err := routeros.NewConn(e.ctx, connector.DeviceConfig{
		DeviceID: "01JDEVICEXXXXXXXXXXXXXXXXX", Kind: domain.ConnectorRouterOSREST, BaseURL: url,
		Username: "tikker", Password: "pw", TLSMode: domain.TLSVerify,
	}, routeros.Options{Clock: e.clk, Logger: e.d.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	e.d.SetChannels([]ChannelSpec{spec(chA, conn)})
	e.start()

	m1 := e.enqueue(e.msg("+48600000001"))[0]
	e.waitStatus(chA, domain.StatusSent, m1.ID)
	sent := srv.Sent()
	if len(sent) != 1 || sent[0].PhoneNumber != "+48600000001" || sent[0].Port != "lte1" || sent[0].Message != m1.Body {
		t.Fatalf("router got %+v", sent)
	}

	// Bad credentials: Auth error, channel degraded.
	srv.FailNext(fake.EndpointSend, fake.Unauthorized(), 1)
	m2 := e.enqueue(e.msg("+48600000002"))[0]
	e.tr.wait(t, "degraded", func(v view) bool { return v.sleepsFor(chA, 0) && e.status(m2.ID) == domain.StatusQueued })
	if k := e.events.kinds(); !slices.Equal(k, []EventKind{EventSent, EventAuthFailed}) {
		t.Fatalf("events = %v", k)
	}
	// Restored by the health checker; retried after its backoff.
	e.d.SetChannelState(chA, domain.ChannelOK, "")
	e.tr.wait(t, "waiting for backoff", func(v view) bool { return v.sleepsFor(chA, 30*time.Second) })
	e.clk.Advance(30 * time.Second)
	e.waitStatus(chA, domain.StatusSent, m2.ID)
	if len(srv.Sent()) != 2 {
		t.Fatalf("router got %d messages", len(srv.Sent()))
	}
}
