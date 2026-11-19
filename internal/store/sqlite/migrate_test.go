// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/m4drak/tikker/internal/clock"
)

func TestOpenMigratesAndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tikker.db")
	clk := clock.NewFake(time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC))

	s, err := Open(ctx, Options{Path: path, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	var name, sum string
	var at int64
	if err := s.writeDB.QueryRow(`SELECT COUNT(*), MIN(name), MIN(checksum), MIN(applied_at) FROM schema_migrations`).
		Scan(&n, &name, &sum, &at); err != nil {
		t.Fatal(err)
	}
	want, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want) || name != "init" || sum != want[0].checksum || at != clk.Now().UnixMilli() {
		t.Fatalf("schema_migrations = %d %q %q %d", n, name, sum, at)
	}
	for _, table := range []string{"users", "api_tokens", "devices", "channels", "messages", "idempotency_keys", "audit_log"} {
		var c int
		if err := s.writeDB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&c); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Second start: nothing to apply, applied_at untouched.
	clk.Advance(time.Hour)
	s, err = Open(ctx, Options{Path: path, Clock: clk})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var at2 int64
	if err := s.writeDB.QueryRow(`SELECT MIN(applied_at) FROM schema_migrations`).Scan(&at2); err != nil || at2 != at {
		t.Fatalf("applied_at changed: %d -> %d (%v)", at, at2, err)
	}
	// Tamper: a changed checksum must be refused on the next start.
	if _, err := s.writeDB.Exec(`UPDATE schema_migrations SET checksum = 'deadbeef' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, Options{Path: path, Clock: clk}); !errors.Is(err, ErrMigrationChecksum) {
		t.Fatalf("modified migration: err = %v, want ErrMigrationChecksum", err)
	}

	// A database from a newer binary is refused too.
	path2 := filepath.Join(t.TempDir(), "newer.db")
	s, err = Open(ctx, Options{Path: path2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`INSERT INTO schema_migrations VALUES (99, 'future', 'x', 0)`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(ctx, Options{Path: path2}); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("newer schema: err = %v, want ErrSchemaTooNew", err)
	}
}

func memDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestApplyMigrationsIncremental(t *testing.T) {
	ctx := context.Background()
	db := memDB(t)
	clk := clock.NewFake(time.Unix(1000, 0))
	fsys := fstest.MapFS{
		"m/0001_a.sql": {Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"m/0002_b.sql": {Data: []byte(`CREATE TABLE b (x INTEGER); INSERT INTO b VALUES (1);`)},
	}
	ms, err := loadMigrations(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, db, clk, ms[:1]); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, db, clk, ms); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, db, clk, ms); err != nil { // idempotent
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("applied = %d, %v", n, err)
	}

	// A failing migration rolls back completely, including its bookkeeping row,
	// and leaves earlier ones in place.
	bad := append([]migration{}, ms...)
	bad = append(bad, migration{version: 3, name: "bad", checksum: "x",
		sql: `CREATE TABLE c (x INTEGER); INSERT INTO nonexistent VALUES (1);`})
	if err := applyMigrations(ctx, db, clk, bad); err == nil {
		t.Fatal("broken migration accepted")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("applied = %d, %v", n, err)
	}
	if _, err := db.Exec(`SELECT * FROM c`); err == nil {
		t.Fatal("table of the failed migration survived")
	}
}

func TestLoadMigrationsValidation(t *testing.T) {
	tests := []struct {
		name  string
		files fstest.MapFS
		ok    bool
	}{
		{"valid", fstest.MapFS{"m/0001_a.sql": {Data: []byte("x")}, "m/0002_b.sql": {Data: []byte("y")}}, true},
		{"bad name", fstest.MapFS{"m/init.sql": {Data: []byte("x")}}, false},
		{"gap", fstest.MapFS{"m/0001_a.sql": {Data: []byte("x")}, "m/0003_c.sql": {Data: []byte("y")}}, false},
		{"starts at 2", fstest.MapFS{"m/0002_a.sql": {Data: []byte("x")}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ms, err := loadMigrations(tc.files, "m")
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, ok = %v", err, tc.ok)
			}
			if tc.ok && (len(ms) != 2 || ms[0].version != 1 || ms[0].checksum == ms[1].checksum) {
				t.Fatalf("ms = %+v", ms)
			}
		})
	}
	if _, err := loadMigrations(fstest.MapFS{}, "missing"); err == nil {
		t.Error("missing directory accepted")
	}
}

func TestEmbeddedMigrationsAreValid(t *testing.T) {
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil || len(ms) == 0 {
		t.Fatalf("embedded migrations: %v (%d)", err, len(ms))
	}
}

func TestHelpers(t *testing.T) {
	if !fromMS(0).IsZero() || toMS(time.Time{}) != 0 {
		t.Error("zero time must round-trip as 0")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	if got := trunc(now); got.Nanosecond() != 123000000 || got.Location() != time.UTC {
		t.Errorf("trunc = %v", got)
	}
	if _, err := normalizeSynchronous(" extra "); err != nil {
		t.Error(err)
	}
	if got, _ := normalizeSynchronous(""); got != "NORMAL" {
		t.Errorf("default synchronous = %q", got)
	}
	if _, err := parseTags("not json"); err == nil {
		t.Error("corrupt tags accepted")
	}
	if _, err := parseActor("bogus"); err == nil {
		t.Error("corrupt actor accepted")
	}
	if tags, err := parseTags("[]"); err != nil || tags != nil {
		t.Errorf("parseTags([]) = %v, %v", tags, err)
	}
	if mapErr(nil) != nil {
		t.Error("mapErr(nil)")
	}
	plain := errors.New("plain")
	if mapErr(plain) != plain {
		t.Error("mapErr must pass unknown errors through")
	}
}
