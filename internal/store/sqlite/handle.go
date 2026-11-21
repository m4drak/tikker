// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlitedrv "modernc.org/sqlite"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

// execer is the subset of *sql.DB and *sql.Tx the repositories use.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// handle gives repositories their database access. Outside a transaction r is
// the read pool and w the write pool; inside [Store.WithTx] both are the
// transaction and tx is set.
type handle struct {
	s  *Store
	r  execer
	w  execer
	tx *sql.Tx
}

var _ store.Repos = (*handle)(nil)

// Messages implements [store.Repos].
func (h *handle) Messages() store.MessageRepo { return &messageRepo{h} }

// Devices implements [store.Repos].
func (h *handle) Devices() store.DeviceRepo { return &deviceRepo{h} }

// Channels implements [store.Repos].
func (h *handle) Channels() store.ChannelRepo { return &channelRepo{h} }

// Users implements [store.Repos].
func (h *handle) Users() store.UserRepo { return &userRepo{h} }

// Tokens implements [store.Repos].
func (h *handle) Tokens() store.TokenRepo { return &tokenRepo{h} }

// Audit implements [store.Repos].
func (h *handle) Audit() store.AuditRepo { return &auditRepo{h} }

// write runs fn atomically on the write connection: inside the caller's
// transaction when there is one, otherwise in a fresh immediate transaction.
func (h *handle) write(ctx context.Context, fn func(q execer) error) error {
	if h.tx != nil {
		return fn(h.tx)
	}
	tx, err := h.s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Value conversion

// toMS converts t to unix milliseconds; the zero time is 0.
func toMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromMS is the inverse of toMS.
func fromMS(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// trunc rounds t to what the database stores.
func trunc(t time.Time) time.Time { return fromMS(toMS(t)) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil { // only strings and string maps are marshalled
		panic(fmt.Sprintf("sqlite: marshal: %v", err))
	}
	return string(b)
}

// tagsJSON encodes a tag list; nil becomes "[]".
func tagsJSON(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	return marshalJSON(tags)
}

func parseTags(s string) ([]string, error) {
	var tags []string
	if err := json.Unmarshal([]byte(s), &tags); err != nil {
		return nil, fmt.Errorf("sqlite: corrupt tags %q: %w", s, err)
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

func parseActor(s string) (domain.Actor, error) {
	a, err := domain.ParseActor(s)
	if err != nil {
		return domain.Actor{}, fmt.Errorf("sqlite: corrupt actor %q: %w", s, err)
	}
	return a, nil
}

// nullInt maps a nullable signal column to *int.
func nullInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func intArg(p *int) any {
	if p == nil {
		return nil
	}
	return int64(*p)
}

// SQLite result codes (primary and extended) used to classify errors.
const (
	codeConstraint       = 19
	codeConstraintUnique = 2067
	codeConstraintPKey   = 1555
	codeConstraintFKey   = 787
)

// mapErr translates driver errors to the domain errors of the store
// contract: UNIQUE/PRIMARY KEY violations become domain.ErrConflict and
// foreign key violations domain.ErrNotFound (the referenced row is missing).
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlitedrv.Error
	if errors.As(err, &se) && se.Code()&0xff == codeConstraint {
		switch se.Code() {
		case codeConstraintUnique, codeConstraintPKey:
			return fmt.Errorf("%w: %s", domain.ErrConflict, se.Error())
		case codeConstraintFKey:
			return fmt.Errorf("%w: referenced row does not exist", domain.ErrNotFound)
		}
	}
	return err
}

// notFound wraps domain.ErrNotFound for entity kind and id.
func notFound(kind string, id any) error {
	return fmt.Errorf("%w: %s %v", domain.ErrNotFound, kind, id)
}

// rowsAffected returns the number of rows a statement changed.
func rowsAffected(res sql.Result) (int64, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: rows affected: %w", err)
	}
	return n, nil
}

// placeholders returns "?,?,?" for n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// pageCursor validates a page cursor, which is the ID of the last item of the
// previous page.
func pageCursor(c string) (string, error) {
	if c == "" {
		return "", nil
	}
	if _, err := domain.ParseID(c); err != nil {
		return "", fmt.Errorf("invalid page cursor: %w", err)
	}
	return c, nil
}

// paginate trims rows (fetched with limit+1) into a page and sets NextCursor
// to the ID of the last returned item when more rows exist.
func paginate[T any](items []T, limit int, id func(T) domain.ID) store.Page[T] {
	if len(items) > limit {
		items = items[:limit]
		return store.Page[T]{Items: items, NextCursor: string(id(items[limit-1]))}
	}
	return store.Page[T]{Items: items}
}
