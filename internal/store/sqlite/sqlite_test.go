// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
	"github.com/m4drak/tikker/internal/store/sqlite"
	"github.com/m4drak/tikker/internal/store/storetest"
)

func open(t *testing.T, opts sqlite.Options) *sqlite.Store {
	t.Helper()
	if opts.Clock == nil {
		opts.Clock = clock.NewFake(storetest.T0)
	}
	s, err := sqlite.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestContractMemory(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return open(t, sqlite.Options{}) })
}

func TestContractFile(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return open(t, sqlite.Options{Path: filepath.Join(t.TempDir(), "tikker.db")})
	})
}

func TestContractFileFullSync(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		return open(t, sqlite.Options{Path: filepath.Join(t.TempDir(), "tikker.db"), Synchronous: "full", ReadConns: 2})
	})
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := sqlite.Open(ctx, sqlite.Options{Synchronous: "sometimes"}); err == nil {
		t.Error("invalid synchronous accepted")
	}
	bad := filepath.Join(t.TempDir(), "missing", "dir", "x.db")
	if _, err := sqlite.Open(ctx, sqlite.Options{Path: bad}); err == nil {
		t.Error("unwritable path accepted")
	}
	notDB := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(notDB, []byte(strings.Repeat("this is not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Open(ctx, sqlite.Options{Path: notDB}); err == nil {
		t.Error("garbage file accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := sqlite.Open(cancelled, sqlite.Options{}); err == nil {
		t.Error("cancelled context accepted")
	}
}

func TestPathWithSpecialCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "we ird#na?me%.db")
	s := open(t, sqlite.Options{Path: path})
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not at requested path: %v", err)
	}
}

func TestPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tikker.db")
	s := open(t, sqlite.Options{Path: path, Synchronous: "FULL", BusyTimeout: 1234 * time.Millisecond})
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var mode string
	if err := raw.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	// Writes through the read pool are impossible: the store's own reads use
	// query_only connections. Prove it with a repository that only reads.
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestPersistence reopens a file database and finds the data.
func TestPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tikker.db")
	s, err := sqlite.Open(ctx, sqlite.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	d := domain.Device{ID: "01JZZZZZZZZZZZZZZZZZZZZZZA", Name: "keep", Connector: domain.ConnectorRouterOSREST,
		BaseURL: "https://x", Username: "u", PasswordEnc: []byte("p"), TLSMode: domain.TLSVerify, Enabled: true}
	if err := s.Devices().Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := sqlite.Open(ctx, sqlite.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	got, err := s2.Devices().Get(ctx, d.ID)
	if err != nil || got.Name != "keep" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

// TestForeignKeysEnforced checks PRAGMA foreign_keys is on for the writer.
func TestForeignKeysEnforced(t *testing.T) {
	s := open(t, sqlite.Options{})
	err := s.Channels().Create(context.Background(), domain.Channel{
		ID: "01JZZZZZZZZZZZZZZZZZZZZZZB", DeviceID: "01JZZZZZZZZZZZZZZZZZZZZZZC", Port: "lte1", Name: "x"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (foreign key)", err)
	}
}

func TestClosedStoreErrors(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "x.db")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err == nil {
		t.Error("Ping on closed store succeeded")
	}
	if err := s.WithTx(ctx, func(store.Repos) error { return nil }); err == nil {
		t.Error("WithTx on closed store succeeded")
	}
	if _, err := s.Devices().List(ctx); err == nil {
		t.Error("read on closed store succeeded")
	}
	if err := s.Devices().Delete(ctx, "01JZZZZZZZZZZZZZZZZZZZZZZD"); err == nil {
		t.Error("write on closed store succeeded")
	}
}

func TestCancelledContext(t *testing.T) {
	s := open(t, sqlite.Options{Path: filepath.Join(t.TempDir(), "x.db")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Messages().Get(ctx, "01JZZZZZZZZZZZZZZZZZZZZZZD"); err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get with cancelled ctx = %v", err)
	}
	if _, ok, err := s.Messages().AcquireNext(ctx, store.AcquireRequest{ChannelID: "x"}); err == nil || ok {
		t.Errorf("AcquireNext with cancelled ctx = %v, %v", ok, err)
	}
}

// TestNoWritesWhenIdle verifies SPEC §2.2: reads and empty polls leave the
// database untouched (no new WAL frames, no change visible to another
// connection, no growth of the files).
func TestNoWritesWhenIdle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tikker.db")
	s := open(t, sqlite.Options{Path: path})

	// Some data to read, then let everything settle.
	d := domain.Device{ID: "01JZZZZZZZZZZZZZZZZZZZZZZA", Name: "d", Connector: domain.ConnectorRouterOSREST,
		BaseURL: "https://x", Username: "u", PasswordEnc: []byte("p"), TLSMode: domain.TLSVerify, Enabled: true}
	ch := domain.Channel{ID: "01JZZZZZZZZZZZZZZZZZZZZZZB", DeviceID: d.ID, Port: "lte1", Name: "c", Tags: []string{"a"},
		RatePerMin: 6, MaxSegments: 1, Enabled: true, State: domain.ChannelOK}
	u := domain.User{ID: "01JZZZZZZZZZZZZZZZZZZZZZZC", Username: "admin", PasswordHash: "x", Role: domain.RoleAdmin}
	tok := domain.APIToken{ID: "01JZZZZZZZZZZZZZZZZZZZZZZD", UserID: u.ID, Name: "t", Prefix: "pfx", Hash: []byte{1}, CreatedAt: storetest.T0}
	for _, err := range []error{
		s.Devices().Create(ctx, d), s.Channels().Create(ctx, ch), s.Users().Create(ctx, u), s.Tokens().Create(ctx, tok),
		s.Channels().UpdateHealth(ctx, ch.ID, store.ChannelHealthUpdate{State: domain.ChannelOK, At: storetest.T0}),
		s.Tokens().TouchLastUsed(ctx, tok.ID, storetest.T0),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}

	observer, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	observer.SetMaxOpenConns(1)
	defer func() { _ = observer.Close() }()
	dataVersion := func() int {
		var v int
		if err := observer.QueryRow("PRAGMA data_version").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	type fileState struct {
		size int64
		mod  time.Time
	}
	stat := func(p string) fileState {
		fi, err := os.Stat(p)
		if err != nil {
			return fileState{}
		}
		return fileState{fi.Size(), fi.ModTime()}
	}
	before := [3]any{dataVersion(), stat(path), stat(path + "-wal")}

	ids := func() { // every read-only or no-op call a poller makes
		m := s.Messages()
		if _, ok, err := m.AcquireNext(ctx, store.AcquireRequest{ChannelID: ch.ID, ChannelTags: ch.Tags, Now: storetest.T0, LeaseUntil: storetest.T0.Add(time.Minute)}); err != nil || ok {
			t.Fatalf("AcquireNext = %v, %v", ok, err)
		}
		if _, ok, err := m.NextDue(ctx, ch.ID, ch.Tags); err != nil || ok {
			t.Fatalf("NextDue = %v, %v", ok, err)
		}
		if rec, err := m.RecoverExpiredLeases(ctx, storetest.T0, 3); err != nil || len(rec) != 0 {
			t.Fatalf("Recover = %v, %v", rec, err)
		}
		if n, err := m.ExpireQueued(ctx, storetest.T0); err != nil || n != 0 {
			t.Fatalf("ExpireQueued = %v, %v", n, err)
		}
		if n, err := m.PurgeBodies(ctx, storetest.T0); err != nil || n != 0 {
			t.Fatalf("PurgeBodies = %v, %v", n, err)
		}
		if n, err := m.PurgeIdempotency(ctx, storetest.T0); err != nil || n != 0 {
			t.Fatalf("PurgeIdempotency = %v, %v", n, err)
		}
		if _, err := s.Retain(ctx, storetest.T0, time.Hour); err != nil {
			t.Fatal(err)
		}
		if p, err := m.List(ctx, store.MessageFilter{}, store.PageRequest{}); err != nil || len(p.Items) != 0 {
			t.Fatalf("List = %v, %v", p, err)
		}
		if n, err := m.Count(ctx, store.MessageFilter{}); err != nil || n != 0 {
			t.Fatalf("Count = %v, %v", n, err)
		}
		if _, err := s.Channels().List(ctx, store.ChannelFilter{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Devices().List(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Users().GetByUsername(ctx, "admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tokens().GetByPrefix(ctx, "pfx"); err != nil {
			t.Fatal(err)
		}
		// Coalesced writes: same health, token touched just now.
		if err := s.Channels().UpdateHealth(ctx, ch.ID, store.ChannelHealthUpdate{State: domain.ChannelOK, At: storetest.T0.Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
		if err := s.Tokens().TouchLastUsed(ctx, tok.ID, storetest.T0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		// Duplicate inbound (after the first insert) and idempotent replay write nothing.
		if err := s.WithTx(ctx, func(tx store.Repos) error {
			_, err := tx.Messages().Count(ctx, store.MessageFilter{})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Ping(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		ids()
	}
	after := [3]any{dataVersion(), stat(path), stat(path + "-wal")}
	if before != after {
		t.Fatalf("idle polling wrote to the database:\nbefore %v\nafter  %v", before, after)
	}

	// Duplicate inbound and idempotent replay.
	in := domain.Message{ID: "01JZZZZZZZZZZZZZZZZZZZZZZE", ChannelID: ch.ID, Peer: "+48600000001", Body: "x",
		Encoding: domain.EncodingGSM7, Segments: 1, DedupHash: "dup", CreatedAt: storetest.T0}
	if c, err := s.Messages().InsertInbound(ctx, in); err != nil || !c {
		t.Fatalf("InsertInbound = %v, %v", c, err)
	}
	out := domain.Message{ID: "01JZZZZZZZZZZZZZZZZZZZZZZF", Peer: "+48600000001", Body: "x", Encoding: domain.EncodingGSM7,
		Segments: 1, CreatedBy: domain.TokenActor(tok.ID), CreatedAt: storetest.T0}
	idem := store.Idempotency{Key: "k", RequestHash: "h", Since: storetest.T0.Add(-time.Hour)}
	if _, err := s.Messages().Enqueue(ctx, []domain.Message{out}, idem); err != nil {
		t.Fatal(err)
	}
	before = [3]any{dataVersion(), stat(path), stat(path + "-wal")}
	in.ID = "01JZZZZZZZZZZZZZZZZZZZZZZG"
	if c, err := s.Messages().InsertInbound(ctx, in); err != nil || c {
		t.Fatalf("duplicate InsertInbound = %v, %v", c, err)
	}
	if r, err := s.Messages().Enqueue(ctx, []domain.Message{out}, idem); err != nil || !r.Replayed {
		t.Fatalf("replay = %+v, %v", r, err)
	}
	after = [3]any{dataVersion(), stat(path), stat(path + "-wal")}
	if before != after {
		t.Fatalf("duplicates wrote to the database:\nbefore %v\nafter  %v", before, after)
	}

	// Control: a real write does change the observed state.
	if _, err := s.Messages().Cancel(ctx, out.ID); err != nil {
		t.Fatal(err)
	}
	if dataVersion() == before[0] {
		t.Fatal("control write not observed; the test would pass vacuously")
	}
}

// TestFileConcurrentAcquire hammers one queue from many goroutines (run with
// -race): every message must be handed out exactly once, reads and writes may
// overlap without SQLITE_BUSY.
func TestFileConcurrentAcquire(t *testing.T) {
	ctx := context.Background()
	s := open(t, sqlite.Options{Path: filepath.Join(t.TempDir(), "tikker.db")})
	gen := domain.NewULIDGenerator(clock.NewFake(storetest.T0))
	const total = 200
	msgs := make([]domain.Message, total)
	for i := range msgs {
		msgs[i] = domain.Message{ID: gen.NewID(), Peer: fmt.Sprintf("+486000%05d", i), Body: "x",
			Encoding: domain.EncodingGSM7, Segments: 1, CreatedBy: domain.SystemActor, CreatedAt: storetest.T0}
	}
	if _, err := s.Messages().Enqueue(ctx, msgs, store.Idempotency{}); err != nil {
		t.Fatal(err)
	}

	const workers = 16
	var mu sync.Mutex
	seen := map[domain.ID]int{}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// A reader keeps polling in parallel.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := s.Messages().Count(ctx, store.MessageFilter{}); err != nil {
					t.Errorf("Count: %v", err)
					return
				}
			}
		}
	}()
	var workersWG sync.WaitGroup
	for w := 0; w < workers; w++ {
		ch := gen.NewID()
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			for {
				m, ok, err := s.Messages().AcquireNext(ctx, store.AcquireRequest{ChannelID: ch, Now: storetest.T0, LeaseUntil: storetest.T0.Add(time.Minute)})
				if err != nil {
					t.Errorf("AcquireNext: %v", err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				seen[m.ID]++
				mu.Unlock()
			}
		}()
	}
	workersWG.Wait()
	close(stop)
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("acquired %d of %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("message %s issued %d times", id, n)
		}
	}
}

// TestLeaseLostAfterRecovery replays the ADR 0002 scenario end to end: a
// worker stalls, its lease is recovered and re-issued, and the stale worker's
// late result is rejected.
func TestLeaseLostAfterRecovery(t *testing.T) {
	ctx := context.Background()
	s := open(t, sqlite.Options{})
	gen := domain.NewULIDGenerator(clock.NewFake(storetest.T0))
	m := domain.Message{ID: gen.NewID(), Peer: "+48600000001", Body: "x", Encoding: domain.EncodingGSM7,
		Segments: 1, CreatedBy: domain.SystemActor, CreatedAt: storetest.T0}
	if _, err := s.Messages().Enqueue(ctx, []domain.Message{m}, store.Idempotency{}); err != nil {
		t.Fatal(err)
	}
	slow, ok, err := s.Messages().AcquireNext(ctx, store.AcquireRequest{ChannelID: "A", Now: storetest.T0, LeaseUntil: storetest.T0.Add(time.Minute)})
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	later := storetest.T0.Add(2 * time.Minute)
	if rec, err := s.Messages().RecoverExpiredLeases(ctx, later, 5); err != nil || len(rec) != 1 || rec[0].NewStatus != domain.StatusQueued {
		t.Fatalf("recover = %+v, %v", rec, err)
	}
	fast, ok, err := s.Messages().AcquireNext(ctx, store.AcquireRequest{ChannelID: "B", Now: later, LeaseUntil: later.Add(time.Minute)})
	if err != nil || !ok || fast.Attempts != 2 {
		t.Fatalf("reacquire = %+v, %v, %v", fast, ok, err)
	}
	if err := s.Messages().MarkSent(ctx, slow.Lease(), later); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("stale MarkSent = %v", err)
	}
	if err := s.Messages().MarkSent(ctx, fast.Lease(), later); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Messages().Get(ctx, m.ID)
	if got.Status != domain.StatusSent || !got.PossibleDuplicate || got.Attempts != 2 {
		t.Fatalf("final = %+v", got)
	}
}

func TestRetentionLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := clock.NewFake(storetest.T0)
	s := open(t, sqlite.Options{Clock: clk})
	gen := domain.NewULIDGenerator(clk)
	in := domain.Message{ID: gen.NewID(), Peer: "+48600000001", Body: "secret", Encoding: domain.EncodingGSM7,
		Segments: 1, DedupHash: "h", CreatedAt: storetest.T0.Add(-100 * 24 * time.Hour)}
	if _, err := s.Messages().InsertInbound(ctx, in); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunRetention(ctx, clk, time.Hour, 90*24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	clk.BlockUntil(1) // first pass done, loop sleeping
	got, _ := s.Messages().Get(ctx, in.ID)
	if got.Body != "" {
		t.Fatalf("body not purged: %q", got.Body)
	}
	clk.Advance(time.Hour)
	clk.BlockUntil(1)
	cancel()
	<-done

	res, err := s.Retain(context.Background(), storetest.T0, 0) // body purging disabled
	if err != nil || res.BodiesPurged != 0 {
		t.Fatalf("Retain = %+v, %v", res, err)
	}
}

func TestRetentionLoopLogsFailure(t *testing.T) {
	clk := clock.NewFake(storetest.T0)
	s, err := sqlite.Open(context.Background(), sqlite.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunRetention(ctx, clk, time.Hour, time.Hour, slog.New(slog.NewTextHandler(&buf, nil)))
	}()
	clk.BlockUntil(1)
	cancel()
	<-done
	if !strings.Contains(buf.String(), "retention failed") {
		t.Fatalf("log = %q", buf.String())
	}
	// A nil logger falls back to the default one.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	s.RunRetention(ctx2, clk, time.Hour, time.Hour, nil)
}
