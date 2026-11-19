// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

// Package sqlite implements the persistence contract of internal/store on
// SQLite, using the pure-Go driver modernc.org/sqlite (no CGO).
//
// # Connections
//
// A file database is opened as two pools. The write pool holds exactly one
// connection (SetMaxOpenConns(1)), so all writes are serialised inside the
// process and can never meet SQLITE_BUSY from a sibling connection; its
// transactions start with BEGIN IMMEDIATE. The read pool (a few connections,
// each with PRAGMA query_only) serves reads in parallel under WAL and never
// blocks on, or blocks, the writer. An in-memory database lives in a single
// connection, so there both pools are the same one-connection pool.
//
// # Time, IDs and tags
//
// Times are INTEGER unix milliseconds in UTC, 0 meaning "not set" (compact,
// index friendly, comparable in SQL, no timezone parsing; sub-millisecond
// precision is dropped). IDs are ULIDs in TEXT columns, ” meaning "none".
// Tag lists are JSON arrays; "all route tags are among the channel's tags"
// is evaluated in SQL with json_each, which keeps AcquireNext a single
// statement without a join table.
//
// # Flash wear
//
// Reads and empty polls never write: AcquireNext checks eligibility on the
// read pool first and only then runs its UPDATE, and no-op statements leave
// the database file untouched. The synchronous level is configurable.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	// Pure-Go SQLite driver, registered as "sqlite".
	_ "modernc.org/sqlite"

	"github.com/m4drak/tikker/internal/clock"
	"github.com/m4drak/tikker/internal/store"
)

// Defaults for [Options].
const (
	DefaultBusyTimeout = 5 * time.Second
	DefaultReadConns   = 4
	// DefaultHealthRefresh is how long UpdateHealth may skip a write whose
	// only change is the timestamp.
	DefaultHealthRefresh = 5 * time.Minute
	// DefaultTouchInterval is the coalescing window of TouchLastUsed.
	DefaultTouchInterval = time.Minute
	// IdempotencyTTL is how long idempotency records are kept.
	IdempotencyTTL = 24 * time.Hour

	memoryPath = ":memory:"
)

// Options configures [Open].
type Options struct {
	// Path of the database file. Empty or ":memory:" selects a private
	// in-memory database (tests).
	Path string
	// Synchronous is the PRAGMA synchronous level: OFF, NORMAL, FULL or
	// EXTRA. Empty means NORMAL, which is safe under WAL against
	// application crashes (a power cut may lose the last transactions but
	// never corrupts the database); use FULL for durability across power
	// loss at the cost of more flash writes.
	Synchronous string
	// BusyTimeout is how long a connection waits for a lock held by another
	// process. Zero means DefaultBusyTimeout.
	BusyTimeout time.Duration
	// ReadConns is the size of the read pool; zero means DefaultReadConns.
	ReadConns int
	// Clock stamps schema_migrations rows and decides "recent" for
	// UpdateHealth and TouchLastUsed. Nil means the system clock.
	Clock clock.Clock
	// HealthRefresh and TouchInterval override the defaults when non-zero.
	HealthRefresh time.Duration
	TouchInterval time.Duration
}

// Store is the SQLite implementation of [store.Store].
type Store struct {
	*handle
	writeDB *sql.DB
	readDB  *sql.DB
	clk     clock.Clock
	opts    Options
	memory  bool
}

var _ store.Store = (*Store)(nil)

// Open opens (creating it if needed) and migrates the database.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Clock == nil {
		opts.Clock = clock.System()
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = DefaultBusyTimeout
	}
	if opts.ReadConns <= 0 {
		opts.ReadConns = DefaultReadConns
	}
	if opts.HealthRefresh <= 0 {
		opts.HealthRefresh = DefaultHealthRefresh
	}
	if opts.TouchInterval <= 0 {
		opts.TouchInterval = DefaultTouchInterval
	}
	sync, err := normalizeSynchronous(opts.Synchronous)
	if err != nil {
		return nil, err
	}
	opts.Synchronous = sync

	s := &Store{clk: opts.Clock, opts: opts}
	s.memory = opts.Path == "" || opts.Path == memoryPath

	s.writeDB, err = sql.Open("sqlite", dsn(opts, true))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	s.writeDB.SetMaxOpenConns(1)
	s.writeDB.SetMaxIdleConns(1)
	s.writeDB.SetConnMaxLifetime(0)
	s.writeDB.SetConnMaxIdleTime(0)
	if err := s.writeDB.PingContext(ctx); err != nil {
		_ = s.writeDB.Close()
		return nil, fmt.Errorf("sqlite: open %q: %w", opts.Path, err)
	}
	if err := migrate(ctx, s.writeDB, s.clk); err != nil {
		_ = s.writeDB.Close()
		return nil, err
	}

	if s.memory {
		s.readDB = s.writeDB
	} else {
		s.readDB, err = sql.Open("sqlite", dsn(opts, false))
		if err != nil {
			_ = s.writeDB.Close()
			return nil, fmt.Errorf("sqlite: open reader: %w", err)
		}
		s.readDB.SetMaxOpenConns(opts.ReadConns)
		s.readDB.SetMaxIdleConns(opts.ReadConns)
		s.readDB.SetConnMaxLifetime(0)
		s.readDB.SetConnMaxIdleTime(0)
		if err := s.readDB.PingContext(ctx); err != nil {
			_ = s.readDB.Close()
			_ = s.writeDB.Close()
			return nil, fmt.Errorf("sqlite: open reader: %w", err)
		}
	}
	s.handle = &handle{s: s, r: s.readDB, w: s.writeDB}
	return s, nil
}

func normalizeSynchronous(v string) (string, error) {
	v = strings.ToUpper(strings.TrimSpace(v))
	switch v {
	case "":
		return "NORMAL", nil
	case "OFF", "NORMAL", "FULL", "EXTRA":
		return v, nil
	}
	return "", fmt.Errorf("sqlite: invalid synchronous level %q (want OFF, NORMAL, FULL or EXTRA)", v)
}

// dsn builds the connection string with per-connection pragmas.
func dsn(o Options, writer bool) string {
	q := url.Values{}
	add := func(p string) { q.Add("_pragma", p) }
	add(fmt.Sprintf("busy_timeout(%d)", o.BusyTimeout.Milliseconds()))
	if writer {
		add("journal_mode(WAL)")
		add("foreign_keys(1)")
		add("synchronous(" + o.Synchronous + ")")
		add("journal_size_limit(4194304)")
		q.Set("_txlock", "immediate")
	} else {
		add("query_only(1)")
	}
	var name string
	if o.Path == "" || o.Path == memoryPath {
		name = ":memory:"
	} else {
		r := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")
		name = r.Replace(o.Path)
	}
	// url.Values.Encode escapes parentheses, which the driver unescapes.
	return "file:" + name + "?" + q.Encode()
}

// Ping implements [store.Store].
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.readDB.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}
	return nil
}

// Close implements [store.Store].
func (s *Store) Close() error {
	var errs []error
	if !s.memory {
		errs = append(errs, s.readDB.Close())
	}
	errs = append(errs, s.writeDB.Close())
	return errors.Join(errs...)
}

// WithTx implements [store.Store].
func (s *Store) WithTx(ctx context.Context, fn func(tx store.Repos) error) (err error) {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(&handle{s: s, r: tx, w: tx, tx: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	return nil
}
