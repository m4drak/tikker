// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"

	"github.com/m4drak/tikker/internal/clock"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// ErrMigrationChecksum is returned when an already applied migration no
// longer matches the file embedded in the binary.
var ErrMigrationChecksum = errors.New("sqlite: applied migration was modified")

// ErrSchemaTooNew is returned when the database has migrations this binary
// does not know (a newer tikker wrote it).
var ErrSchemaTooNew = errors.New("sqlite: database schema is newer than this binary")

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version  int
	name     string
	checksum string
	sql      string
}

// loadMigrations reads and orders the embedded migrations, rejecting gaps and
// duplicate versions.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read migrations: %w", err)
	}
	var ms []migration
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("sqlite: migration file %q does not match NNNN_name.sql", e.Name())
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("sqlite: read migration %s: %w", e.Name(), err)
		}
		v, _ := strconv.Atoi(m[1])
		sum := sha256.Sum256(body)
		ms = append(ms, migration{version: v, name: m[2], checksum: hex.EncodeToString(sum[:]), sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("sqlite: migrations must be numbered 0001.. without gaps (found %04d at position %d)", m.version, i+1)
		}
	}
	return ms, nil
}

// migrate brings the database to the latest schema. Applied migrations are
// verified by checksum; new ones run each in its own transaction, in order.
// When everything is up to date nothing is written.
func migrate(ctx context.Context, db *sql.DB, clk clock.Clock) error {
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	return applyMigrations(ctx, db, clk, ms)
}

func applyMigrations(ctx context.Context, db *sql.DB, clk clock.Clock, ms []migration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER NOT NULL PRIMARY KEY,
		name       TEXT    NOT NULL,
		checksum   TEXT    NOT NULL,
		applied_at INTEGER NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("sqlite: create schema_migrations: %w", err)
	}

	applied := map[int]string{} // version -> checksum
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("sqlite: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			_ = rows.Close()
			return fmt.Errorf("sqlite: read schema_migrations: %w", err)
		}
		applied[v] = sum
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("sqlite: read schema_migrations: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: read schema_migrations: %w", err)
	}

	known := map[int]bool{}
	for _, m := range ms {
		known[m.version] = true
		if sum, ok := applied[m.version]; ok && sum != m.checksum {
			return fmt.Errorf("%w: %04d_%s", ErrMigrationChecksum, m.version, m.name)
		}
	}
	for v := range applied {
		if !known[v] {
			return fmt.Errorf("%w: unknown migration %04d", ErrSchemaTooNew, v)
		}
	}

	for _, m := range ms {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := applyOne(ctx, db, clk, m); err != nil {
			return fmt.Errorf("sqlite: migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

func applyOne(ctx context.Context, db *sql.DB, clk clock.Clock, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		m.version, m.name, m.checksum, toMS(clk.Now())); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
