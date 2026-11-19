// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/m4drak/tikker/internal/domain"
	"github.com/m4drak/tikker/internal/store"
)

type sessionRepo struct{ h *handle }

var _ store.SessionRepo = (*sessionRepo)(nil)

// Sessions implements [store.Repos].
func (h *handle) Sessions() store.SessionRepo { return &sessionRepo{h} }

const sessionCols = `id, user_id, created_at, last_seen_at, expires_at, user_agent, ip`

// errSessionNotFound keeps the session ID (a hash of a secret) out of error
// messages.
var errSessionNotFound = fmt.Errorf("%w: session", domain.ErrNotFound)

func scanSession(sc scanner) (store.Session, error) {
	var s store.Session
	var user string
	var created, seen, expires int64
	if err := sc.Scan(&s.ID, &user, &created, &seen, &expires, &s.UserAgent, &s.IP); err != nil {
		return store.Session{}, err
	}
	s.UserID = domain.ID(user)
	s.CreatedAt, s.LastSeenAt, s.ExpiresAt = fromMS(created), fromMS(seen), fromMS(expires)
	return s, nil
}

// Create implements [store.SessionRepo].
func (r *sessionRepo) Create(ctx context.Context, s store.Session) error {
	if s.ID == "" || s.UserID.IsZero() {
		return errors.New("sqlite: session needs ID and UserID")
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = r.h.s.clk.Now()
	}
	if s.LastSeenAt.IsZero() {
		s.LastSeenAt = s.CreatedAt
	}
	return r.h.write(ctx, func(q execer) error {
		_, err := q.ExecContext(ctx,
			`INSERT INTO sessions (`+sessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			s.ID, string(s.UserID), toMS(s.CreatedAt), toMS(s.LastSeenAt), toMS(s.ExpiresAt), s.UserAgent, s.IP)
		if err != nil {
			return fmt.Errorf("sqlite: create session: %w", mapErr(err))
		}
		return nil
	})
}

// Get implements [store.SessionRepo].
func (r *sessionRepo) Get(ctx context.Context, id string) (store.Session, error) {
	s, err := scanSession(r.h.r.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, errSessionNotFound
	}
	if err != nil {
		return store.Session{}, fmt.Errorf("sqlite: get session: %w", err)
	}
	return s, nil
}

// List implements [store.SessionRepo].
func (r *sessionRepo) List(ctx context.Context, userID domain.ID) ([]store.Session, error) {
	rows, err := r.h.r.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE user_id = ? ORDER BY created_at DESC, id`, string(userID))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list sessions: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list sessions: %w", err)
	}
	return out, nil
}

// Touch implements [store.SessionRepo].
func (r *sessionRepo) Touch(ctx context.Context, id string, at time.Time) error {
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, toMS(at), id)
		if err != nil {
			return fmt.Errorf("sqlite: touch session: %w", err)
		}
		return affectedOne(res)
	})
}

// Delete implements [store.SessionRepo].
func (r *sessionRepo) Delete(ctx context.Context, id string) error {
	return r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("sqlite: delete session: %w", err)
		}
		return affectedOne(res)
	})
}

func affectedOne(res sql.Result) error {
	n, err := rowsAffected(res)
	if err != nil {
		return err
	}
	if n == 0 {
		return errSessionNotFound
	}
	return nil
}

// DeleteByUser implements [store.SessionRepo].
func (r *sessionRepo) DeleteByUser(ctx context.Context, userID domain.ID) (int, error) {
	return r.deleteWhere(ctx, `user_id = ?`, string(userID))
}

// DeleteExpired implements [store.SessionRepo].
func (r *sessionRepo) DeleteExpired(ctx context.Context, now, idleBefore time.Time) (int, error) {
	return r.deleteWhere(ctx, `expires_at <= ? OR last_seen_at <= ?`, toMS(now), toMS(idleBefore))
}

func (r *sessionRepo) deleteWhere(ctx context.Context, where string, args ...any) (int, error) {
	var n int64
	err := r.h.write(ctx, func(q execer) error {
		res, err := q.ExecContext(ctx, `DELETE FROM sessions WHERE `+where, args...)
		if err != nil {
			return fmt.Errorf("sqlite: delete sessions: %w", err)
		}
		n, err = rowsAffected(res)
		return err
	})
	return int(n), err
}
