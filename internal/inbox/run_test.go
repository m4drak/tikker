// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package inbox

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // observer connection

	"github.com/m4drak/tikker/internal/connector/fake"
	"github.com/m4drak/tikker/internal/domain"
)

func TestBackoffAndReporting(t *testing.T) {
	e := newEnv(t, nil)
	const iv = 15 * time.Second
	e.srv.FailNext(fake.EndpointInbox, fake.HTTPError(503, "busy"), -1)

	var delays []time.Duration
	for range 7 {
		delays = append(delays, e.p.step(e.ctx))
	}
	// With the default cap of 5 minutes: 30s 1m 2m 4m 5m 5m 5m.
	wantDefault := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i := range wantDefault {
		if delays[i] != wantDefault[i] {
			t.Fatalf("delays = %v, want %v", delays, wantDefault)
		}
	}
	// One report for the first failure; the class did not change since.
	if calls := e.rep.get(); len(calls) != 1 || !errors.Is(calls[0], domain.ErrTransient) {
		t.Fatalf("reports = %v", calls)
	}
	// Failures are logged at warn level only when the count reaches a power of two.
	if n := strings.Count(e.logs.String(), "level=WARN msg=\"inbox poll failed\""); n != 3 { // 1, 2, 4
		t.Errorf("%d warnings for 7 failures, want 3:\n%s", n, e.logs.String())
	}

	// A credentials problem is a new class and is reported again.
	e.srv.ClearFaults()
	e.srv.FailNext(fake.EndpointInbox, fake.Unauthorized(), -1)
	e.p.step(e.ctx)
	calls := e.rep.get()
	if len(calls) != 2 || !errors.Is(calls[1], domain.ErrAuth) {
		t.Fatalf("reports = %v", calls)
	}
	e.p.step(e.ctx)
	if len(e.rep.get()) != 2 {
		t.Fatalf("repeated auth failure reported again: %v", e.rep.get())
	}

	// Recovery resets the back-off and clears the state.
	e.srv.ClearFaults()
	if d := e.p.step(e.ctx); d != iv {
		t.Fatalf("delay after recovery = %v, want %v", d, iv)
	}
	calls = e.rep.get()
	if len(calls) != 3 || calls[2] != nil {
		t.Fatalf("reports = %v", calls)
	}
	if d := e.p.step(e.ctx); d != iv || len(e.rep.get()) != 3 {
		t.Fatalf("steady state: delay %v, reports %v", d, e.rep.get())
	}
}

func TestDeleteFailureBacksOffAndRecovers(t *testing.T) {
	e := newEnv(t, nil)
	e.inject("hello")
	e.srv.FailNext(fake.EndpointInboxDelete, fake.Unauthorized(), 1)
	if d := e.p.step(e.ctx); d != 30*time.Second {
		t.Fatalf("delay = %v", d)
	}
	if calls := e.rep.get(); len(calls) != 1 || !errors.Is(calls[0], domain.ErrAuth) {
		t.Fatalf("reports = %v", calls)
	}
	if d := e.p.step(e.ctx); d != 15*time.Second {
		t.Fatalf("delay after recovery = %v", d)
	}
	if calls := e.rep.get(); len(calls) != 2 || calls[1] != nil {
		t.Fatalf("reports = %v", calls)
	}
	if len(e.inbound()) != 1 || e.routerInbox() != 0 {
		t.Fatalf("stored %d, router %d", len(e.inbound()), e.routerInbox())
	}
}

func TestBackoffNeverOverflows(t *testing.T) {
	e := newEnv(t, nil)
	e.p.failures = 1 << 30
	if d := e.p.backoff(); d != e.p.maxBackoff {
		t.Fatalf("backoff = %v", d)
	}
}

func TestDeleteInChunks(t *testing.T) {
	e := newEnv(t, nil)
	const n = deleteChunk*2 + 5
	for i := range n {
		e.inject("m" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('A'+i/26)))
	}
	res := e.poll()
	if res.Fetched != n || res.Stored != n || res.Deleted != n || e.routerInbox() != 0 {
		t.Fatalf("result = %+v, router %d", res, e.routerInbox())
	}
}

func TestRunPollsOnTimer(t *testing.T) {
	got := make(chan domain.Message, 8)
	e := newEnv(t, func(c *Config) { c.OnMessage = func(m domain.Message) { got <- m } })
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- e.p.Run(ctx) }()

	e.clk.BlockUntil(1) // first (empty) poll done, sleeping
	e.inject("first")
	e.clk.Advance(15 * time.Second)
	if m := <-got; m.Body != "first" {
		t.Fatalf("message = %+v", m)
	}
	e.clk.BlockUntil(1)
	e.inject("second")
	e.clk.Advance(15 * time.Second)
	if m := <-got; m.Body != "second" {
		t.Fatalf("message = %+v", m)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

// SPEC §2.2: an idle poller does not write to the database file.
func TestNoWritesWhenIdle(t *testing.T) {
	e := newEnv(t, nil)
	e.inject("one")
	e.poll() // creates the first rows and the WAL

	observer, err := sql.Open("sqlite", "file:"+e.path+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	observer.SetMaxOpenConns(1)
	defer func() { _ = observer.Close() }()
	type state struct {
		version int
		db, wal int64
		dbMod   time.Time
	}
	snapshot := func() state {
		var s state
		if err := observer.QueryRow("PRAGMA data_version").Scan(&s.version); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(e.path); err == nil {
			s.db, s.dbMod = fi.Size(), fi.ModTime()
		}
		if fi, err := os.Stat(e.path + "-wal"); err == nil {
			s.wal = fi.Size()
		}
		return s
	}
	before := snapshot()

	// Empty inbox, with a pending part that is not yet due.
	for range 5 {
		if res, err := e.p.PollOnce(e.ctx); err != nil || res != (CycleResult{}) {
			t.Fatalf("empty poll = %+v, %v", res, err)
		}
		e.p.step(e.ctx)
	}
	if after := snapshot(); after != before {
		t.Fatalf("empty polling wrote to the database: %+v -> %+v", before, after)
	}

	// Duplicates only: the router keeps redelivering because deleting fails.
	e.inject("two")
	e.srv.FailNext(fake.EndpointInboxDelete, fake.HTTPError(500, "boom"), 1)
	if _, err := e.p.PollOnce(e.ctx); err == nil {
		t.Fatal("expected delete failure")
	}
	mid := snapshot()
	e.srv.FailNext(fake.EndpointInboxDelete, fake.HTTPError(500, "boom"), 3)
	for range 3 {
		res, err := e.p.PollOnce(e.ctx)
		if err == nil || res.Duplicates != 1 || res.Stored != 0 {
			t.Fatalf("duplicate poll = %+v, %v", res, err)
		}
	}
	if after := snapshot(); after != mid {
		t.Fatalf("duplicates wrote to the database: %+v -> %+v", mid, after)
	}
	if len(e.inbound()) != 2 {
		t.Fatalf("stored %d messages", len(e.inbound()))
	}
}

func TestNoWritesWithUnexpiredPendingParts(t *testing.T) {
	e := newEnv(t, nil)
	e.injectParts(strings.Repeat("Zażółć gęślą jaźń. ", 9), 1, 1)
	e.poll()
	observer, err := sql.Open("sqlite", "file:"+e.path+"?_pragma=busy_timeout(2000)")
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
	e.clk.Advance(time.Hour)
	for range 3 {
		if res := e.poll(); res != (CycleResult{}) {
			t.Fatalf("poll = %+v", res)
		}
	}
	if after := version(); after != before {
		t.Fatalf("data_version %d -> %d", before, after)
	}
}
